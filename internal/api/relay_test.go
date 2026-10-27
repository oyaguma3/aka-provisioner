package api

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/downstream"
)

func problemResp(status int, cause string) downstream.RelayResponse {
	return downstream.RelayResponse{Status: status, ContentType: "application/problem+json",
		Body: []byte(`{"title":"x","status":` + itoa(status) + `,"cause":"` + cause + `"}`)}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestRelayCreateClient(t *testing.T) {
	e := newEnv(t, false)
	e.prov.relay = func(req downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{Status: 201, ContentType: "application/json", Location: "/admin/v1/clients/7",
			Body: []byte(`{"id":7,"ip":"198.51.100.1","name":"ap","vendor":""}`)}, nil
	}
	body := `{"ip":"198.51.100.1","secret":"s3cr3t","name":"ap"}`
	w := e.doBody("POST", "/admin/v1/clients?x=1", map[string]string{
		"Content-Type": "application/json; charset=utf-8", "X-Operator-Id": "alice", "X-Trace-ID": "trace-c1",
	}, body)
	if w.Code != 201 || w.Header().Get("Location") != "/admin/v1/clients/7" || w.Header().Get("Content-Type") != "application/json" ||
		!strings.Contains(w.Body.String(), `"id":7`) {
		t.Fatalf("response = %d %v %s", w.Code, w.Header(), w.Body)
	}
	// 本文・Content-Type・クエリ・操作者・トレースID をそのまま下流に渡す。
	req := e.prov.requests[0]
	if req.Method != "POST" || !slices.Equal(req.Path, []string{"clients"}) || req.RawQuery != "x=1" || string(req.Body) != body ||
		req.ContentType != "application/json; charset=utf-8" || e.prov.operators[0] != "alice" || e.prov.traces[0] != "trace-c1" {
		t.Errorf("relayed = %+v, operator %q, trace %q", req, e.prov.operators[0], e.prov.traces[0])
	}
	// 監査ログ: 対象は採番された ID。秘密の値は残さない。
	audits := e.st.auditEntries()
	if len(audits) != 1 {
		t.Fatalf("audits = %+v", audits)
	}
	if a := audits[0]; a.Action != "client.create" || a.Target != "7" || a.Operator != "alice" || a.MgmtClient != "bff-01" ||
		a.TraceID != "trace-c1" || a.Result != "completed" || a.Details != `{"downstream":"prov"}` {
		t.Errorf("audit = %+v", a)
	}
	if strings.Contains(string(e.logs.Bytes()), "s3cr3t") {
		t.Error("secret is logged")
	}
}

func TestRelayErrors(t *testing.T) {
	e := newEnv(t, false)
	// 下流の ProblemDetails は、ステータスと内容をそのまま返し、downstream を加える。
	e.prov.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return problemResp(409, "CLIENT_ALREADY_EXISTS"), nil
	}
	w := e.doBody("POST", "/admin/v1/clients", nil, `{}`)
	if p := decode[problem](t, w, 409); p.Cause != "CLIENT_ALREADY_EXISTS" || p.Downstream != "prov" || p.Title != "x" {
		t.Errorf("409 = %+v", p)
	}
	if len(e.st.auditEntries()) != 0 {
		t.Error("failed request is audited")
	}

	// ProblemDetails でない応答は 502。
	e.prov.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{Status: 404, ContentType: "text/plain", Body: []byte("404 page not found")}, nil
	}
	if p := decode[problem](t, e.do("GET", "/admin/v1/clients/3", nil), 502); p.Cause != "DOWNSTREAM_ERROR" ||
		p.Downstream != "prov" || p.DownstreamStatus != 404 {
		t.Errorf("502 = %+v", p)
	}

	// 届かなければ 503。
	e.prov.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{}, errors.New("dial tcp: connection refused")
	}
	if p := decode[problem](t, e.do("GET", "/admin/v1/sessions", nil), 503); p.Cause != "DOWNSTREAM_UNAVAILABLE" || p.Downstream != "prov" {
		t.Errorf("503 = %+v", p)
	}
}

func TestRelayPathValues(t *testing.T) {
	e := newEnv(t, false)
	for _, path := range []string{
		"/admin/v1/clients/abc", "/admin/v1/clients/0", "/admin/v1/clients/01", "/admin/v1/clients/1%2Fsecret",
		"/admin/v1/clients/99999999999999999999/secret", "/admin/v1/policies/12345", "/admin/v1/policies/00101000000000a",
		"/admin/v1/policies/001010000000001%2F",
	} {
		p := decode[problem](t, e.do("GET", path, nil), 400)
		if p.Cause != "MANDATORY_IE_INCORRECT" || len(p.InvalidParams) != 1 {
			t.Errorf("%s: %+v", path, p)
		}
	}
	if n := e.prov.calls(); n != 0 {
		t.Errorf("relayed %d requests", n)
	}
	if p := decode[problem](t, e.doBody("PUT", "/admin/v1/policies/12345/status", nil, `{"status":"active"}`), 400); p.Cause != "MANDATORY_IE_INCORRECT" {
		t.Errorf("status: %+v", p)
	}

	// 正しい値はそのまま下流のパスになる。
	e.do("GET", "/admin/v1/clients/12/secret", nil)
	e.do("GET", "/admin/v1/policies/001010000000001", nil)
	e.doBody("PUT", "/admin/v1/policies/001010000000001/status", nil, `{"status":"suspended"}`)
	if got := [][]string{e.prov.requests[0].Path, e.prov.requests[1].Path, e.prov.requests[2].Path}; !slices.Equal(got[0], []string{"clients", "12", "secret"}) ||
		!slices.Equal(got[1], []string{"policies", "001010000000001"}) || !slices.Equal(got[2], []string{"policies", "001010000000001", "status"}) {
		t.Errorf("paths = %v", got)
	}
	// 本文は検証せずにそのまま送る。
	if r := e.prov.requests[2]; r.Method != "PUT" || string(r.Body) != `{"status":"suspended"}` {
		t.Errorf("status request = %+v", r)
	}
}

func TestRelayAudit(t *testing.T) {
	e := newEnv(t, false)
	status := 200
	respBody := `{}`
	e.prov.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{Status: status, ContentType: "application/json", Body: []byte(respBody)}, nil
	}
	e.do("GET", "/admin/v1/clients/5/secret", nil)
	e.doBody("PATCH", "/admin/v1/clients/5", nil, `{"secret":"new","name":"ap-2"}`)
	status = 204
	e.do("DELETE", "/admin/v1/clients/5", nil)
	status = 201
	e.doBody("PUT", "/admin/v1/policies/001010000000001", nil, `{"default":"deny","rules":[]}`)
	status = 200
	e.doBody("PUT", "/admin/v1/policies/001010000000001", nil, `{"default":"allow","rules":[]}`)
	status = 204
	e.do("DELETE", "/admin/v1/policies/001010000000001", nil)
	// 停止・再開は、応答の状態（変更後）で分ける。同じ状態への変更も残す。
	status = 200
	respBody = `{"imsi":"001010000000001","default":"deny","rules":[],"status":"suspended"}`
	e.doBody("PUT", "/admin/v1/policies/001010000000001/status", nil, `{"status":"suspended"}`)
	e.doBody("PUT", "/admin/v1/policies/001010000000001/status", nil, `{"status":"suspended"}`)
	respBody = `{"imsi":"001010000000001","default":"deny","rules":[],"status":"active"}`
	e.doBody("PUT", "/admin/v1/policies/001010000000001/status", nil, `{"status":" Active "}`)
	respBody = `{}`
	e.doBody("PUT", "/admin/v1/policies/001010000000001/status", nil, `{"status":"active"}`)
	// 断られた要求は残さない。
	status = 404
	e.doBody("PUT", "/admin/v1/policies/001010000000002/status", nil, `{"status":"suspended"}`)
	// 読み取り（秘密の値以外）は残さない。
	status = 200
	e.do("GET", "/admin/v1/clients", nil)
	e.do("GET", "/admin/v1/policies/001010000000001", nil)

	var got []string
	for _, a := range e.st.auditEntries() {
		got = append(got, a.Action+" "+a.Target+" "+a.Details)
	}
	want := []string{
		`client.secret.read 5 {"downstream":"prov"}`,
		// 変更は項目名だけを残す（値は秘密の値を含みうる）。
		`client.update 5 {"downstream":"prov","fields":["name","secret"]}`,
		`client.delete 5 {"downstream":"prov"}`,
		`policy.create 001010000000001 {"downstream":"prov"}`,
		`policy.update 001010000000001 {"downstream":"prov"}`,
		`policy.delete 001010000000001 {"downstream":"prov"}`,
		`policy.suspend 001010000000001 {"downstream":"prov"}`,
		`policy.suspend 001010000000001 {"downstream":"prov"}`,
		`policy.resume 001010000000001 {"downstream":"prov"}`,
		`policy.status.update 001010000000001 {"downstream":"prov"}`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("audits =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Valkey への保存に失敗しても操作は成功し、ログに残す。
	e.st.auditErr = errors.New("valkey down")
	if w := e.do("GET", "/admin/v1/clients/5/secret", nil); w.Code != 200 {
		t.Errorf("status = %d", w.Code)
	}
	if !strings.Contains(string(e.logs.Bytes()), `"msg":"append audit"`) {
		t.Error("audit store error is not logged")
	}
}

func TestRelayPolicyLock(t *testing.T) {
	e := newEnv(t, false)
	release := make(chan struct{})
	entered := make(chan struct{})
	e.prov.relay = func(req downstream.RelayRequest) (downstream.RelayResponse, error) {
		if req.Method == http.MethodPut {
			close(entered)
			<-release
		}
		return downstream.RelayResponse{Status: 200, ContentType: "application/json", Body: []byte(`{}`)}, nil
	}
	var wg sync.WaitGroup
	wg.Go(func() { e.doBody("PUT", "/admin/v1/policies/001010000000001", nil, `{}`) })
	<-entered

	// 同じ IMSI の書き込みは 409。読み取りと別の IMSI は通る。
	if p := decode[problem](t, e.do("DELETE", "/admin/v1/policies/001010000000001", nil), 409); p.Cause != "OPERATION_IN_PROGRESS" {
		t.Errorf("same imsi = %+v", p)
	}
	if p := decode[problem](t, e.doBody("PUT", "/admin/v1/policies/001010000000001/status", nil, `{"status":"suspended"}`), 409); p.Cause != "OPERATION_IN_PROGRESS" {
		t.Errorf("same imsi (status) = %+v", p)
	}
	if w := e.do("GET", "/admin/v1/policies/001010000000001", nil); w.Code != 200 {
		t.Errorf("read = %d", w.Code)
	}
	if w := e.do("DELETE", "/admin/v1/policies/001010000000002", nil); w.Code != 200 {
		t.Errorf("other imsi = %d", w.Code)
	}
	close(release)
	wg.Wait()
	// 終われば解放される。
	if w := e.do("DELETE", "/admin/v1/policies/001010000000001", nil); w.Code != 200 {
		t.Errorf("after release = %d", w.Code)
	}
}

func TestIdempotency(t *testing.T) {
	e := newEnv(t, false)
	e.prov.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{Status: 201, ContentType: "application/json", Location: "/admin/v1/clients/9", Body: []byte(`{"id":9}`)}, nil
	}
	hdr := map[string]string{"Idempotency-Key": "key-1"}
	first := e.doBody("POST", "/admin/v1/clients", hdr, `{"ip":"198.51.100.9"}`)
	again := e.doBody("POST", "/admin/v1/clients", hdr, `{"ip":"198.51.100.9"}`)
	if first.Code != 201 || first.Header().Get("Idempotent-Replayed") != "" {
		t.Errorf("first = %d %v", first.Code, first.Header())
	}
	// 同じ要求には最初の応答を返し、下流は呼ばない。監査ログも増えない。
	if again.Code != 201 || again.Header().Get("Idempotent-Replayed") != "true" || again.Header().Get("Location") != "/admin/v1/clients/9" ||
		again.Body.String() != first.Body.String() || e.prov.calls() != 1 || len(e.st.auditEntries()) != 1 {
		t.Errorf("again = %d %v %s (calls %d)", again.Code, again.Header(), again.Body, e.prov.calls())
	}
	// 同じキーで内容が違えば 422。
	if p := decode[problem](t, e.doBody("POST", "/admin/v1/clients", hdr, `{"ip":"198.51.100.10"}`), 422); p.Cause != "IDEMPOTENCY_KEY_MISMATCH" {
		t.Errorf("mismatch = %+v", p)
	}
	if p := decode[problem](t, e.do("DELETE", "/admin/v1/clients/9", hdr), 422); p.Cause != "IDEMPOTENCY_KEY_MISMATCH" {
		t.Errorf("other path = %+v", p)
	}

	// 5xx は覚えず、同じキーでやり直せる。
	e.prov.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{}, errors.New("unreachable")
	}
	hdr2 := map[string]string{"Idempotency-Key": "key-2"}
	if w := e.do("DELETE", "/admin/v1/clients/3", hdr2); w.Code != 503 {
		t.Fatalf("503 = %d", w.Code)
	}
	e.prov.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{Status: 204}, nil
	}
	if w := e.do("DELETE", "/admin/v1/clients/3", hdr2); w.Code != 204 || w.Header().Get("Idempotent-Replayed") != "" {
		t.Errorf("retry after 503 = %d %v", w.Code, w.Header())
	}
	if w := e.do("DELETE", "/admin/v1/clients/3", hdr2); w.Code != 204 || w.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay 204 = %d %v", w.Code, w.Header())
	}

	// 4xx は覚える（同じ要求は同じ結果になる）。ただし処理中の 409 は覚えない。
	e.prov.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return problemResp(404, "CLIENT_NOT_FOUND"), nil
	}
	hdr3 := map[string]string{"Idempotency-Key": "key-3"}
	e.do("DELETE", "/admin/v1/clients/4", hdr3)
	if w := e.do("DELETE", "/admin/v1/clients/4", hdr3); w.Code != 404 || w.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay 404 = %d %v", w.Code, w.Header())
	}
	e.st.locks["001010000000001"] = "held"
	hdr4 := map[string]string{"Idempotency-Key": "key-4"}
	if w := e.do("DELETE", "/admin/v1/policies/001010000000001", hdr4); w.Code != 409 {
		t.Fatalf("locked = %d", w.Code)
	}
	delete(e.st.locks, "001010000000001")
	e.prov.relay = func(downstream.RelayRequest) (downstream.RelayResponse, error) {
		return downstream.RelayResponse{Status: 204}, nil
	}
	if w := e.do("DELETE", "/admin/v1/policies/001010000000001", hdr4); w.Code != 204 {
		t.Errorf("after lock released = %d", w.Code)
	}

	// 最初の要求が処理中なら 409。
	e.doBody("POST", "/admin/v1/clients", map[string]string{"Idempotency-Key": "key-6"}, `{}`)
	e.st.idem["bff-01:key-6"].done = false // 最初の要求がまだ処理中の状態にする
	if p := decode[problem](t, e.doBody("POST", "/admin/v1/clients", map[string]string{"Idempotency-Key": "key-6"}, `{}`), 409); p.Cause != "OPERATION_IN_PROGRESS" {
		t.Errorf("pending = %+v", p)
	}

	// キーの形式が違えば 400。読み取りではキーを使わない。
	if p := decode[problem](t, e.doBody("POST", "/admin/v1/clients", map[string]string{"Idempotency-Key": "has space"}, `{}`), 400); p.Cause != "OPTIONAL_IE_INCORRECT" ||
		p.InvalidParams[0].Param != "Idempotency-Key" {
		t.Errorf("bad key = %+v", p)
	}
	before := e.prov.calls()
	e.do("GET", "/admin/v1/clients", map[string]string{"Idempotency-Key": "key-1"})
	e.do("GET", "/admin/v1/clients", map[string]string{"Idempotency-Key": "key-1"})
	if e.prov.calls() != before+2 {
		t.Error("GET used the idempotency key")
	}
}

func TestRelayBodyLimit(t *testing.T) {
	e := newEnv(t, false)
	big := `{"x":"` + strings.Repeat("a", maxBodyBytes) + `"}`
	for _, hdr := range []map[string]string{nil, {"Idempotency-Key": "k"}} {
		if p := decode[problem](t, e.doBody("POST", "/admin/v1/clients", hdr, big), 400); p.Cause != "INVALID_MSG_FORMAT" {
			t.Errorf("%v: %+v", hdr, p)
		}
	}
	if e.prov.calls() != 0 {
		t.Error("relayed a too large body")
	}
}

func TestRelayAka(t *testing.T) {
	// aka を扱わない設定。
	e := newEnv(t, false)
	for _, path := range []string{"/admin/v1/aka/audit-logs", "/admin/v1/aka/av-clients/1"} {
		if p := decode[problem](t, e.do("GET", path, nil), 404); p.Cause != "DOWNSTREAM_NOT_CONFIGURED" {
			t.Errorf("%s: %+v", path, p)
		}
	}

	e = newEnv(t, true)
	e.aka.relay = func(req downstream.RelayRequest) (downstream.RelayResponse, error) {
		if req.Path[0] == "clients" {
			return problemResp(404, "CLIENT_NOT_FOUND"), nil
		}
		return downstream.RelayResponse{Status: 200, ContentType: "application/json", Body: []byte(`{"items":[]}`)}, nil
	}
	if w := e.do("GET", "/admin/v1/aka/audit-logs?limit=5", nil); w.Code != 200 || e.aka.requests[0].RawQuery != "limit=5" ||
		!slices.Equal(e.aka.requests[0].Path, []string{"audit-logs"}) {
		t.Errorf("aka audit = %d %+v", w.Code, e.aka.requests)
	}
	if p := decode[problem](t, e.do("GET", "/admin/v1/aka/av-clients/3", nil), 404); p.Cause != "CLIENT_NOT_FOUND" || p.Downstream != "aka" ||
		!slices.Equal(e.aka.requests[1].Path, []string{"clients", "3"}) {
		t.Errorf("aka av client = %+v %+v", p, e.aka.requests)
	}
	// 書き込みのメソッドはない。
	if w := e.do("DELETE", "/admin/v1/aka/av-clients/3", nil); w.Code != 405 {
		t.Errorf("delete av client = %d", w.Code)
	}
	if e.prov.calls() != 0 {
		t.Error("prov was called")
	}
}

func TestListAuditLogs(t *testing.T) {
	e := newEnv(t, false)
	for i := range 3 {
		e.do("DELETE", "/admin/v1/clients/"+itoa(i+1), map[string]string{"X-Trace-ID": "t" + itoa(i)})
	}
	type list struct {
		Items []struct {
			ID         string         `json:"id"`
			Action     string         `json:"action"`
			Target     string         `json:"target"`
			TraceID    string         `json:"traceId"`
			Result     string         `json:"result"`
			MgmtClient string         `json:"mgmtClient"`
			Details    map[string]any `json:"details"`
			Time       time.Time      `json:"time"`
		} `json:"items"`
		NextBefore string `json:"nextBefore"`
	}
	l := decode[list](t, e.do("GET", "/admin/v1/audit-logs?limit=2", nil), 200)
	if len(l.Items) != 2 || l.Items[0].Target != "3" || l.Items[0].TraceID != "t2" || l.Items[0].Result != "completed" ||
		l.Items[0].MgmtClient != "bff-01" || l.Items[0].Details["downstream"] != "prov" || l.Items[0].Time.IsZero() || l.NextBefore == "" {
		t.Fatalf("page 1 = %+v", l)
	}
	l = decode[list](t, e.do("GET", "/admin/v1/audit-logs?before="+l.NextBefore, nil), 200)
	if len(l.Items) != 1 || l.Items[0].Target != "1" || l.NextBefore != "" {
		t.Errorf("page 2 = %+v", l)
	}
	for _, q := range []string{"limit=0", "limit=501", "limit=x", "before=abc"} {
		if p := decode[problem](t, e.do("GET", "/admin/v1/audit-logs?"+q, nil), 400); p.Cause != "INVALID_QUERY_PARAM" {
			t.Errorf("%s: %+v", q, p)
		}
	}
}

func TestRewriteLocation(t *testing.T) {
	for _, c := range []struct{ loc, base, want string }{
		{"/admin/v1/clients/7", "https://provisioning-api:9444/admin/v1", "/admin/v1/clients/7"},
		{"/api/admin/v1/policies/001010000000001", "https://10.0.0.1:9444/api/admin/v1/", "/admin/v1/policies/001010000000001"},
		{"/other/clients/7", "https://provisioning-api:9444/admin/v1", ""},
		{"", "https://provisioning-api:9444/admin/v1", ""},
	} {
		if got := rewriteLocation(c.loc, c.base); got != c.want {
			t.Errorf("rewriteLocation(%q, %q) = %q, want %q", c.loc, c.base, got, c.want)
		}
	}
}
