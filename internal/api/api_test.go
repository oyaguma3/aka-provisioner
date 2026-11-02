package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/audit"
	"github.com/oyaguma3/aka-provisioner/internal/certs"
	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/store"
	"github.com/oyaguma3/aka-provisioner/internal/trace"
)

// fakeRelayer は下流の代わり。受けた要求を記録し、relay で応える（nil なら 200 の空の JSON）。
type fakeRelayer struct {
	mu        sync.Mutex
	baseURL   string
	cert      *x509.Certificate
	relay     func(downstream.RelayRequest) (downstream.RelayResponse, error)
	requests  []downstream.RelayRequest
	operators []string
	traces    []string
}

func (f *fakeRelayer) BaseURL() string                      { return f.baseURL }
func (f *fakeRelayer) ServerCertificate() *x509.Certificate { return f.cert }
func (f *fakeRelayer) Relay(ctx context.Context, req downstream.RelayRequest) (downstream.RelayResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.operators = append(f.operators, downstream.OperatorFrom(ctx))
	f.traces = append(f.traces, trace.From(ctx))
	relay := f.relay
	f.mu.Unlock()
	if relay == nil {
		return downstream.RelayResponse{Status: 200, ContentType: "application/json", Body: []byte(`{}`)}, nil
	}
	return relay(req)
}

func (f *fakeRelayer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// memStore は Valkey の代わり（監査ログの Stream と Idempotency-Key の記録）。
type memStore struct {
	mu      sync.Mutex
	pingErr error
	audits  []store.AuditEntry
	idem    map[string]idemRecord
}

type idemRecord struct {
	hash string
	done bool
	resp store.StoredResponse
}

func (m *memStore) Ping(context.Context) error { return m.pingErr }

func (m *memStore) AppendAudit(_ context.Context, e store.AuditEntry, _ int64) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.ID = strconv.Itoa(len(m.audits)+1) + "-0"
	m.audits = append(m.audits, e)
	return e.ID, nil
}

func (m *memStore) ListAudit(_ context.Context, before string, limit int) ([]store.AuditEntry, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.AuditEntry
	for _, e := range slices.Backward(m.audits) {
		if before != "" && e.ID >= before {
			continue
		}
		out = append(out, e)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].ID
	}
	return out, next, nil
}

func (m *memStore) auditEntries() []store.AuditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.audits)
}

func (m *memStore) BeginIdempotent(_ context.Context, client, key, hash string, _ time.Duration) (store.IdempotencyState, store.StoredResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := client + "/" + key
	rec, ok := m.idem[k]
	switch {
	case !ok:
		m.idem[k] = idemRecord{hash: hash}
		return store.IdempotencyNew, store.StoredResponse{}, nil
	case rec.hash != hash:
		return store.IdempotencyMismatch, store.StoredResponse{}, nil
	case !rec.done:
		return store.IdempotencyInProgress, store.StoredResponse{}, nil
	}
	return store.IdempotencyReplay, rec.resp, nil
}

func (m *memStore) CompleteIdempotent(_ context.Context, client, key, hash string, resp store.StoredResponse, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.idem[client+"/"+key] = idemRecord{hash: hash, done: true, resp: resp}
	return nil
}

func (m *memStore) AbandonIdempotent(_ context.Context, client, key, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.idem, client+"/"+key)
	return nil
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type env struct {
	h      http.Handler
	st     *memStore
	vecgen *fakeRelayer
	radius *fakeRelayer
	ca     *fakeRelayer
	edge   *fakeRelayer
	edge2  *fakeRelayer
	// names は登録簿の今の下流（試験の中で変えて、読み直しの代わりにする）。
	names  []downstream.Name
	poller *Poller
	logs   *lockedBuffer
}

const (
	edge01 = downstream.Name("edge:edge-01")
	edge02 = downstream.Name("edge:edge-02")
)

// fake は下流の名前の代わりを返す。登録簿になければ nil。
func (e *env) fake(n downstream.Name) *fakeRelayer {
	if !slices.Contains(e.names, n) {
		return nil
	}
	return map[downstream.Name]*fakeRelayer{downstream.Vecgen: e.vecgen, downstream.Radius: e.radius, downstream.CA: e.ca, edge01: e.edge, edge02: e.edge2}[n]
}

func testCert(t *testing.T, name string) *x509.Certificate {
	t.Helper()
	certPEM, keyPEM, err := certs.SelfSigned(name, []string{name}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return c.Leaf
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		st:     &memStore{idem: map[string]idemRecord{}},
		vecgen: &fakeRelayer{baseURL: "https://vecgen.example:9443/admin/v2", cert: testCert(t, "vecgen.example")},
		radius: &fakeRelayer{baseURL: "https://radius.example:9444/admin/v2", cert: testCert(t, "radius.example")},
		ca:     &fakeRelayer{baseURL: "https://radsec-ca:9448", cert: testCert(t, "radsec-ca")},
		// エッジはまだ接続していない（cert が nil）。
		edge:  &fakeRelayer{baseURL: "https://10.0.0.1:9447"},
		edge2: &fakeRelayer{baseURL: "https://10.0.0.2:9447"},
		names: []downstream.Name{downstream.Vecgen, downstream.Radius, downstream.CA, edge01, edge02},
		logs:  &lockedBuffer{},
	}
	log := slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	relayer := func(n downstream.Name) Relayer {
		if f := e.fake(n); f != nil {
			return f
		}
		return nil
	}
	e.poller = &Poller{Names: func() []downstream.Name { return e.names }, Downstream: relayer, Log: log}
	client := testCert(t, "provisioner")
	server := testCert(t, "aka-provisioner")
	h := &Handler{
		Log:        log,
		MgmtClient: func(*http.Request) string { return "ossbss" },
		Downstream: relayer,
		Store:      e.st,
		Audit:      &audit.Recorder{Log: log, Store: e.st, MaxLen: 100},
		Status: &StatusSource{
			Version:             "v-test",
			StartedAt:           time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC),
			AdminClients:        func() ([]string, time.Time) { return []string{"ossbss"}, time.Date(2026, 10, 30, 1, 0, 1, 0, time.UTC) },
			DownstreamsLoadedAt: func() time.Time { return time.Date(2026, 10, 30, 1, 0, 2, 0, time.UTC) },
			ClientCertificate:   func() *x509.Certificate { return client },
			ServerCertificate:   func() *x509.Certificate { return server },
			Poller:              e.poller,
		},
	}
	e.h = h.Routes()
	return e
}

func (e *env) do(method, path string, header map[string]string, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder, wantStatus int) T {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d: %s", w.Code, wantStatus, w.Body)
	}
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
	return v
}

func problemOf(t *testing.T, w *httptest.ResponseRecorder, wantStatus int) problem {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content-type = %q", ct)
	}
	return decode[problem](t, w, wantStatus)
}

// pathValues は試験で使うパスの値。
var pathValues = map[string]string{"{imsi}": "440100123456789", "{clientId}": "7", "{caId}": "tenant.acme", "{serial}": "0abc12", "{tenantId}": "42"}

// TestAllowlist は、許す口がすべて下流の同じメソッドとパスに届き、書き込みでは Idempotency-Key を求め、
// radsec-ca とエッジにだけキーを渡すことを確かめる。
func TestAllowlist(t *testing.T) {
	for _, k := range kinds {
		for _, rt := range allowlist[k] {
			t.Run(string(k)+" "+rt.method+" "+rt.path, func(t *testing.T) {
				e := newEnv(t)
				fake := map[kind]*fakeRelayer{kindVecgen: e.vecgen, kindRadius: e.radius, kindCA: e.ca, kindEdge: e.edge}[k]
				path := rt.path
				for k, v := range pathValues {
					path = strings.ReplaceAll(path, k, v)
				}
				prefix := strings.ReplaceAll(k.pathPrefix(), "{edgeId}", "edge-01")
				url := basePath + prefix + path + "?limit=5"
				body := ""
				if rt.method != http.MethodGet && rt.method != http.MethodDelete {
					body = `{"state":"inactive"}`
				}
				if rt.write() {
					// キーがなければ下流を呼ばずに 400。
					p := problemOf(t, e.do(rt.method, url, nil, body), http.StatusBadRequest)
					if p.Cause != causeMandatoryIEMissing || fake.calls() != 0 {
						t.Errorf("without key: %+v, calls %d", p, fake.calls())
					}
				}
				w := e.do(rt.method, url, map[string]string{"Idempotency-Key": "k-1", "Content-Type": "application/json"}, body)
				if w.Code != http.StatusOK || fake.calls() != 1 {
					t.Fatalf("relay: %d %s, calls %d", w.Code, w.Body, fake.calls())
				}
				req := fake.requests[0]
				if req.Method != rt.method || "/"+strings.Join(req.Path, "/") != path || req.RawQuery != "limit=5" || string(req.Body) != body {
					t.Errorf("relayed = %+v, want %s %s", req, rt.method, path)
				}
				if want := map[bool]string{true: "k-1"}[k.radsec() && rt.write()]; req.IdempotencyKey != want {
					t.Errorf("idempotency key = %q, want %q", req.IdempotencyKey, want)
				}
				audits := e.st.auditEntries()
				if got := len(audits); got != map[bool]int{true: 1, false: 0}[rt.write()] {
					t.Fatalf("audits = %d", got)
				}
				if rt.write() {
					// パスの値のない口（登録と発行）は Location から取るので、ここでは空。
					want := pathValues["{"+rt.targetParam()+"}"]
					if a := audits[0]; a.Action != string(k)+"."+rt.action || a.Target != want {
						t.Errorf("audit = %+v, want target %q", a, want)
					}
				}
			})
		}
	}
}

// TestActions は監査ログの action の名前を確かめる（P5）。
func TestActions(t *testing.T) {
	var got []string
	for _, k := range []kind{kindCA, kindEdge} {
		for _, rt := range allowlist[k] {
			if rt.write() {
				got = append(got, string(k)+"."+rt.action)
			}
		}
	}
	want := []string{"ca.ca.create", "ca.ca.disable", "ca.crl.upload", "ca.crl.refresh", "ca.crl_source.put", "ca.cert.issue", "ca.cert.revoke",
		"edge.tenant.put", "edge.tenant.delete", "edge.server_csr.create", "edge.server_cert.put", "edge.probe_csr.create", "edge.probe_cert.put",
		"edge.tenant.apply", "edge.crl.refresh", "edge.tenant.restart"}
	if !slices.Equal(got, want) {
		t.Errorf("actions = %q", got)
	}
}

func TestDenied(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ method, path, ds string }{
		{http.MethodGet, "/admin/v2/vecgen/subscribers/440100123456789/keys", "vecgen"}, // 鍵の読み出しは通さない
		{http.MethodPost, "/admin/v2/radius/clients", "radius"},                         // RADIUS クライアントの書き込みはない
		{http.MethodPut, "/admin/v2/radius/subscribers", "radius"},                      // メソッドが違うだけでも許さない
		{http.MethodGet, "/admin/v2/vecgen/unknown", "vecgen"},
		{http.MethodDelete, "/admin/v2/ca/cas/tenant.acme", "ca"},
		{http.MethodPost, "/admin/v2/ca/cas/tenant.acme/refresh", "ca"}, // crl:refresh の綴りの違い
		{http.MethodGet, "/admin/v2/edges/edge-01/tenants/42/aps", "edge:edge-01"},
		{http.MethodPost, "/admin/v2/edges/none/unknown", "edge:none"},
		{http.MethodGet, "/admin/v2/edges/Bad_ID/unknown", "edge"},
	} {
		p := problemOf(t, e.do(c.method, c.path, map[string]string{"Idempotency-Key": "k"}, ""), http.StatusNotFound)
		if p.Cause != causeRouteNotAllowed || p.Downstream != c.ds {
			t.Errorf("%s %s: %+v", c.method, c.path, p)
		}
	}
	if e.vecgen.calls()+e.radius.calls()+e.ca.calls()+e.edge.calls() != 0 {
		t.Error("denied routes reached a downstream")
	}
	if !strings.Contains(e.logs.String(), `"event_id":"RELAY_DENIED"`) {
		t.Error("no RELAY_DENIED log")
	}
	// provisioner 自身の口は 404 と 405。
	if p := problemOf(t, e.do(http.MethodGet, "/admin/v2/unknown", nil, ""), http.StatusNotFound); p.Cause != "" {
		t.Errorf("unknown: %+v", p)
	}
	if w := e.do(http.MethodPost, "/admin/v2/status", nil, ""); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("405: %d %v", w.Code, w.Header())
	}
	// v1 の口はない。
	if w := e.do(http.MethodGet, "/admin/v1/status", nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("v1: %d", w.Code)
	}
}

func TestPathValues(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{
		"/admin/v2/vecgen/subscribers/1234",                    // 短すぎる
		"/admin/v2/radius/subscribers/4401001234567890",        // 長すぎる
		"/admin/v2/radius/subscribers/44010012345678x",         // 数字でない
		"/admin/v2/vecgen/clients/0",                           // 1 以上
		"/admin/v2/vecgen/clients/07",                          // 先頭の 0
		"/admin/v2/vecgen/subscribers/440100123456789%2Fkeys",  // "/" を含む値
		"/admin/v2/ca/cas/acme",                                // 接頭辞がない
		"/admin/v2/ca/cas/tenant.Acme",                         // 大文字
		"/admin/v2/ca/cas/tenant.",                             // 名前がない
		"/admin/v2/ca/cas/tenant.a.b",                          // 名前が多い
		"/admin/v2/ca/cas/brought.a",                           // 名前が足りない
		"/admin/v2/ca/cas/server2",                             // server の綴りの違い
		"/admin/v2/ca/cas/tenant.a%2Fb",                        // "/" を含む値
		"/admin/v2/ca/certs/0ABC",                              // 大文字
		"/admin/v2/ca/certs/xyz",                               // 16 進でない
		"/admin/v2/edges/Edge_1/status",                        // エッジの ID の形
		"/admin/v2/edges/edge-01/tenants/0",                    // 0 で始まる
		"/admin/v2/edges/edge-01/tenants/acme",                 // 数字でない
		"/admin/v2/edges/edge-01/tenants/12345678901234567890", // 20 桁
	} {
		p := problemOf(t, e.do(http.MethodGet, path, nil, ""), http.StatusBadRequest)
		if p.Cause != causeMandatoryIEIncorrect {
			t.Errorf("%s: %+v", path, p)
		}
	}
	if e.vecgen.calls()+e.radius.calls()+e.ca.calls()+e.edge.calls() != 0 {
		t.Error("bad path values reached a downstream")
	}
	// 正しい値は通す。
	for _, path := range []string{
		"/admin/v2/ca/cas/server", "/admin/v2/ca/cas/tenant.a", "/admin/v2/ca/cas/tenant.a-1", "/admin/v2/ca/cas/brought.acme.meraki-1",
		"/admin/v2/ca/certs/0abc", "/admin/v2/edges/edge-01/tenants/1234567890123456789",
	} {
		if w := e.do(http.MethodGet, path, nil, ""); w.Code != http.StatusOK {
			t.Errorf("%s: %d %s", path, w.Code, w.Body)
		}
	}
}

func TestCreateAndAudit(t *testing.T) {
	e := newEnv(t)
	e.vecgen.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{Status: 201, ContentType: "application/json", Location: "/admin/v2/subscribers/440100123456789",
			Body: []byte(`{"imsi":"440100123456789","state":"active"}`)}, nil
	}
	body := `{"imsi":"440100123456789","ki":"465b5ce8b199b49faa5f0a2ee238a6bc","opc":"cd63cb71954a9f4e48a5994e37a02baf","state":"active"}`
	w := e.do(http.MethodPost, "/admin/v2/vecgen/subscribers", map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "job-1:440100123456789", "X-Operator-Id": "alice", "X-Trace-ID": "trace-c1",
	}, body)
	if w.Code != 201 || w.Header().Get("Location") != "/admin/v2/vecgen/subscribers/440100123456789" ||
		w.Header().Get("X-Trace-ID") != "trace-c1" || !strings.Contains(w.Body.String(), `"state":"active"`) {
		t.Fatalf("response = %d %v %s", w.Code, w.Header(), w.Body)
	}
	// 操作者とトレースID を下流に渡す。aka-vecgen には Idempotency-Key を渡さない（RelayRequest にヘッダーはない）。
	if e.vecgen.operators[0] != "alice" || e.vecgen.traces[0] != "trace-c1" || e.vecgen.requests[0].ContentType != "application/json" {
		t.Errorf("operator %q trace %q", e.vecgen.operators[0], e.vecgen.traces[0])
	}
	audits := e.st.auditEntries()
	if len(audits) != 1 {
		t.Fatalf("audits = %+v", audits)
	}
	if a := audits[0]; a.Action != "vecgen.subscriber.create" || a.Target != "440100123456789" || a.Operator != "alice" ||
		a.MgmtClient != "ossbss" || a.TraceID != "trace-c1" || a.Detail != `{"downstream":"vecgen","fields":["imsi","ki","opc","state"]}` {
		t.Errorf("audit = %+v", a)
	}
	// 鍵はログにも監査ログにも残さない。
	if strings.Contains(e.logs.String(), "465b5ce8b199b49faa5f0a2ee238a6bc") {
		t.Error("key is logged")
	}

	// 状態の変更は IMSI を対象にする。失敗（4xx）は監査ログに残さない。
	e.radius.relay = func(req downstream.RelayRequest) (downstream.RelayResponse, error) {
		if req.Method == http.MethodDelete {
			return downstream.RelayResponse{Status: 409, ContentType: "application/problem+json",
				Body: []byte(`{"title":"Conflict","status":409,"cause":"SUBSCRIBER_ACTIVE"}`)}, nil
		}
		return downstream.RelayResponse{Status: 200, ContentType: "application/json", Body: []byte(`{}`)}, nil
	}
	e.do(http.MethodPut, "/admin/v2/radius/subscribers/440100123456789/state", map[string]string{"Idempotency-Key": "k-2"}, `{"state":"inactive","reason":"x"}`)
	p := problemOf(t, e.do(http.MethodDelete, "/admin/v2/radius/subscribers/440100123456789", map[string]string{"Idempotency-Key": "k-3"}, ""), 409)
	if p.Cause != "SUBSCRIBER_ACTIVE" || p.Downstream != "radius" {
		t.Errorf("409 = %+v", p)
	}
	audits = e.st.auditEntries()
	if len(audits) != 2 || audits[1].Action != "radius.subscriber.state" || audits[1].Target != "440100123456789" ||
		audits[1].Detail != `{"downstream":"radius","fields":["reason","state"]}` {
		t.Errorf("audits = %+v", audits)
	}
}

// TestRadsecRelay は radsec-ca とエッジの中継の作法を確かめる（P2・P8・P9・P10）。
func TestRadsecRelay(t *testing.T) {
	const crlKey = "AKIA-secret-meraki-api-key"
	body := `{"tenant":"acme","kind":"brought","name":"meraki","crl":{"source":"meraki","merakiOrgId":"1","merakiApiKey":"` + crlKey + `"}}`
	hdr := func(key string) map[string]string {
		return map[string]string{"Idempotency-Key": key, "Content-Type": "application/json", "X-Operator-Id": "alice"}
	}

	t.Run("登録の Location と target", func(t *testing.T) {
		e := newEnv(t)
		e.ca.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
			return downstream.RelayResponse{Status: 201, ContentType: "application/json", Location: "/cas/brought.acme.meraki", Body: []byte(`{"id":"brought.acme.meraki"}`)}, nil
		}
		w := e.do(http.MethodPost, "/admin/v2/ca/cas", hdr("k-1"), body)
		if w.Code != 201 || w.Header().Get("Location") != "/admin/v2/ca/cas/brought.acme.meraki" {
			t.Fatalf("response = %d %v", w.Code, w.Header())
		}
		// 再送でも書き換えた Location を返す。
		if again := e.do(http.MethodPost, "/admin/v2/ca/cas", hdr("k-1"), body); again.Header().Get("Location") != "/admin/v2/ca/cas/brought.acme.meraki" || e.ca.calls() != 1 {
			t.Errorf("replay = %v, calls %d", again.Header(), e.ca.calls())
		}
		a := e.st.auditEntries()
		if len(a) != 1 || a[0].Action != "ca.ca.create" || a[0].Target != "brought.acme.meraki" ||
			a[0].Detail != `{"downstream":"ca","fields":["crl","kind","name","tenant"]}` {
			t.Errorf("audit = %+v", a)
		}
		// Meraki の API キーの値はログにも監査ログにも残らない（項目の名前も最上位だけ）。
		if strings.Contains(e.logs.String(), crlKey) || strings.Contains(a[0].Detail, crlKey) || strings.Contains(a[0].Detail, "merakiApiKey") {
			t.Errorf("the API key is logged: %s %s", e.logs.String(), a[0].Detail)
		}
		// 発行の Location。
		e.ca.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
			return downstream.RelayResponse{Status: 201, ContentType: "application/json", Location: "/certs/0abc", Body: []byte(`{}`)}, nil
		}
		if w := e.do(http.MethodPost, "/admin/v2/ca/certs", hdr("k-2"), `{"profile":"ap-client"}`); w.Header().Get("Location") != "/admin/v2/ca/certs/0abc" {
			t.Errorf("issue = %v", w.Header())
		}
		if a := e.st.auditEntries(); a[1].Action != "ca.cert.issue" || a[1].Target != "0abc" {
			t.Errorf("issue audit = %+v", a[1])
		}
	})

	t.Run("エッジの下流の名前と Retry-After", func(t *testing.T) {
		e := newEnv(t)
		e.edge.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
			return downstream.RelayResponse{Status: 503, ContentType: "application/problem+json", RetryAfter: "2",
				Body: []byte(`{"title":"Service Unavailable","status":503,"cause":"TENANT_BUSY"}`)}, nil
		}
		w := e.do(http.MethodPost, "/admin/v2/edges/edge-01/tenants/42/restart", hdr("k-1"), "")
		p := problemOf(t, w, 503)
		if p.Cause != "TENANT_BUSY" || p.Downstream != "edge:edge-01" || w.Header().Get("Retry-After") != "2" {
			t.Errorf("busy = %+v %v", p, w.Header())
		}
		// 5xx は覚えないので、同じ鍵でやり直せる。
		e.edge.relay = nil
		if w := e.do(http.MethodPost, "/admin/v2/edges/edge-01/tenants/42/restart", hdr("k-1"), ""); w.Code != 200 || e.edge.calls() != 2 {
			t.Errorf("retry = %d, calls %d", w.Code, e.edge.calls())
		}
		a := e.st.auditEntries()
		if len(a) != 1 || a[0].Action != "edge.tenant.restart" || a[0].Target != "42" || a[0].Detail != `{"downstream":"edge:edge-01"}` {
			t.Errorf("audit = %+v", a)
		}
		// 別のエッジには別の下流の名前。同じ鍵を別のエッジに使うと 422（aka-ossbss は鍵をエッジごとに分ける）。
		e.do(http.MethodPost, "/admin/v2/edges/edge-02/tenants/42/restart", hdr("k-2"), "")
		if e.edge2.calls() != 1 || e.edge.calls() != 2 {
			t.Errorf("calls = %d %d", e.edge.calls(), e.edge2.calls())
		}
		if w := e.do(http.MethodPost, "/admin/v2/edges/edge-02/tenants/42/restart", hdr("k-1"), ""); w.Code != 422 {
			t.Errorf("same key on another edge = %d", w.Code)
		}
	})

	t.Run("下流の再送の印を中継する", func(t *testing.T) {
		e := newEnv(t)
		e.ca.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
			return downstream.RelayResponse{Status: 200, ContentType: "application/json", IdempotentReplayed: "true", Body: []byte(`{}`)}, nil
		}
		w := e.do(http.MethodPost, "/admin/v2/ca/cas/tenant.a/disable", hdr("k-1"), "")
		if w.Header().Get("Idempotent-Replayed") != "true" {
			t.Errorf("header = %v", w.Header())
		}
	})

	t.Run("下流の IDEMPOTENCY_CONFLICT は覚えない", func(t *testing.T) {
		e := newEnv(t)
		e.ca.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
			return downstream.RelayResponse{Status: 409, ContentType: "application/problem+json",
				Body: []byte(`{"title":"Conflict","status":409,"cause":"IDEMPOTENCY_CONFLICT"}`)}, nil
		}
		p := problemOf(t, e.do(http.MethodPost, "/admin/v2/ca/certs", hdr("k-1"), `{}`), 409)
		if p.Cause != "IDEMPOTENCY_CONFLICT" || p.Downstream != "ca" {
			t.Errorf("conflict = %+v", p)
		}
		e.ca.relay = nil
		if w := e.do(http.MethodPost, "/admin/v2/ca/certs", hdr("k-1"), `{}`); w.Code != 200 || w.Header().Get("Idempotent-Replayed") != "" || e.ca.calls() != 2 {
			t.Errorf("retry = %d %v, calls %d", w.Code, w.Header(), e.ca.calls())
		}
		// ほかの 409 は覚える。
		e.ca.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
			return downstream.RelayResponse{Status: 409, ContentType: "application/problem+json",
				Body: []byte(`{"title":"Conflict","status":409,"cause":"CA_EXISTS"}`)}, nil
		}
		e.do(http.MethodPost, "/admin/v2/ca/cas", hdr("k-2"), `{}`)
		if w := e.do(http.MethodPost, "/admin/v2/ca/cas", hdr("k-2"), `{}`); w.Code != 409 || w.Header().Get("Idempotent-Replayed") != "true" {
			t.Errorf("other 409 = %d %v", w.Code, w.Header())
		}
	})

	t.Run("登録簿にない下流", func(t *testing.T) {
		e := newEnv(t)
		e.names = []downstream.Name{downstream.Vecgen, downstream.Radius}
		// radsec-ca がなければ 503（書き込みも覚えない）。
		for _, c := range []struct {
			method, path string
			key          map[string]string
		}{{http.MethodGet, "/admin/v2/ca/status", nil}, {http.MethodPost, "/admin/v2/ca/certs", hdr("k-1")}} {
			p := problemOf(t, e.do(c.method, c.path, c.key, ""), 503)
			if p.Cause != causeDownstreamUnavailable || p.Downstream != "ca" {
				t.Errorf("%s: %+v", c.path, p)
			}
		}
		// 登録簿にないエッジは 404。
		p := problemOf(t, e.do(http.MethodPut, "/admin/v2/edges/edge-01/tenants/42", hdr("k-2"), `{}`), 404)
		if p.Cause != causeEdgeNotFound || p.Downstream != "edge:edge-01" {
			t.Errorf("edge = %+v", p)
		}
		// 足して読み直すと、同じキーでやり直せる。
		e.names = append(e.names, downstream.CA, edge01)
		if w := e.do(http.MethodPost, "/admin/v2/ca/certs", hdr("k-1"), ""); w.Code != 200 {
			t.Errorf("ca after adding = %d %s", w.Code, w.Body)
		}
		if w := e.do(http.MethodPut, "/admin/v2/edges/edge-01/tenants/42", hdr("k-2"), `{}`); w.Code != 200 {
			t.Errorf("edge after adding = %d %s", w.Code, w.Body)
		}
	})
}

func TestIdempotency(t *testing.T) {
	e := newEnv(t)
	e.radius.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{Status: 201, ContentType: "application/json", Location: "/admin/v2/subscribers/440100123456789",
			Body: []byte(`{"imsi":"440100123456789"}`)}, nil
	}
	hdr := map[string]string{"Idempotency-Key": "k-1", "Content-Type": "application/json"}
	body := `{"imsi":"440100123456789","state":"active","sourceId":"vecgen","rules":[]}`
	first := e.do(http.MethodPost, "/admin/v2/radius/subscribers", hdr, body)
	// 同じキー・同じ内容は、下流を呼ばずに最初の応答を返す。
	again := e.do(http.MethodPost, "/admin/v2/radius/subscribers", hdr, body)
	if again.Code != 201 || again.Header().Get("Idempotent-Replayed") != "true" || again.Body.String() != first.Body.String() ||
		again.Header().Get("Location") != "/admin/v2/radius/subscribers/440100123456789" || e.radius.calls() != 1 {
		t.Errorf("replay: %d %v %s, calls %d", again.Code, again.Header(), again.Body, e.radius.calls())
	}
	if len(e.st.auditEntries()) != 1 || !strings.Contains(e.logs.String(), `"event_id":"IDEMPOTENT_REPLAYED"`) {
		t.Errorf("audits = %d", len(e.st.auditEntries()))
	}
	// 同じキーで内容が違えば 422。
	if p := problemOf(t, e.do(http.MethodPost, "/admin/v2/radius/subscribers", hdr, `{"imsi":"440100123456780"}`), 422); p.Cause != causeIdempotencyKeyMismatch {
		t.Errorf("mismatch = %+v", p)
	}
	// キーの形が違えば 400。
	if p := problemOf(t, e.do(http.MethodDelete, "/admin/v2/radius/subscribers/440100123456789", map[string]string{"Idempotency-Key": "a b"}, ""), 400); p.Cause != causeOptionalIEIncorrect {
		t.Errorf("bad key = %+v", p)
	}
	// 下流に接続できない（5xx）は覚えず、同じキーでやり直せる。
	e.vecgen.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{}, errors.New("dial")
	}
	h2 := map[string]string{"Idempotency-Key": "k-5"}
	e.do(http.MethodDelete, "/admin/v2/vecgen/subscribers/440100123456789", h2, "")
	e.vecgen.relay = nil
	if w := e.do(http.MethodDelete, "/admin/v2/vecgen/subscribers/440100123456789", h2, ""); w.Code != 200 || w.Header().Get("Idempotent-Replayed") != "" || e.vecgen.calls() != 2 {
		t.Errorf("retry after 5xx: %d %v, calls %d", w.Code, w.Header(), e.vecgen.calls())
	}
}

func TestDownstreamErrors(t *testing.T) {
	e := newEnv(t)
	// 下流の ProblemDetails は、ステータスと内容をそのまま返し、downstream を加える。
	e.vecgen.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{Status: 404, ContentType: "application/problem+json",
			Body: []byte(`{"title":"Not Found","status":404,"cause":"USER_NOT_FOUND"}`)}, nil
	}
	if p := problemOf(t, e.do(http.MethodGet, "/admin/v2/vecgen/subscribers/440100123456789", nil, ""), 404); p.Cause != "USER_NOT_FOUND" || p.Downstream != "vecgen" {
		t.Errorf("passthrough = %+v", p)
	}
	// ProblemDetails でない誤りは 502。
	e.vecgen.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{Status: 500, ContentType: "text/plain", Body: []byte("boom")}, nil
	}
	if p := problemOf(t, e.do(http.MethodGet, "/admin/v2/vecgen/status", nil, ""), 502); p.Cause != causeDownstreamError || p.Downstream != "vecgen" || p.DownstreamStatus != 500 {
		t.Errorf("502 = %+v", p)
	}
	// 接続できなければ 503。
	e.radius.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{}, errors.New("dial tcp: connection refused")
	}
	if p := problemOf(t, e.do(http.MethodGet, "/admin/v2/radius/sessions?tenantId=1", nil, ""), 503); p.Cause != causeDownstreamUnavailable || p.Downstream != "radius" {
		t.Errorf("503 = %+v", p)
	}
	if !strings.Contains(e.logs.String(), `"event_id":"DOWNSTREAM_UNREACHABLE"`) {
		t.Error("no DOWNSTREAM_UNREACHABLE log")
	}
}

func TestOperatorAndTrace(t *testing.T) {
	e := newEnv(t)
	if p := problemOf(t, e.do(http.MethodGet, "/admin/v2/vecgen/status", map[string]string{"X-Operator-Id": "a b"}, ""), 400); p.Cause != causeOptionalIEIncorrect {
		t.Errorf("bad operator = %+v", p)
	}
	// トレースID がない・形が違うときは採番して返し、下流にも同じ値を渡す。
	w := e.do(http.MethodGet, "/admin/v2/vecgen/status", map[string]string{"X-Trace-ID": "bad trace"}, "")
	if id := w.Header().Get("X-Trace-ID"); !trace.Valid(id) || id == "bad trace" || e.vecgen.traces[0] != id {
		t.Errorf("trace = %q, relayed %q", id, e.vecgen.traces[0])
	}
	if !strings.Contains(e.logs.String(), `"event_id":"ADMIN_REQUEST"`) || !strings.Contains(e.logs.String(), `"mgmt_client":"ossbss"`) {
		t.Errorf("request log: %s", e.logs.String())
	}
}

func TestStatus(t *testing.T) {
	e := newEnv(t)
	e.vecgen.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{Status: 200, ContentType: "application/json", Body: []byte(`{"version":"vg-1","subscriberCount":3}`)}, nil
	}
	e.radius.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{}, errors.New("dial tcp: connection refused")
	}
	e.ca.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{Status: 200, ContentType: "application/json", Body: []byte(`{"service":"radsec-ca"}`)}, nil
	}
	e.edge.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{Status: 200, ContentType: "application/json", Body: []byte(`{"mode":"managed"}`)}, nil
	}
	e.edge2.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{}, errors.New("dial tcp: connection refused")
	}
	// 集める前は、つながるかが分からない（reachable false、checkedAt なし）。
	out := decode[map[string]any](t, e.do(http.MethodGet, "/admin/v2/status", nil, ""), 200)
	ds := out["downstreams"].([]any)
	if len(ds) != 5 || ds[0].(map[string]any)["reachable"] != false || ds[0].(map[string]any)["checkedAt"] != nil {
		t.Errorf("before poll = %v", ds)
	}

	e.poller.PollOnce(t.Context())
	out = decode[map[string]any](t, e.do(http.MethodGet, "/admin/v2/status", nil, ""), 200)
	if out["version"] != "v-test" || out["valkey"].(map[string]any)["reachable"] != true ||
		!slices.Equal(out["adminClients"].([]any), []any{"ossbss"}) || out["downstreamsLoadedAt"] != "2026-10-30T01:00:02Z" ||
		out["clientCertificate"].(map[string]any)["subject"] != "CN=provisioner" {
		t.Errorf("status = %v", out)
	}
	ds = out["downstreams"].([]any)
	vg, rd := ds[0].(map[string]any), ds[1].(map[string]any)
	if vg["name"] != "vecgen" || vg["url"] != "https://vecgen.example:9443/admin/v2" || vg["reachable"] != true ||
		vg["status"].(map[string]any)["subscriberCount"] != float64(3) || vg["serverCertificate"].(map[string]any)["subject"] != "CN=vecgen.example" {
		t.Errorf("vecgen = %v", vg)
	}
	if rd["name"] != "radius" || rd["reachable"] != false || rd["error"] == nil || rd["status"] != nil {
		t.Errorf("radius = %v", rd)
	}
	// つながらなくなったときだけログを出す（2 回目は出さない）。
	e.poller.PollOnce(t.Context())
	if n := strings.Count(e.logs.String(), `"event_id":"DOWNSTREAM_UNREACHABLE"`); n != 2 {
		t.Errorf("DOWNSTREAM_UNREACHABLE logged %d times", n)
	}
	// radsec-ca とエッジも出す。接続していないエッジは証明書を省く。読み直しで消えたエッジは出さない。
	ca, edge, edge2 := ds[2].(map[string]any), ds[3].(map[string]any), ds[4].(map[string]any)
	if ca["name"] != "ca" || ca["url"] != "https://radsec-ca:9448" || ca["status"].(map[string]any)["service"] != "radsec-ca" ||
		ca["serverCertificate"].(map[string]any)["subject"] != "CN=radsec-ca" {
		t.Errorf("ca = %v", ca)
	}
	if edge["name"] != "edge:edge-01" || edge["reachable"] != true || edge["status"].(map[string]any)["mode"] != "managed" || edge["serverCertificate"] != nil {
		t.Errorf("edge = %v", edge)
	}
	if edge2["name"] != "edge:edge-02" || edge2["reachable"] != false {
		t.Errorf("edge2 = %v", edge2)
	}
	e.edge.cert = testCert(t, "edge-01")
	ds = decode[map[string]any](t, e.do(http.MethodGet, "/admin/v2/status", nil, ""), 200)["downstreams"].([]any)
	if c := ds[3].(map[string]any)["serverCertificate"].(map[string]any); c["subject"] != "CN=edge-01" {
		t.Errorf("edge certificate = %v", c)
	}
	e.names = []downstream.Name{downstream.Vecgen, downstream.Radius, downstream.CA, edge01}
	e.poller.PollOnce(t.Context())
	ds = decode[map[string]any](t, e.do(http.MethodGet, "/admin/v2/status", nil, ""), 200)["downstreams"].([]any)
	if len(ds) != 4 {
		t.Errorf("after removing an edge = %v", ds)
	}
	if _, ok := e.poller.result(edge02); ok {
		t.Error("the result of the removed edge remains")
	}
	if !strings.Contains(e.logs.String(), `"event_id":"DOWNSTREAM_REACHABLE"`) {
		t.Error("no DOWNSTREAM_REACHABLE log")
	}

	// Valkey に届かなくても 200 で、valkey に誤りを出す。
	e.st.pingErr = errors.New("valkey is down")
	out = decode[map[string]any](t, e.do(http.MethodGet, "/admin/v2/status", nil, ""), 200)
	if v := out["valkey"].(map[string]any); v["reachable"] != false || v["error"] != "valkey is down" {
		t.Errorf("valkey = %v", v)
	}
}

func TestAuditLogs(t *testing.T) {
	e := newEnv(t)
	for i := range 3 {
		e.do(http.MethodPut, "/admin/v2/vecgen/subscribers/44010012345678"+strconv.Itoa(i)+"/state",
			map[string]string{"Idempotency-Key": "k" + strconv.Itoa(i), "X-Operator-Id": "alice"}, `{"state":"inactive"}`)
	}
	out := decode[auditListJSON](t, e.do(http.MethodGet, "/admin/v2/audit-logs?limit=2", nil, ""), 200)
	if len(out.Items) != 2 || out.NextBefore == "" || out.Items[0].Target != "440100123456782" || out.Items[0].Operator != "alice" ||
		out.Items[0].Action != "vecgen.subscriber.state" || string(out.Items[0].Detail) != `{"downstream":"vecgen","fields":["state"]}` {
		t.Errorf("page 1 = %+v", out)
	}
	out = decode[auditListJSON](t, e.do(http.MethodGet, "/admin/v2/audit-logs?limit=2&before="+out.NextBefore, nil, ""), 200)
	if len(out.Items) != 1 || out.NextBefore != "" || out.Items[0].Target != "440100123456780" {
		t.Errorf("page 2 = %+v", out)
	}
	for _, q := range []string{"limit=0", "limit=501", "before=x"} {
		if p := problemOf(t, e.do(http.MethodGet, "/admin/v2/audit-logs?"+q, nil, ""), 400); p.Cause != causeInvalidQueryParam {
			t.Errorf("%s: %+v", q, p)
		}
	}
}

func TestBodyLimit(t *testing.T) {
	e := newEnv(t)
	big := `{"imsis":["` + strings.Repeat("1", maxBodyBytes) + `"]}`
	if p := problemOf(t, e.do(http.MethodPost, "/admin/v2/radius/subscribers/batch-get", nil, big), 400); p.Cause != causeInvalidMsgFormat {
		t.Errorf("big body = %+v", p)
	}
	if e.radius.calls() != 0 {
		t.Error("big body reached the downstream")
	}
}
