// Package downstream は、下流の API（本PoCの provisioning-api と、aka-vecgen の管理API）を呼ぶ共通のクライアント。
// 下流の 2 つは作法を揃えてある（パス /admin/v1、JSON、ProblemDetails、mTLS とフィンガープリントの固定、
// X-Operator-Id、X-Trace-ID）ので、接続と呼び出しの仕組みはここにまとめ、型つきの呼び出しは
// internal/provapi と internal/akaapi に置く。
//
// 接続は mTLS で、provisioner のクライアント証明書を提示し、下流のサーバー証明書（自己署名）を
// 信頼する証明書として検証する（ホスト名も検証する）。
//
// 操作者（WithOperator）がコンテキストに入っていれば X-Operator-Id ヘッダーで、
// トレースID（internal/trace）が入っていれば X-Trace-ID ヘッダーで渡す。トレースID が入っていなければ呼び出しごとに採番する。
// provisioner は管理クライアントから受け取った値をそのまま渡すだけで、操作者の有無は確かめない（下流と同じく任意）。
package downstream

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/trace"
)

const (
	defaultTimeout = 5 * time.Second
	// maxResponseBytes は応答ボディを読む上限。一覧の最大（500 件）でも収まる大きさにする。
	maxResponseBytes = 16 << 20
	operatorHeader   = "X-Operator-Id"
)

// Name は下流の API の名前。ProblemDetails の downstream や、ログに使う。
type Name string

const (
	// Prov は本PoCの Provisioning API（provisioning-api）。
	Prov Name = "prov"
	// Aka は aka-vecgen の管理API。
	Aka Name = "aka"
)

// Options はクライアントの設定。
type Options struct {
	Name Name
	// BaseURL は下流の API のベース URL（例: https://provisioning-api:9444/admin/v1）。
	BaseURL string
	// ClientCertFile は provisioner のクライアント証明書（PEM）。
	ClientCertFile string
	// ClientKeyFile はクライアント証明書の秘密鍵（PEM）。空なら ClientCertFile から読む
	// （gen-client-cert は、出力先を分けなければ証明書と秘密鍵を 1 つの PEM にまとめて出力する）。
	ClientKeyFile string
	// ServerCertFile は下流のサーバー証明書（PEM）。これを信頼する証明書として検証する。
	ServerCertFile string
	// Timeout は 1 回の呼び出しの上限時間。0 なら 5 秒。
	Timeout time.Duration
	Log     *slog.Logger
}

// Client は下流の API のクライアント。
type Client struct {
	name Name
	base *url.URL
	hc   *http.Client
	log  *slog.Logger
	// clientCert は提示するクライアント証明書。フィンガープリントの表示に使う。
	clientCert *x509.Certificate
}

// New はクライアントを作る。証明書のファイルはここで読み込む。
func New(opts Options) (*Client, error) {
	base, err := url.Parse(opts.BaseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return nil, fmt.Errorf("%s url %q: want https://host:port/path", opts.Name, opts.BaseURL)
	}
	cert, err := tls.LoadX509KeyPair(opts.ClientCertFile, cmp.Or(opts.ClientKeyFile, opts.ClientCertFile))
	if err != nil {
		return nil, fmt.Errorf("load client certificate: %w", err)
	}
	serverPEM, err := os.ReadFile(opts.ServerCertFile)
	if err != nil {
		return nil, fmt.Errorf("load %s server certificate: %w", opts.Name, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(serverPEM) {
		return nil, fmt.Errorf("load %s server certificate: no PEM certificate in %s", opts.Name, opts.ServerCertFile)
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			RootCAs:      roots,
		},
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 8,
	}
	return &Client{
		name: opts.Name,
		base: base,
		hc: &http.Client{
			Transport: tr,
			Timeout:   cmp.Or(opts.Timeout, defaultTimeout),
			// 下流はリダイレクトを返さない。返ってきたらそのまま応答として扱う。
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		log:        opts.Log.With("downstream", string(opts.Name)),
		clientCert: cert.Leaf,
	}, nil
}

// Name は下流の名前を返す。
func (c *Client) Name() Name { return c.name }

// BaseURL は下流のベース URL を返す。
func (c *Client) BaseURL() string { return c.base.String() }

// ClientCertificate は下流に提示するクライアント証明書を返す。
func (c *Client) ClientCertificate() *x509.Certificate { return c.clientCert }

// ---- 操作者 ----

type operatorKey struct{}

// WithOperator は、下流に渡す操作者（X-Operator-Id）をコンテキストに入れる。
func WithOperator(ctx context.Context, operator string) context.Context {
	return context.WithValue(ctx, operatorKey{}, operator)
}

// OperatorFrom はコンテキストに入れた操作者を返す。入っていなければ空文字列。
func OperatorFrom(ctx context.Context) string {
	id, _ := ctx.Value(operatorKey{}).(string)
	return id
}

// ---- エラー ----

// InvalidParam は不正だった項目。
type InvalidParam struct {
	// Param は項目の名前（例: ki、rules[0].allowedSsids[1]）。
	Param  string `json:"param"`
	Reason string `json:"reason,omitempty"`
}

// Problem は下流のエラー応答（ProblemDetails）。
type Problem struct {
	Title         string         `json:"title,omitempty"`
	Status        int            `json:"status,omitempty"`
	Detail        string         `json:"detail,omitempty"`
	Cause         string         `json:"cause,omitempty"`
	InvalidParams []InvalidParam `json:"invalidParams,omitempty"`
}

// Error は下流がエラー（2xx 以外）を返したことを表す。
// 応答が ProblemDetails でなかった場合、Problem は空になる。
type Error struct {
	Downstream Name
	Method     string
	Path       string
	Status     int
	// TraceID は下流が応答の X-Trace-ID で返したトレースID（返さなければ送った値）。
	TraceID string
	Problem Problem
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s %s %s: %d %s", e.Downstream, e.Method, e.Path, e.Status, http.StatusText(e.Status))
	if e.Problem.Cause != "" {
		msg += " (" + e.Problem.Cause + ")"
	}
	if e.Problem.Detail != "" {
		msg += ": " + e.Problem.Detail
	}
	return msg
}

// CauseOf は、err が下流のエラー応答なら cause を返す。そうでなければ空文字列。
func CauseOf(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Problem.Cause
	}
	return ""
}

// IsUnavailable は、err が下流に届かなかった（接続・TLS・タイムアウトなど）ことを表すかを返す。
// 下流が応答を返した場合は false。
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	_, isAPIError := errors.AsType[*Error](err)
	return !isAPIError
}

// ---- 呼び出し ----

// Request は 1 回の呼び出しの内容。
type Request struct {
	Method string
	// Path は BaseURL からの相対パスの要素。各要素は 1 つのセグメントとしてエスケープして連結する。
	Path  []string
	Query url.Values
	// Body は JSON にして送る値。nil なら送らない。
	Body        any
	ContentType string
}

// Call は下流を呼び出し、2xx の応答ボディを T として読む。T が struct{} ならボディは読まない。
func (c *Client) Call[T any](ctx context.Context, req Request) (T, error) {
	var out T
	resp, err := c.Send(ctx, req)
	if err != nil {
		return out, err
	}
	defer drainClose(resp.Body)

	if _, noBody := any(out).(struct{}); noBody {
		return out, nil
	}
	if err := json.UnmarshalRead(io.LimitReader(resp.Body, maxResponseBytes), &out); err != nil {
		return out, fmt.Errorf("%s %s %s: decode response: %w", c.name, req.Method, resp.Request.URL.Path, err)
	}
	return out, nil
}

// Send はリクエストを送り、2xx の応答を返す（呼び出し側でボディを閉じる）。それ以外の応答は *Error にして返す。
func (c *Client) Send(ctx context.Context, req Request) (*http.Response, error) {
	var body []byte
	var contentType string
	if req.Body != nil {
		b, err := json.Marshal(req.Body)
		if err != nil {
			return nil, fmt.Errorf("%s %s %s: encode request: %w", c.name, req.Method, strings.Join(req.Path, "/"), err)
		}
		body, contentType = b, cmp.Or(req.ContentType, "application/json")
	}
	resp, traceID, err := c.do(ctx, req.Method, req.Path, req.Query.Encode(), body, contentType)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer drainClose(resp.Body)
	apiErr := &Error{Downstream: c.name, Method: req.Method, Path: resp.Request.URL.Path, Status: resp.StatusCode, TraceID: traceID}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "application/problem+json" {
		// 読めなくても、ステータスコードだけで扱えるようにする。
		_ = json.UnmarshalRead(io.LimitReader(resp.Body, maxResponseBytes), &apiErr.Problem)
	}
	return nil, apiErr
}

// RelayRequest は中継する要求。本文は検証せずにそのまま送る。
type RelayRequest struct {
	Method string
	// Path は BaseURL からの相対パスの要素。各要素は 1 つのセグメントとしてエスケープして連結する。
	Path []string
	// RawQuery はクエリ文字列（エンコード済み）。そのまま送る。
	RawQuery string
	// Body は要求の本文。nil なら送らない。
	Body []byte
	// ContentType は Body の Content-Type。
	ContentType string
}

// RelayResponse は中継する応答。
type RelayResponse struct {
	Status int
	// ContentType と Location は下流の応答のヘッダー（なければ空文字列）。
	ContentType string
	Location    string
	Body        []byte
}

// Relay は要求をそのまま下流に送り、応答（2xx 以外を含む）をそのまま返す。
// エラーを返すのは、下流に届かなかった場合と、応答を読めなかった場合だけ。
func (c *Client) Relay(ctx context.Context, req RelayRequest) (RelayResponse, error) {
	resp, _, err := c.do(ctx, req.Method, req.Path, req.RawQuery, req.Body, req.ContentType)
	if err != nil {
		return RelayResponse{}, err
	}
	defer drainClose(resp.Body)
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return RelayResponse{}, fmt.Errorf("%s %s %s: read response: %w", c.name, req.Method, resp.Request.URL.Path, err)
	}
	return RelayResponse{
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Location:    resp.Header.Get("Location"),
		Body:        body,
	}, nil
}

// do はリクエストを送り、応答（ステータスによらない）と、下流が返したトレースID を返す。
// 操作者とトレースID はコンテキストから取る。
func (c *Client) do(ctx context.Context, method string, path []string, rawQuery string, body []byte, contentType string) (*http.Response, string, error) {
	op := OperatorFrom(ctx)
	traceID := cmp.Or(trace.From(ctx), trace.New())

	u, err := c.url(path)
	if err != nil {
		return nil, "", fmt.Errorf("%s %s: %w", c.name, method, err)
	}
	u.RawQuery = rawQuery

	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	hreq, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, "", err
	}
	hreq.Header.Set("Accept", "application/json, application/problem+json")
	if contentType != "" {
		hreq.Header.Set("Content-Type", contentType)
	}
	if op != "" {
		hreq.Header.Set(operatorHeader, op)
	}
	hreq.Header.Set(trace.Header, traceID)

	start := time.Now()
	resp, err := c.hc.Do(hreq)
	if err != nil {
		c.log.Warn("downstream call failed", "method", method, "path", u.Path, "trace_id", traceID, "error", err)
		return nil, "", fmt.Errorf("%s %s %s: %w", c.name, method, u.Path, err)
	}
	// 下流は使ったトレースID を応答で返す（送った値と同じになるはず）。
	traceID = cmp.Or(resp.Header.Get(trace.Header), traceID)
	// リクエストとレスポンスのボディ、クエリ文字列は出さない（Ki / OPc や検索条件を含みうるため）。
	c.log.Debug("downstream call", "method", method, "path", u.Path, "status", resp.StatusCode,
		"duration_ms", time.Since(start).Milliseconds(), "operator", op, "trace_id", traceID)
	return resp, traceID, nil
}

// drainClose は、接続を再利用できるよう残りのボディを読み捨ててから閉じる。
func drainClose(body io.ReadCloser) {
	io.Copy(io.Discard, io.LimitReader(body, maxResponseBytes))
	body.Close()
}

// url は BaseURL に path の要素を連結した URL を返す。各要素は 1 つのセグメントとしてエスケープする。
// url.URL.JoinPath は要素の中の "/" をエスケープしないので使わない（例えば中継で受け取った値 "1/secret" が、
// 別の操作のパス /clients/1/secret として下流に届かないようにする）。空・"."・".." の要素は、
// 下流のルーターがパスを正規化して別のパスとして扱いうるので、送らずにエラーにする。
func (c *Client) url(path []string) (*url.URL, error) {
	u := *c.base
	raw := strings.TrimSuffix(c.base.EscapedPath(), "/")
	for _, p := range path {
		if p == "" || p == "." || p == ".." {
			return nil, fmt.Errorf("invalid path element %q", p)
		}
		raw += "/" + url.PathEscape(p)
	}
	u.RawPath = raw
	u.Path, _ = url.PathUnescape(raw) // PathEscape したものなので失敗しない
	return &u, nil
}

// ---- 一覧の条件 ----

// ListParams は加入者・認可ポリシーの一覧の条件（下流 2 つで同じ）。ゼロ値の項目は指定しない。
type ListParams struct {
	// Prefix は IMSI の前方一致条件。
	Prefix string
	// Cursor は前のページの NextCursor。
	Cursor string
	// Limit は 1 ページの件数（1〜500、省略時は 50）。
	Limit int
}

// Query はクエリパラメーターにする。
func (p ListParams) Query() url.Values {
	q := url.Values{}
	if p.Prefix != "" {
		q.Set("prefix", p.Prefix)
	}
	if p.Cursor != "" {
		q.Set("cursor", p.Cursor)
	}
	if p.Limit != 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	return q
}
