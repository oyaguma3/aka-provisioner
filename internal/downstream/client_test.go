package downstream_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/certs"
	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/downstream/downstreamtest"
	"github.com/oyaguma3/aka-provisioner/internal/trace"
)

type status struct {
	Version string `json:"version"`
}

func newClient(t *testing.T, srv *downstreamtest.Server) *downstream.Client {
	t.Helper()
	opts := srv.Options
	opts.Name = downstream.Prov
	c, err := downstream.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func getStatus(t *testing.T, c *downstream.Client, ctx context.Context) (status, error) {
	t.Helper()
	return c.Call[status](ctx, downstream.Request{Method: http.MethodGet, Path: []string{"status"}})
}

func TestCallOverMTLS(t *testing.T) {
	var gotFP, gotPath, gotTrace, gotOperator string
	srv := downstreamtest.NewServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotFP = certs.Fingerprint(r.TLS.PeerCertificates[0])
		gotTrace = r.Header.Get("X-Trace-ID")
		gotOperator = r.Header.Get("X-Operator-Id")
		downstreamtest.WriteJSON(w, 200, map[string]any{"version": "0.3.0", "futureField": "ignored"})
	})
	c := newClient(t, srv)
	st, err := getStatus(t, c, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != "0.3.0" || gotPath != "/admin/v1/status" || gotFP != srv.ClientFP {
		t.Errorf("status = %+v, path = %q, client fp match = %v", st, gotPath, gotFP == srv.ClientFP)
	}
	// トレースID がなければ採番し、操作者がなければ送らない。
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(gotTrace) || gotOperator != "" {
		t.Errorf("X-Trace-ID = %q, X-Operator-Id = %q", gotTrace, gotOperator)
	}
	if c.Name() != downstream.Prov || !strings.HasSuffix(c.BaseURL(), "/admin/v1") ||
		certs.Fingerprint(c.ClientCertificate()) != srv.ClientFP {
		t.Error("accessors")
	}

	// コンテキストのトレースID と操作者を渡す。
	ctx := downstream.WithOperator(trace.With(t.Context(), "trace-001"), "alice")
	if _, err := getStatus(t, c, ctx); err != nil {
		t.Fatal(err)
	}
	if gotTrace != "trace-001" || gotOperator != "alice" {
		t.Errorf("X-Trace-ID = %q, X-Operator-Id = %q", gotTrace, gotOperator)
	}
}

func TestRequestBody(t *testing.T) {
	var gotType, gotQuery string
	var gotBody []byte
	srv := downstreamtest.NewServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotType, gotQuery = r.Header.Get("Content-Type"), r.URL.RawQuery
		gotBody = make([]byte, r.ContentLength)
		r.Body.Read(gotBody)
		w.WriteHeader(http.StatusNoContent)
	})
	c := newClient(t, srv)
	_, err := c.Call[struct{}](t.Context(), downstream.Request{
		Method: http.MethodPatch, Path: []string{"subscribers", "001010000000001"},
		Query: url.Values{"x": {"1"}}, Body: map[string]string{"amf": "b9b9"}, ContentType: "application/merge-patch+json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotType != "application/merge-patch+json" || gotQuery != "x=1" || string(gotBody) != `{"amf":"b9b9"}` {
		t.Errorf("type = %q, query = %q, body = %s", gotType, gotQuery, gotBody)
	}
}

func TestErrors(t *testing.T) {
	srv := downstreamtest.NewServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Trace-ID", r.Header.Get("X-Trace-ID"))
		switch r.URL.Path {
		case "/admin/v1/subscribers/001010000000001":
			downstreamtest.WriteProblem(w, 400, `{"title":"Bad Request","status":400,"cause":"MANDATORY_IE_INCORRECT",`+
				`"invalidParams":[{"param":"ki","reason":"must be 32 hex digits"}]}`)
		default:
			http.NotFound(w, r)
		}
	})
	c := newClient(t, srv)

	ctx := trace.With(t.Context(), "trace-err")
	_, err := c.Call[status](ctx, downstream.Request{Method: http.MethodGet, Path: []string{"subscribers", "001010000000001"}})
	apiErr, ok := errors.AsType[*downstream.Error](err)
	if !ok || apiErr.Status != 400 || apiErr.Downstream != downstream.Prov || apiErr.TraceID != "trace-err" ||
		!slices.Equal(apiErr.Problem.InvalidParams, []downstream.InvalidParam{{Param: "ki", Reason: "must be 32 hex digits"}}) ||
		downstream.CauseOf(err) != "MANDATORY_IE_INCORRECT" || downstream.IsUnavailable(err) {
		t.Errorf("err = %#v", err)
	}
	if !strings.Contains(err.Error(), "prov GET /admin/v1/subscribers/001010000000001: 400 Bad Request (MANDATORY_IE_INCORRECT)") {
		t.Errorf("message = %q", err)
	}

	// ProblemDetails でない応答も、ステータスで扱える。
	_, err = getStatus(t, c, t.Context())
	if apiErr, ok := errors.AsType[*downstream.Error](err); !ok || apiErr.Status != 404 || apiErr.Problem.Cause != "" {
		t.Errorf("plain 404: err = %v", err)
	}

	if downstream.IsUnavailable(nil) || !downstream.IsUnavailable(errors.New("dial")) || downstream.CauseOf(errors.New("x")) != "" {
		t.Error("IsUnavailable / CauseOf")
	}
}

func TestNewErrors(t *testing.T) {
	srv := downstreamtest.NewServer(t, func(http.ResponseWriter, *http.Request) {})
	for name, mutate := range map[string]func(*downstream.Options){
		"http url":          func(o *downstream.Options) { o.BaseURL = "http://127.0.0.1:9444/admin/v1" },
		"no client cert":    func(o *downstream.Options) { o.ClientCertFile = filepath.Join(srv.Dir, "none.pem") },
		"no server cert":    func(o *downstream.Options) { o.ServerCertFile = filepath.Join(srv.Dir, "none.pem") },
		"server cert empty": func(o *downstream.Options) { o.ServerCertFile = filepath.Join(srv.Dir, "empty.pem") },
	} {
		opts := srv.Options
		mutate(&opts)
		downstreamtest.WriteFile(t, filepath.Join(srv.Dir, "empty.pem"), nil)
		if _, err := downstream.New(opts); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

var hints = downstream.Hints{
	HostnameMismatch:  "hostname",
	UnknownServer:     "unknown",
	ServerCertInvalid: "invalid",
	ClientRejected:    "rejected",
	ConnectionReset:   "reset",
	NameNotFound:      "name %s",
	DialFailed:        "dial",
	Timeout:           "timeout",
}

func TestDiagnose(t *testing.T) {
	ok := func(w http.ResponseWriter, r *http.Request) { downstreamtest.WriteJSON(w, 200, map[string]any{}) }

	// 別の証明書を信頼している。
	srv := downstreamtest.NewServer(t, ok)
	otherPEM, _, err := certs.SelfSigned("other", []string{"127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	downstreamtest.WriteFile(t, filepath.Join(srv.Dir, "server.pem"), otherPEM)
	_, err = getStatus(t, newClient(t, srv), t.Context())
	if got := downstream.Diagnose(err, hints); got != "unknown" {
		t.Errorf("unknown authority: %q (err %v)", got, err)
	}

	// サーバー証明書の SAN に接続先がない。
	srv = downstreamtest.NewServer(t, ok)
	srv.Options.BaseURL = strings.Replace(srv.Options.BaseURL, "127.0.0.1", "localhost", 1)
	_, err = getStatus(t, newClient(t, srv), t.Context())
	if got := downstream.Diagnose(err, hints); got != "hostname" {
		t.Errorf("hostname: %q (err %v)", got, err)
	}

	// クライアント証明書を拒否された（下流と同じく VerifyConnection で拒否する）。
	srv = downstreamtest.NewServer(t, ok)
	srv.TLS.VerifyConnection = func(tls.ConnectionState) error { return errors.New("not a configured admin client") }
	_, err = getStatus(t, newClient(t, srv), t.Context())
	// TLS 1.3 では拒否のアラートより先に接続のリセットが届くことがある。
	if got := downstream.Diagnose(err, hints); got != "rejected" && got != "reset" {
		t.Errorf("rejected client: %q (err %v)", got, err)
	}

	resetErr := &url.Error{Op: "Get", URL: "https://provisioning-api:9444/admin/v1/status",
		Err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}
	if got := downstream.Diagnose(resetErr, hints); got != "reset" {
		t.Errorf("reset: %q", got)
	}

	// 接続できない（手元の WSL では、閉じたループバックのポートへの接続が拒否ではなくタイムアウトになる）。
	srv = downstreamtest.NewServer(t, ok)
	srv.Options.BaseURL = "https://" + downstreamtest.ClosedAddr(t) + "/admin/v1"
	srv.Options.Timeout = 500 * time.Millisecond
	_, err = getStatus(t, newClient(t, srv), t.Context())
	if got := downstream.Diagnose(err, hints); got != "dial" && got != "timeout" {
		t.Errorf("dial: %q (err %v)", got, err)
	}

	dnsErr := fmt.Errorf("wrap: %w", &net.DNSError{Name: "provisioning-api", IsNotFound: true})
	if got := downstream.Diagnose(dnsErr, hints); got != "name provisioning-api" {
		t.Errorf("dns: %q", got)
	}
	if got := downstream.Diagnose(&downstream.Error{Status: 404}, hints); got != "" {
		t.Errorf("api error: %q", got)
	}
	if got := downstream.Diagnose(nil, hints); got != "" {
		t.Errorf("nil: %q", got)
	}
}

func TestRelay(t *testing.T) {
	type captured struct {
		method, path, query, contentType, operator, trace, body string
	}
	var got captured
	srv := downstreamtest.NewServer(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		got = captured{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Content-Type"),
			r.Header.Get("X-Operator-Id"), r.Header.Get("X-Trace-ID"), string(b)}
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Location", "/admin/v1/clients/7")
			downstreamtest.WriteJSON(w, 201, map[string]any{"id": 7})
		default:
			downstreamtest.WriteProblem(w, 409, `{"title":"Conflict","status":409,"cause":"CLIENT_ALREADY_EXISTS"}`)
		}
	})
	c := newClient(t, srv)
	ctx := downstream.WithOperator(trace.With(t.Context(), "trace-relay"), "alice")

	// 本文はそのまま送り、Location とステータスを返す。
	resp, err := c.Relay(ctx, downstream.RelayRequest{
		Method: http.MethodPost, Path: []string{"clients"}, RawQuery: "a=1&b=%2F",
		Body: []byte(`{"ip":"198.51.100.1", "unknown":true}`), ContentType: "application/json; charset=utf-8",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 201 || resp.Location != "/admin/v1/clients/7" || resp.ContentType != "application/json" ||
		strings.TrimSpace(string(resp.Body)) != `{"id":7}` {
		t.Errorf("resp = %+v (%s)", resp, resp.Body)
	}
	want := captured{"POST", "/admin/v1/clients", "a=1&b=%2F", "application/json; charset=utf-8", "alice", "trace-relay",
		`{"ip":"198.51.100.1", "unknown":true}`}
	if got != want {
		t.Errorf("request = %+v, want %+v", got, want)
	}

	// 2xx 以外もエラーにせず、そのまま返す。パスの要素はエスケープする。
	resp, err = c.Relay(ctx, downstream.RelayRequest{Method: http.MethodPatch, Path: []string{"clients", "a/b"}})
	if err != nil || resp.Status != 409 || resp.ContentType != "application/problem+json" ||
		!strings.Contains(string(resp.Body), "CLIENT_ALREADY_EXISTS") || got.path != "/admin/v1/clients/a%2Fb" || got.contentType != "" {
		t.Errorf("relay 409: %+v, %v (request %+v)", resp, err, got)
	}

	// 届かなければエラー。
	srv.Options.BaseURL = "https://" + downstreamtest.ClosedAddr(t) + "/admin/v1"
	srv.Options.Timeout = 500 * time.Millisecond
	if _, err := newClient(t, srv).Relay(ctx, downstream.RelayRequest{Method: http.MethodGet, Path: []string{"clients"}}); !downstream.IsUnavailable(err) {
		t.Errorf("unreachable: %v", err)
	}
}

func TestListParams(t *testing.T) {
	if q := (downstream.ListParams{}).Query().Encode(); q != "" {
		t.Errorf("zero = %q", q)
	}
	if q := (downstream.ListParams{Prefix: "00101", Cursor: "001010000000005", Limit: 20}).Query().Encode(); q != "cursor=001010000000005&limit=20&prefix=00101" {
		t.Errorf("query = %q", q)
	}
}

func TestPathEscape(t *testing.T) {
	var got []string
	srv := downstreamtest.NewServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.EscapedPath())
		w.WriteHeader(http.StatusNoContent)
	})
	c := newClient(t, srv)
	// "/" を含む値も 1 つのセグメントとして送る（別のパスにならない）。
	for _, v := range []string{"1/secret", "a b", "%2F", "..x"} {
		if _, err := c.Call[struct{}](t.Context(), downstream.Request{Method: http.MethodDelete, Path: []string{"clients", v}}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"/admin/v1/clients/1%2Fsecret", "/admin/v1/clients/a%20b", "/admin/v1/clients/%252F", "/admin/v1/clients/..x"}
	if !slices.Equal(got, want) {
		t.Errorf("paths = %q, want %q", got, want)
	}
	// 空・"."・".." は送らない。
	for _, v := range []string{"", ".", ".."} {
		_, err := c.Relay(t.Context(), downstream.RelayRequest{Method: http.MethodGet, Path: []string{"clients", v, "secret"}})
		if err == nil {
			t.Errorf("%q: want error", v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("requests were sent: %q", got[len(want):])
	}
}
