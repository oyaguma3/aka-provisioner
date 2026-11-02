package downstream_test

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

var testTimeouts = downstream.Timeouts{Downstream: 500 * time.Millisecond, Radsec: 500 * time.Millisecond}

func ok(w http.ResponseWriter, r *http.Request) { downstreamtest.WriteJSON(w, 200, map[string]any{}) }

// load は、vecgen と radius を同じ srv に向けた登録簿を書いて読む（url と証明書は差し替えられる）。
func load(t *testing.T, vecgenURL string, vecgenCert []byte, radiusURL string, radiusCert []byte) (*downstream.Registry, downstreamtest.Registry) {
	t.Helper()
	w := downstreamtest.WriteRegistry(t, vecgenURL, vecgenCert, radiusURL, radiusCert)
	r, err := downstream.LoadRegistry(w.Path, testTimeouts, downstreamtest.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return r, w
}

func getStatus(t *testing.T, c *downstream.Client) error {
	t.Helper()
	_, err := c.Relay(t.Context(), downstream.RelayRequest{Method: http.MethodGet, Path: []string{"status"}})
	return err
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
			w.Header().Set("Location", "/admin/v2/subscribers/440100123456789")
			downstreamtest.WriteJSON(w, 201, map[string]any{"imsi": "440100123456789"})
		default:
			downstreamtest.WriteProblem(w, 409, `{"title":"Conflict","status":409,"cause":"SUBSCRIBER_ACTIVE"}`)
		}
	})
	reg, _ := load(t, srv.BaseURL, srv.ServerCertPEM, srv.BaseURL, srv.ServerCertPEM)
	c := reg.Client(downstream.Radius)
	if c.Name() != downstream.Radius || c.BaseURL() != srv.BaseURL {
		t.Errorf("client = %s %s", c.Name(), c.BaseURL())
	}
	ctx := downstream.WithOperator(trace.With(t.Context(), "trace-relay"), "alice")

	// 本文はそのまま送り、Location とステータスを返す。
	resp, err := c.Relay(ctx, downstream.RelayRequest{
		Method: http.MethodPost, Path: []string{"subscribers"}, RawQuery: "a=1&b=%2F",
		Body: []byte(`{"imsi":"440100123456789", "unknown":true}`), ContentType: "application/json; charset=utf-8",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 201 || resp.Location != "/admin/v2/subscribers/440100123456789" || resp.ContentType != "application/json" ||
		strings.TrimSpace(string(resp.Body)) != `{"imsi":"440100123456789"}` {
		t.Errorf("resp = %+v (%s)", resp, resp.Body)
	}
	want := captured{"POST", "/admin/v2/subscribers", "a=1&b=%2F", "application/json; charset=utf-8", "alice", "trace-relay",
		`{"imsi":"440100123456789", "unknown":true}`}
	if got != want {
		t.Errorf("request = %+v, want %+v", got, want)
	}

	// 2xx 以外もエラーにせず、そのまま返す。
	resp, err = c.Relay(ctx, downstream.RelayRequest{Method: http.MethodDelete, Path: []string{"subscribers", "440100123456789"}})
	if err != nil || resp.Status != 409 || resp.ContentType != "application/problem+json" ||
		!strings.Contains(string(resp.Body), "SUBSCRIBER_ACTIVE") || got.contentType != "" {
		t.Errorf("relay 409: %+v, %v (request %+v)", resp, err, got)
	}

	// トレースID がなければ採番する。
	if _, err := c.Relay(t.Context(), downstream.RelayRequest{Method: http.MethodGet, Path: []string{"status"}}); err != nil || !trace.Valid(got.trace) {
		t.Errorf("trace = %q, %v", got.trace, err)
	}
}

func TestPathEscape(t *testing.T) {
	var got []string
	srv := downstreamtest.NewServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.EscapedPath())
		w.WriteHeader(http.StatusNoContent)
	})
	reg, _ := load(t, srv.BaseURL, srv.ServerCertPEM, srv.BaseURL, srv.ServerCertPEM)
	c := reg.Client(downstream.Vecgen)
	// "/" を含む値も 1 つのセグメントとして送る（別のパスにならない）。
	for _, v := range []string{"1/keys", "a b", "%2F", "..x"} {
		if _, err := c.Relay(t.Context(), downstream.RelayRequest{Method: http.MethodGet, Path: []string{"subscribers", v}}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"/admin/v2/subscribers/1%2Fkeys", "/admin/v2/subscribers/a%20b", "/admin/v2/subscribers/%252F", "/admin/v2/subscribers/..x"}
	if !slices.Equal(got, want) {
		t.Errorf("paths = %q, want %q", got, want)
	}
	// 空・"."・".." は送らない。
	for _, v := range []string{"", ".", ".."} {
		if _, err := c.Relay(t.Context(), downstream.RelayRequest{Method: http.MethodGet, Path: []string{"subscribers", v, "keys"}}); err == nil {
			t.Errorf("%q: want error", v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("requests were sent: %q", got[len(want):])
	}
}

func TestRegistryErrors(t *testing.T) {
	srv := downstreamtest.NewServer(t, ok)
	w := downstreamtest.WriteRegistry(t, srv.BaseURL, srv.ServerCertPEM, srv.BaseURL, srv.ServerCertPEM)
	base := `"clientCert":"client.pem","clientKey":"client.key"`
	entry := func(u string) string { return `{"url":"` + u + `","serverCert":"vecgen-admin.pem"}` }
	for name, body := range map[string]string{
		"JSON でない":         `{`,
		"知らない項目":           `{` + base + `,"vecgen":` + entry(srv.BaseURL) + `,"radius":` + entry(srv.BaseURL) + `,"cas":{}}`,
		"radius がない":       `{` + base + `,"vecgen":` + entry(srv.BaseURL) + `}`,
		"平文の HTTP":         `{` + base + `,"vecgen":` + entry("http://127.0.0.1:1/admin/v2") + `,"radius":` + entry(srv.BaseURL) + `}`,
		"パスが違う":            `{` + base + `,"vecgen":` + entry("https://127.0.0.1:1/admin/v1") + `,"radius":` + entry(srv.BaseURL) + `}`,
		"クライアント証明書がない":     `{"clientCert":"none.pem","clientKey":"client.key","vecgen":` + entry(srv.BaseURL) + `,"radius":` + entry(srv.BaseURL) + `}`,
		"サーバー証明書がない":       `{` + base + `,"vecgen":{"url":"` + srv.BaseURL + `","serverCert":"none.pem"},"radius":` + entry(srv.BaseURL) + `}`,
		"サーバー証明書が PEM でない": `{` + base + `,"vecgen":{"url":"` + srv.BaseURL + `","serverCert":"client.key"},"radius":` + entry(srv.BaseURL) + `}`,
		"後ろにデータ":           `{` + base + `,"vecgen":` + entry(srv.BaseURL) + `,"radius":` + entry(srv.BaseURL) + `} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(w.Dir, "bad.json")
			downstreamtest.WriteFile(t, path, []byte(body))
			if _, err := downstream.LoadRegistry(path, testTimeouts, downstreamtest.Discard); err == nil {
				t.Error("want error")
			}
		})
	}
}

func TestRegistryReload(t *testing.T) {
	var hits []string
	a := downstreamtest.NewServer(t, func(w http.ResponseWriter, r *http.Request) { hits = append(hits, "a"); ok(w, r) })
	b := downstreamtest.NewServer(t, func(w http.ResponseWriter, r *http.Request) { hits = append(hits, "b"); ok(w, r) })
	reg, w := load(t, a.BaseURL, a.ServerCertPEM, a.BaseURL, a.ServerCertPEM)
	first := reg.LoadedAt()
	if err := getStatus(t, reg.Client(downstream.Vecgen)); err != nil {
		t.Fatal(err)
	}

	// 形の違うファイルでは前の登録簿のまま。
	orig, err := os.ReadFile(w.Path)
	if err != nil {
		t.Fatal(err)
	}
	downstreamtest.WriteFile(t, w.Path, []byte(`{`))
	if err := reg.Reload(); err == nil {
		t.Error("broken file: want error")
	}
	if !reg.LoadedAt().Equal(first) || reg.Client(downstream.Vecgen).BaseURL() != a.BaseURL {
		t.Error("registry changed after failed reload")
	}

	// vecgen を b に向けて読み直すと、次の要求から b に送る。
	downstreamtest.WriteFile(t, filepath.Join(w.Dir, "b.pem"), b.ServerCertPEM)
	updated := strings.Replace(string(orig), `{"serverCert":"vecgen-admin.pem","url":"`+a.BaseURL+`"}`,
		`{"serverCert":"b.pem","url":"`+b.BaseURL+`"}`, 1)
	if updated == string(orig) {
		t.Fatalf("registry file has unexpected form: %s", orig)
	}
	downstreamtest.WriteFile(t, w.Path, []byte(updated))
	if err := reg.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := getStatus(t, reg.Client(downstream.Vecgen)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hits, []string{"a", "b"}) || !reg.LoadedAt().After(first) {
		t.Errorf("hits = %v", hits)
	}
}

func TestDiagnose(t *testing.T) {
	// 別の証明書を信頼している。
	srv := downstreamtest.NewServer(t, ok)
	otherPEM, _, err := certs.SelfSigned("other", []string{"127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := load(t, srv.BaseURL, otherPEM, srv.BaseURL, srv.ServerCertPEM)
	err = getStatus(t, reg.Client(downstream.Vecgen))
	if got := downstream.Diagnose(downstream.Vecgen, err); !strings.Contains(got, "serverCert と一致しません") {
		t.Errorf("unknown authority: %q (err %v)", got, err)
	}

	// サーバー証明書の SAN に接続先がない。
	reg, _ = load(t, strings.Replace(srv.BaseURL, "127.0.0.1", "localhost", 1), srv.ServerCertPEM, srv.BaseURL, srv.ServerCertPEM)
	err = getStatus(t, reg.Client(downstream.Vecgen))
	if got := downstream.Diagnose(downstream.Vecgen, err); !strings.Contains(got, "SAN") {
		t.Errorf("hostname: %q (err %v)", got, err)
	}

	// クライアント証明書を拒否された（下流と同じく VerifyConnection で拒否する）。
	srv = downstreamtest.NewServer(t, ok)
	srv.TLS.VerifyConnection = func(tls.ConnectionState) error { return errors.New("not a configured admin client") }
	reg, _ = load(t, srv.BaseURL, srv.ServerCertPEM, srv.BaseURL, srv.ServerCertPEM)
	err = getStatus(t, reg.Client(downstream.Radius))
	// TLS 1.3 では拒否のアラートより先に接続のリセットが届くことがある。
	if got := downstream.Diagnose(downstream.Radius, err); !strings.Contains(got, "ピンのファイル") {
		t.Errorf("rejected client: %q (err %v)", got, err)
	}

	resetErr := &url.Error{Op: "Get", URL: "https://aka-radius-admin:9444/admin/v2/status",
		Err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}
	if got := downstream.Diagnose(downstream.Radius, resetErr); !strings.Contains(got, "radius が接続を切りました") {
		t.Errorf("reset: %q", got)
	}

	// 接続できない（手元の WSL では、閉じたループバックのポートへの接続が拒否ではなくタイムアウトになる）。
	reg, _ = load(t, "https://"+downstreamtest.ClosedAddr(t)+"/admin/v2", srv.ServerCertPEM, srv.BaseURL, srv.ServerCertPEM)
	err = getStatus(t, reg.Client(downstream.Vecgen))
	if got := downstream.Diagnose(downstream.Vecgen, err); !strings.Contains(got, "接続できません") && !strings.Contains(got, "応答がありません") {
		t.Errorf("dial: %q (err %v)", got, err)
	}

	dnsErr := fmt.Errorf("wrap: %w", &net.DNSError{Name: "aka-vecgen", IsNotFound: true})
	if got := downstream.Diagnose(downstream.Vecgen, dnsErr); !strings.Contains(got, "ホスト名 aka-vecgen を解決できません") {
		t.Errorf("dns: %q", got)
	}
	if got := downstream.Diagnose(downstream.Vecgen, nil); got != "" {
		t.Errorf("nil: %q", got)
	}
}

// radsecRegistry は、ca と edges を足した登録簿のファイルを読む。
func radsecRegistry(t *testing.T, srv *downstreamtest.Server, timeouts downstream.Timeouts, extra func(dir string, reg map[string]any)) (*downstream.Registry, downstreamtest.Registry) {
	t.Helper()
	w := downstreamtest.WriteRegistry(t, srv.BaseURL, srv.ServerCertPEM, srv.BaseURL, srv.ServerCertPEM, extra)
	r, err := downstream.LoadRegistry(w.Path, timeouts, downstreamtest.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return r, w
}

// TestRadsecDownstreams は、radsec-ca（接頭辞なし、serverCert）とエッジ（接頭辞なし、serverFingerprint）への中継を確かめる。
func TestRadsecDownstreams(t *testing.T) {
	var path, key string
	srv := downstreamtest.NewServer(t, func(w http.ResponseWriter, r *http.Request) {
		path, key = r.URL.EscapedPath(), r.Header.Get("Idempotency-Key")
		w.Header().Set("Retry-After", "2")
		w.Header().Set("Idempotent-Replayed", "true")
		downstreamtest.WriteProblem(w, 503, `{"title":"Service Unavailable","status":503,"cause":"TENANT_BUSY"}`)
	})
	reg, _ := radsecRegistry(t, srv, testTimeouts, func(dir string, m map[string]any) {
		downstreamtest.WriteFile(t, filepath.Join(dir, "ca-admin.pem"), srv.ServerCertPEM)
		m["ca"] = map[string]string{"url": srv.RootURL(), "serverCert": "ca-admin.pem"}
		// エッジのサーバー証明書はホスト名を見ない（SAN は 127.0.0.1 だけで、localhost につなぐ）。コロン区切りの大文字も書ける。
		fp := strings.ToUpper(srv.Fingerprint())
		m["edges"] = []any{
			map[string]string{"id": "edge-01", "url": strings.Replace(srv.RootURL(), "127.0.0.1", "localhost", 1), "serverFingerprint": fp[:2] + ":" + fp[2:]},
			map[string]string{"id": "edge-02", "url": srv.RootURL() + "/", "serverFingerprint": strings.Repeat("ab", 32)},
		}
	})
	want := []downstream.Name{downstream.Vecgen, downstream.Radius, downstream.CA, downstream.EdgeName("edge-01"), downstream.EdgeName("edge-02")}
	if got := reg.Names(); !slices.Equal(got, want) {
		t.Errorf("names = %v", got)
	}
	if reg.Client(downstream.EdgeName("none")) != nil {
		t.Error("unknown edge: want nil")
	}

	// radsec-ca: 接頭辞がなく、Idempotency-Key を渡し、Retry-After と Idempotent-Replayed を受け取る。":" はそのまま送る。
	ca := reg.Client(downstream.CA)
	resp, err := ca.Relay(t.Context(), downstream.RelayRequest{Method: http.MethodPost, Path: []string{"cas", "tenant.a", "crl:refresh"}, IdempotencyKey: "k-1"})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/cas/tenant.a/crl:refresh" || key != "k-1" || resp.RetryAfter != "2" || resp.IdempotentReplayed != "true" {
		t.Errorf("relay = %s %s %+v", path, key, resp)
	}
	if ca.ServerCertificate() == nil {
		t.Error("ca has no server certificate")
	}
	// Idempotency-Key を与えなければ付けない。
	if _, err := ca.Relay(t.Context(), downstream.RelayRequest{Method: http.MethodGet, Path: []string{"status"}}); err != nil || key != "" {
		t.Errorf("no key: %q, %v", key, err)
	}

	// エッジ: 接続するまで証明書を持たず、接続すると受け取った証明書を返す。
	edge := reg.Client(downstream.EdgeName("edge-01"))
	if edge.ServerCertificate() != nil {
		t.Error("edge has a certificate before connecting")
	}
	if _, err := edge.Relay(t.Context(), downstream.RelayRequest{Method: http.MethodGet, Path: []string{"tenants", "42"}}); err != nil {
		t.Fatal(err)
	}
	if path != "/tenants/42" || edge.ServerCertificate() == nil || edge.ServerCertificate().Subject.CommonName != "downstream" {
		t.Errorf("edge: %s %v", path, edge.ServerCertificate())
	}

	// フィンガープリントが違えば接続せず、原因の見当を出す。
	bad := reg.Client(downstream.EdgeName("edge-02"))
	err = getStatus(t, bad)
	if _, ok := errors.AsType[*downstream.FingerprintError](err); !ok {
		t.Fatalf("mismatch: %v", err)
	}
	if got := downstream.Diagnose(bad.Name(), err); !strings.Contains(got, "edge:edge-02") || !strings.Contains(got, "serverFingerprint と一致しません") {
		t.Errorf("hint = %q", got)
	}
	if bad.ServerCertificate() != nil {
		t.Error("the certificate of a mismatched edge is kept")
	}
}

// TestRadsecTimeout は、radsec-ca とエッジだけが長いタイムアウトを使い、原因の見当に環境変数の名前が出ることを確かめる。
func TestRadsecTimeout(t *testing.T) {
	srv := downstreamtest.NewServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		ok(w, r)
	})
	reg, _ := radsecRegistry(t, srv, downstream.Timeouts{Downstream: 50 * time.Millisecond, Radsec: 5 * time.Second}, func(dir string, m map[string]any) {
		downstreamtest.WriteFile(t, filepath.Join(dir, "ca-admin.pem"), srv.ServerCertPEM)
		m["ca"] = map[string]string{"url": srv.RootURL(), "serverCert": "ca-admin.pem"}
		m["edges"] = []any{map[string]string{"id": "edge-01", "url": srv.RootURL(), "serverFingerprint": srv.Fingerprint()}}
	})
	err := getStatus(t, reg.Client(downstream.Vecgen))
	if got := downstream.Diagnose(downstream.Vecgen, err); !strings.Contains(got, "PROVISIONER_DOWNSTREAM_TIMEOUT") {
		t.Errorf("vecgen: %q (err %v)", got, err)
	}
	for _, n := range []downstream.Name{downstream.CA, downstream.EdgeName("edge-01")} {
		if err := getStatus(t, reg.Client(n)); err != nil {
			t.Errorf("%s: %v", n, err)
		}
		if got := downstream.Diagnose(n, &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}); !strings.Contains(got, "PROVISIONER_RADSEC_TIMEOUT") {
			t.Errorf("%s: %q", n, got)
		}
	}
}

func TestRadsecRegistryErrors(t *testing.T) {
	srv := downstreamtest.NewServer(t, ok)
	fp := srv.Fingerprint()
	edge := func(id, u, fp string) map[string]string {
		return map[string]string{"id": id, "url": u, "serverFingerprint": fp}
	}
	for name, set := range map[string]func(m map[string]any){
		"ca の接頭辞": func(m map[string]any) {
			m["ca"] = map[string]string{"url": srv.BaseURL, "serverCert": "vecgen-admin.pem"}
		},
		"ca の証明書がない": func(m map[string]any) { m["ca"] = map[string]string{"url": srv.RootURL()} },
		"ca の平文": func(m map[string]any) {
			m["ca"] = map[string]string{"url": "http://127.0.0.1:1", "serverCert": "vecgen-admin.pem"}
		},
		"エッジの接頭辞":     func(m map[string]any) { m["edges"] = []any{edge("e1", srv.BaseURL, fp)} },
		"エッジの ID の形":  func(m map[string]any) { m["edges"] = []any{edge("Edge_1", srv.RootURL(), fp)} },
		"エッジの ID がない": func(m map[string]any) { m["edges"] = []any{edge("", srv.RootURL(), fp)} },
		"エッジの ID の重複": func(m map[string]any) {
			m["edges"] = []any{edge("e1", srv.RootURL(), fp), edge("e1", srv.RootURL(), fp)}
		},
		"指紋がない": func(m map[string]any) { m["edges"] = []any{edge("e1", srv.RootURL(), "")} },
		"指紋の形":  func(m map[string]any) { m["edges"] = []any{edge("e1", srv.RootURL(), "abcd")} },
		"エッジの serverCert": func(m map[string]any) {
			m["edges"] = []any{map[string]string{"id": "e1", "url": srv.RootURL(), "serverCert": "x.pem"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := downstreamtest.WriteRegistry(t, srv.BaseURL, srv.ServerCertPEM, srv.BaseURL, srv.ServerCertPEM, func(_ string, m map[string]any) { set(m) })
			if _, err := downstream.LoadRegistry(w.Path, testTimeouts, downstreamtest.Discard); err == nil {
				t.Error("want error")
			}
		})
	}
}

// TestRegistryReloadEdges は、読み直しでエッジを足せて、消したエッジのクライアントがなくなることを確かめる。
func TestRegistryReloadEdges(t *testing.T) {
	srv := downstreamtest.NewServer(t, ok)
	reg, w := load(t, srv.BaseURL, srv.ServerCertPEM, srv.BaseURL, srv.ServerCertPEM)
	if got := reg.Names(); !slices.Equal(got, []downstream.Name{downstream.Vecgen, downstream.Radius}) {
		t.Errorf("names = %v", got)
	}
	if reg.Client(downstream.CA) != nil {
		t.Error("ca: want nil")
	}
	orig, err := os.ReadFile(w.Path)
	if err != nil {
		t.Fatal(err)
	}
	added := strings.Replace(string(orig), `{"clientCert"`,
		`{"edges":[{"id":"e1","url":"`+srv.RootURL()+`","serverFingerprint":"`+srv.Fingerprint()+`"}],"clientCert"`, 1)
	downstreamtest.WriteFile(t, w.Path, []byte(added))
	if err := reg.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := getStatus(t, reg.Client(downstream.EdgeName("e1"))); err != nil {
		t.Fatal(err)
	}
	downstreamtest.WriteFile(t, w.Path, orig)
	if err := reg.Reload(); err != nil {
		t.Fatal(err)
	}
	if reg.Client(downstream.EdgeName("e1")) != nil || len(reg.Names()) != 2 {
		t.Errorf("edge remains: %v", reg.Names())
	}
}
