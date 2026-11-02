// Package downstream は、下流（aka-vecgen・aka-radius・radsec-ca・エッジの管理API）への中継のクライアントと、下流の登録簿を扱う（設計概要 §6・§8）。
//
// 接続は mTLS で、provisioner のクライアント証明書を提示する。aka-vecgen・aka-radius・radsec-ca は、下流のサーバー証明書（自己署名）を
// 信頼する証明書として検証する（ホスト名も検証する）。エッジはサーバー証明書のフィンガープリントと照合する（ホスト名は見ない。P3）。
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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/certs"
	"github.com/oyaguma3/aka-provisioner/internal/trace"
)

const (
	defaultTimeout = 5 * time.Second
	// StatusTimeout は下流の /status を確かめるときの上限（P4）。radsec-ca・エッジの 30 秒を待たない。
	StatusTimeout = 10 * time.Second
	// maxResponseBytes は応答ボディを読む上限。一覧・一括取得の最大（500 件）でも収まる大きさにする。
	maxResponseBytes = 16 << 20
	operatorHeader   = "X-Operator-Id"

	idempotencyKeyHeader     = "Idempotency-Key"
	idempotentReplayedHeader = "Idempotent-Replayed"
)

// Name は下流の名前。ProblemDetails の downstream とログに使う。エッジは edge:<ID>（EdgeName）。
type Name string

// 下流の名前。
const (
	Vecgen Name = "vecgen"
	Radius Name = "radius"
	CA     Name = "ca"
)

// edgePrefix はエッジの名前の接頭辞。
const edgePrefix = "edge:"

// EdgeName はエッジの ID から下流の名前を返す。
func EdgeName(id string) Name { return Name(edgePrefix + id) }

// IsEdge はエッジの名前か。
func (n Name) IsEdge() bool { return strings.HasPrefix(string(n), edgePrefix) }

// radsec は radsec-ca かエッジか（時間のかかる処理があるので、タイムアウトが別。P4）。
func (n Name) radsec() bool { return n == CA || n.IsEdge() }

// clientOptions はクライアントを作るときの設定。serverCert か serverFingerprint のどちらかを与える。
type clientOptions struct {
	name              Name
	baseURL           *url.URL
	clientCert        tls.Certificate
	serverCert        *x509.Certificate
	serverFingerprint string
	timeout           time.Duration
	log               *slog.Logger
}

// Client は 1 つの下流への中継のクライアント。
type Client struct {
	name       Name
	base       *url.URL
	hc         *http.Client
	tr         *http.Transport
	log        *slog.Logger
	serverCert *x509.Certificate
	// seenCert はフィンガープリントで照合する下流の、最後の接続で受け取った証明書（/status に出す）。
	seenCert atomic.Pointer[x509.Certificate]
}

func newClient(opts clientOptions) *Client {
	c := &Client{
		name:       opts.name,
		base:       opts.baseURL,
		log:        opts.log.With("downstream", string(opts.name)),
		serverCert: opts.serverCert,
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{opts.clientCert},
	}
	if opts.serverCert != nil {
		roots := x509.NewCertPool()
		roots.AddCert(opts.serverCert)
		cfg.RootCAs = roots
	} else {
		// 標準の検証（チェーンとホスト名）はせず、VerifyConnection でフィンガープリントだけを照合する。
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no server certificate")
			}
			leaf := cs.PeerCertificates[0]
			if got := certs.Fingerprint(leaf); got != opts.serverFingerprint {
				return &FingerprintError{Want: opts.serverFingerprint, Got: got}
			}
			c.seenCert.Store(leaf)
			return nil
		}
	}
	c.tr = &http.Transport{
		TLSClientConfig:     cfg,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 8,
	}
	c.hc = &http.Client{
		Transport: c.tr,
		Timeout:   cmp.Or(opts.timeout, defaultTimeout),
		// 下流はリダイレクトしない。思わぬ所へ要求を送らないよう、リダイレクトには従わない。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c
}

// FingerprintError は、下流のサーバー証明書が登録簿の serverFingerprint と一致しないときの誤り。
type FingerprintError struct {
	Want, Got string
}

func (e *FingerprintError) Error() string {
	return fmt.Sprintf("server certificate fingerprint %s does not match serverFingerprint %s", e.Got, e.Want)
}

// Name は下流の名前を返す。
func (c *Client) Name() Name { return c.name }

// BaseURL は下流の基底 URL を返す。
func (c *Client) BaseURL() string { return c.base.String() }

// ServerCertificate は下流のサーバー証明書を返す。serverCert の下流は信頼の起点として読んだもの、
// serverFingerprint の下流は最後の接続で受け取ったもの（まだ接続していなければ nil）。
func (c *Client) ServerCertificate() *x509.Certificate {
	if c.serverCert != nil {
		return c.serverCert
	}
	return c.seenCert.Load()
}

// closeIdle は、使わなくなったクライアントの待機中の接続を閉じる（登録簿を読み直したとき）。
func (c *Client) closeIdle() { c.tr.CloseIdleConnections() }

// --- 操作者 ---

type operatorKey struct{}

// WithOperator は、下流に X-Operator-Id として渡す操作者をコンテキストに入れる。
func WithOperator(ctx context.Context, operator string) context.Context {
	return context.WithValue(ctx, operatorKey{}, operator)
}

// OperatorFrom はコンテキストの操作者を返す。なければ空文字。
func OperatorFrom(ctx context.Context) string {
	id, _ := ctx.Value(operatorKey{}).(string)
	return id
}

// --- 中継 ---

// RelayRequest は、管理クライアントから受けた要求をそのまま下流に送るときの内容。
type RelayRequest struct {
	Method string
	// Path は下流の基底 URL より後ろのパスの要素（PathEscape して "/" でつなぐ）。
	Path []string
	// RawQuery は管理クライアントから受けたクエリ文字列（そのまま渡す）。
	RawQuery string
	// Body は要求ボディ（なければ nil）。
	Body []byte
	// ContentType は要求ボディの Content-Type（Body があるときだけ使う）。
	ContentType string
	// IdempotencyKey は下流に渡す Idempotency-Key（radsec-ca とエッジだけ。空なら付けない）。
	IdempotencyKey string
}

// RelayResponse は下流の応答（エラーの応答を含む）。
type RelayResponse struct {
	Status      int
	ContentType string
	Location    string
	// RetryAfter は Retry-After（エッジの 503 TENANT_BUSY）。
	RetryAfter string
	Body       []byte
	// IdempotentReplayed は Idempotent-Replayed（radsec-ca とエッジの再送の応答）。
	IdempotentReplayed string
}

// Relay は要求をそのまま下流に送り、下流の応答をそのまま返す（ステータスが 2xx でなくてもエラーにしない）。
// エラーを返すのは、下流につながらない・タイムアウトなど、応答を受け取れなかったときだけ。
func (c *Client) Relay(ctx context.Context, req RelayRequest) (RelayResponse, error) {
	resp, err := c.do(ctx, req)
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
		RetryAfter:  resp.Header.Get("Retry-After"),
		Body:        body,

		IdempotentReplayed: resp.Header.Get(idempotentReplayedHeader),
	}, nil
}

func (c *Client) do(ctx context.Context, req RelayRequest) (*http.Response, error) {
	op := OperatorFrom(ctx)
	traceID := cmp.Or(trace.From(ctx), trace.New())
	method := req.Method

	u, err := c.url(req.Path)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", c.name, method, err)
	}
	u.RawQuery = req.RawQuery

	var rd io.Reader
	if req.Body != nil {
		rd = bytes.NewReader(req.Body)
	}
	hreq, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Accept", "application/json, application/problem+json")
	if req.ContentType != "" {
		hreq.Header.Set("Content-Type", req.ContentType)
	}
	if op != "" {
		hreq.Header.Set(operatorHeader, op)
	}
	if req.IdempotencyKey != "" {
		hreq.Header.Set(idempotencyKeyHeader, req.IdempotencyKey)
	}
	hreq.Header.Set(trace.Header, traceID)

	start := time.Now()
	resp, err := c.hc.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("%s %s %s: %w", c.name, method, u.Path, err)
	}
	c.log.Debug("downstream call", "method", method, "path", u.Path, "status", resp.StatusCode,
		"duration_ms", time.Since(start).Milliseconds(), "operator", op, "trace_id", traceID)
	return resp, nil
}

func drainClose(body io.ReadCloser) {
	io.Copy(io.Discard, io.LimitReader(body, maxResponseBytes))
	body.Close()
}

// url は基底 URL にパスの要素をつないだ URL を返す。要素はそれぞれ PathEscape するので、
// 値に "/" や "?" が含まれていても 1 つの要素として届く。"." と ".." と空の要素は受け付けない。
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
