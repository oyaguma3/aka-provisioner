package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/akaapi"
	"github.com/oyaguma3/aka-provisioner/internal/audit"
	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/plmn"
	"github.com/oyaguma3/aka-provisioner/internal/provapi"
	"github.com/oyaguma3/aka-provisioner/internal/store"
	"github.com/oyaguma3/aka-provisioner/internal/trace"
)

// ---- 偽物 ----

// fakeDownstream は下流の偽物。Relay の要求を記録し、relay で決めた応答を返す。
type fakeDownstream struct {
	mu      sync.Mutex
	baseURL string
	relay   func(req downstream.RelayRequest) (downstream.RelayResponse, error)
	// requests は Relay の要求、operators と traces はそのときのコンテキストの操作者とトレースID。
	requests  []downstream.RelayRequest
	operators []string
	traces    []string
}

func (f *fakeDownstream) BaseURL() string { return f.baseURL }
func (f *fakeDownstream) Relay(ctx context.Context, req downstream.RelayRequest) (downstream.RelayResponse, error) {
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

func (f *fakeDownstream) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

type fakeProv struct {
	fakeDownstream
	status provapi.Status
	err    error
	// statusOperators は Status のコンテキストに入っていた操作者。
	statusOperators []string
}

func (f *fakeProv) Status(ctx context.Context) (provapi.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusOperators = append(f.statusOperators, downstream.OperatorFrom(ctx))
	return f.status, f.err
}

type fakeAka struct {
	fakeDownstream
	status    akaapi.Status
	statusErr error
	av        akaapi.AVClient
	avErr     error
}

func (f *fakeAka) Status(context.Context) (akaapi.Status, error) { return f.status, f.statusErr }
func (f *fakeAka) GetAVClient(context.Context, int64) (akaapi.AVClient, error) {
	return f.av, f.avErr
}

// memStore は provisioner 専用 Valkey の偽物（メモリ上。ロックと Idempotency-Key の振る舞いを持つ）。
type memStore struct {
	mu        sync.Mutex
	pingErr   error
	auditErr  error
	audits    []store.AuditEntry
	locks     map[string]string
	idem      map[string]*memIdem
	nextToken int
}

type memIdem struct {
	hash string
	done bool
	resp store.StoredResponse
}

func newMemStore() *memStore {
	return &memStore{locks: map[string]string{}, idem: map[string]*memIdem{}}
}

func (m *memStore) Ping(context.Context) error { return m.pingErr }

func (m *memStore) AppendAudit(_ context.Context, e store.AuditEntry, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.auditErr != nil {
		return m.auditErr
	}
	e.ID = fmt.Sprintf("%d-0", len(m.audits)+1)
	e.Time = time.Now().UTC()
	m.audits = append(m.audits, e)
	return nil
}

func (m *memStore) ListAudit(_ context.Context, before string, limit int) ([]store.AuditEntry, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.AuditEntry
	for i := len(m.audits) - 1; i >= 0; i-- {
		e := m.audits[i]
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

func (m *memStore) AcquireIMSILock(_ context.Context, imsi string, _ time.Duration) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.locks[imsi]; ok {
		return "", store.ErrLocked
	}
	m.nextToken++
	token := fmt.Sprint(m.nextToken)
	m.locks[imsi] = token
	return token, nil
}

func (m *memStore) ReleaseIMSILock(_ context.Context, imsi, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks[imsi] == token {
		delete(m.locks, imsi)
	}
	return nil
}

func (m *memStore) BeginIdempotent(_ context.Context, client, key, hash string, _ time.Duration) (store.IdempotencyState, store.StoredResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.idem[client+":"+key]
	switch {
	case !ok:
		m.idem[client+":"+key] = &memIdem{hash: hash}
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
	if rec, ok := m.idem[client+":"+key]; ok && rec.hash == hash {
		rec.done, rec.resp = true, resp
	}
	return nil
}

func (m *memStore) AbandonIdempotent(_ context.Context, client, key, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, ok := m.idem[client+":"+key]; ok && rec.hash == hash && !rec.done {
		delete(m.idem, client+":"+key)
	}
	return nil
}

type env struct {
	h    http.Handler
	prov *fakeProv
	aka  *fakeAka
	st   *memStore
	subs *fakeSubs
	logs *lockedBuffer
}

// lockedBuffer は、ハンドラーのゴルーチンが書くログを、テストから安全に読むためのバッファ。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.buf.Bytes())
}

func newEnv(t *testing.T, withAka bool) *env {
	t.Helper()
	m, err := plmn.Parse("00102:01,001010:00")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		prov: &fakeProv{
			fakeDownstream: fakeDownstream{baseURL: "https://provisioning-api:9444/admin/v1"},
			status:         provapi.Status{Version: "0.3.0", NodeName: "poc-01", SubscriberCount: 3},
		},
		aka: &fakeAka{
			fakeDownstream: fakeDownstream{baseURL: "https://aka-vecgen:9443/admin/v1"},
			status:         akaapi.Status{Version: "1.0.0", SubscriberCount: 7},
			av:             akaapi.AVClient{ID: 2, Name: "vector-gateway", Enabled: true},
		},
		st:   newMemStore(),
		subs: &fakeSubs{},
		logs: &lockedBuffer{},
	}
	h := &Handler{
		Log:               slog.New(slog.NewJSONHandler(e.logs, nil)),
		MgmtClient:        func(*http.Request) string { return "bff-01" },
		Prov:              e.prov,
		PLMNMap:           m,
		Store:             e.st,
		Subscribers:       e.subs,
		DownstreamTimeout: time.Second,
		Version:           "test",
		StartedAt:         time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
	}
	h.Audit = &audit.Recorder{Log: h.Log, Store: e.st, MaxLen: 1000}
	if withAka {
		h.Aka, h.AkaAVClientID = e.aka, 2
	}
	e.h = h.Routes()
	return e
}

func (e *env) do(method, path string, header map[string]string) *httptest.ResponseRecorder {
	return e.doBody(method, path, header, "")
}

// doBody は本文つきの要求を送る（本文が空文字列なら送らない）。
func (e *env) doBody(method, path string, header map[string]string, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, "https://provisioner"+path, rd)
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
		t.Fatalf("status = %d, want %d (body %s)", w.Code, wantStatus, w.Body)
	}
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	return v
}

// ---- /status ----

func TestStatus(t *testing.T) {
	e := newEnv(t, true)
	got := decode[map[string]any](t, e.do("GET", "/admin/v1/status", nil), http.StatusOK)
	want := map[string]any{
		"version":   "test",
		"startedAt": "2026-10-10T00:00:00Z",
		"plmnMap": []any{
			map[string]any{"plmn": "00102", "keyStore": "aka"},
			map[string]any{"plmn": "001010", "keyStore": "poc"},
		},
		"downstreams": map[string]any{
			"prov": map[string]any{"configured": true, "url": "https://provisioning-api:9444/admin/v1", "reachable": true,
				"version": "0.3.0", "nodeName": "poc-01", "subscriberCount": 3.0},
			"aka": map[string]any{"configured": true, "url": "https://aka-vecgen:9443/admin/v1", "reachable": true,
				"version": "1.0.0", "subscriberCount": 7.0},
		},
		"avClient":   map[string]any{"id": 2.0, "exists": true, "enabled": true, "name": "vector-gateway"},
		"valkey":     map[string]any{"reachable": true},
		"operations": map[string]any{"running": 0.0, "retrying": 0.0, "failed": 0.0},
	}
	if !jsonEqual(got, want) {
		t.Errorf("status =\n%v\nwant\n%v", got, want)
	}
}

func TestStatusFailures(t *testing.T) {
	// 下流や Valkey に接続できなくても 200 で、項目ごとに示す。
	e := newEnv(t, true)
	e.prov.err = errors.New("dial tcp: connection refused")
	e.aka.avErr = &downstream.Error{Downstream: downstream.Aka, Status: 404, Problem: downstream.Problem{Cause: "CLIENT_NOT_FOUND"}}
	e.st.pingErr = errors.New("valkey down")
	got := decode[statusJSON](t, e.do("GET", "/admin/v1/status", nil), http.StatusOK)
	if p := got.Downstreams.Prov; p.Reachable || p.Error == "" || p.SubscriberCount != nil {
		t.Errorf("prov = %+v", p)
	}
	if av := got.AVClient; av.Exists == nil || *av.Exists || av.Enabled != nil {
		t.Errorf("av client = %+v", av)
	}
	if got.Valkey.Reachable || got.Valkey.Error != "valkey down" || got.Operations != nil {
		t.Errorf("valkey = %+v, operations = %+v", got.Valkey, got.Operations)
	}

	// aka-vecgen に接続できなければ、AVクライアントは分からない（exists を省略）。
	e = newEnv(t, true)
	e.aka.statusErr = errors.New("timeout")
	got = decode[statusJSON](t, e.do("GET", "/admin/v1/status", nil), http.StatusOK)
	if a := got.Downstreams.Aka; a.Reachable || a.Error != "timeout" || got.AVClient.Exists != nil || got.AVClient.ID != 2 {
		t.Errorf("aka = %+v, av = %+v", a, got.AVClient)
	}

	// aka-vecgen を扱わない設定。
	e = newEnv(t, false)
	raw := decode[map[string]any](t, e.do("GET", "/admin/v1/status", nil), http.StatusOK)
	if !jsonEqual(raw["downstreams"].(map[string]any)["aka"], map[string]any{"configured": false, "reachable": false}) ||
		!jsonEqual(raw["avClient"], map[string]any{"id": 0.0}) {
		t.Errorf("aka not configured: %v / %v", raw["downstreams"], raw["avClient"])
	}
}

// ---- 共通の作法 ----

func TestTraceID(t *testing.T) {
	e := newEnv(t, false)
	if w := e.do("GET", "/admin/v1/status", map[string]string{"X-Trace-ID": "trace-001"}); w.Header().Get("X-Trace-ID") != "trace-001" {
		t.Errorf("given: %q", w.Header().Get("X-Trace-ID"))
	}
	generated := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for _, v := range []string{"", "has space"} {
		if w := e.do("GET", "/admin/v1/status", map[string]string{"X-Trace-ID": v}); !generated.MatchString(w.Header().Get("X-Trace-ID")) {
			t.Errorf("%q: %q", v, w.Header().Get("X-Trace-ID"))
		}
	}
	// エラーの応答にも付ける。
	if w := e.do("GET", "/admin/v1/nothing", map[string]string{"X-Trace-ID": "trace-404"}); w.Header().Get("X-Trace-ID") != "trace-404" {
		t.Errorf("404: %q", w.Header().Get("X-Trace-ID"))
	}
}

func TestOperator(t *testing.T) {
	e := newEnv(t, false)
	// 形式が違えば 400。
	p := decode[problem](t, e.do("GET", "/admin/v1/status", map[string]string{"X-Operator-Id": "bad operator!"}), http.StatusBadRequest)
	if p.Cause != "OPTIONAL_IE_INCORRECT" || len(p.InvalidParams) != 1 || p.InvalidParams[0].Param != "X-Operator-Id" {
		t.Errorf("problem = %+v", p)
	}
	// 下流にそのまま渡す（省略された場合は渡さない）。
	e.do("GET", "/admin/v1/status", map[string]string{"X-Operator-Id": "alice@example"})
	e.do("GET", "/admin/v1/status", nil)
	if ops := e.prov.statusOperators; len(ops) != 2 || ops[0] != "alice@example" || ops[1] != "" {
		t.Errorf("operators = %q", ops)
	}
}

func TestNotFoundAndMethodNotAllowed(t *testing.T) {
	e := newEnv(t, false)
	w := e.do("GET", "/admin/v1/nothing", nil)
	if p := decode[problem](t, w, http.StatusNotFound); p.Cause != "" || p.Title != "Not Found" ||
		w.Header().Get("Content-Type") != "application/problem+json" {
		t.Errorf("404 = %+v", p)
	}
	w = e.do("DELETE", "/admin/v1/status", nil)
	if p := decode[problem](t, w, http.StatusMethodNotAllowed); p.Cause != "" || w.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("405 = %+v, Allow = %q", p, w.Header().Get("Allow"))
	}
}

func TestRequestLog(t *testing.T) {
	e := newEnv(t, false)
	e.do("GET", "/admin/v1/status?x=secret", map[string]string{"X-Trace-ID": "trace-log", "X-Operator-Id": "alice"})
	e.do("GET", "/admin/v1/status", map[string]string{"X-Trace-ID": "trace-bad", "X-Operator-Id": "bad operator!"})

	entries := map[string]map[string]any{}
	sc := bufio.NewScanner(bytes.NewReader(e.logs.Bytes()))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err == nil && m["msg"] == "request completed" {
			entries[m["trace_id"].(string)] = m
		}
	}
	if m := entries["trace-log"]; m == nil || m["method"] != "GET" || m["path"] != "/admin/v1/status" ||
		m["http_status"] != 200.0 || m["mgmt_client"] != "bff-01" || m["operator"] != "alice" {
		t.Errorf("log = %v", m)
	}
	// 形式の違う操作者 ID はそのまま記録しない。
	if m := entries["trace-bad"]; m == nil || m["http_status"] != 400.0 || m["operator"] != "" {
		t.Errorf("bad operator log = %v", m)
	}
	// クエリ文字列は出さない。
	if bytes.Contains(e.logs.Bytes(), []byte("secret")) {
		t.Errorf("query string is logged: %s", e.logs.Bytes())
	}
}

func jsonEqual(a, b any) bool {
	x, err1 := json.Marshal(a, json.Deterministic(true))
	y, err2 := json.Marshal(b, json.Deterministic(true))
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}
