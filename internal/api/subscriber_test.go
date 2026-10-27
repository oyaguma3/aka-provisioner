package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/plmn"
	"github.com/oyaguma3/aka-provisioner/internal/provapi"
	"github.com/oyaguma3/aka-provisioner/internal/store"
	"github.com/oyaguma3/aka-provisioner/internal/subscriber"
)

// fakeSubs は加入者の統合操作の偽物。受け取った入力を記録し、決めた結果を返す。
type fakeSubs struct {
	mu      sync.Mutex
	err     error
	sub     subscriber.Subscriber
	list    subscriber.List
	res     subscriber.Result
	create  *subscriber.CreateInput
	update  *subscriber.UpdateInput
	actor   subscriber.Actor
	listArg downstream.ListParams
	calls   int

	ops       []store.Operation
	op        store.Operation
	opArgs    []any
	counts    [3]int
	countsErr error
	// unresolved は CheckUnresolved が返すエラー（ポリシーの PUT・DELETE）。
	unresolved error
}

func (f *fakeSubs) Get(context.Context, string) (subscriber.Subscriber, error) { return f.sub, f.err }
func (f *fakeSubs) List(_ context.Context, p downstream.ListParams) (subscriber.List, error) {
	f.listArg = p
	return f.list, f.err
}
func (f *fakeSubs) Keys(context.Context, string) (string, string, error) {
	return "465b5ce8b199b49faa5f0a2ee238a6bc", "cd63cb71954a9f4e48a5994e37a02baf", f.err
}
func (f *fakeSubs) Create(_ context.Context, a subscriber.Actor, in subscriber.CreateInput) (subscriber.Subscriber, subscriber.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.create, f.actor = &in, a
	return f.sub, f.res, f.err
}
func (f *fakeSubs) Update(_ context.Context, a subscriber.Actor, _ string, in subscriber.UpdateInput) (subscriber.Subscriber, subscriber.Result, error) {
	f.calls++
	f.update, f.actor = &in, a
	return f.sub, f.res, f.err
}
func (f *fakeSubs) Delete(_ context.Context, a subscriber.Actor, _ string) (subscriber.Result, error) {
	f.calls++
	f.actor = a
	return f.res, f.err
}

func (f *fakeSubs) Operations(_ context.Context, status string, limit int) ([]store.Operation, int, error) {
	f.opArgs = []any{status, limit}
	return f.ops, len(f.ops), f.err
}
func (f *fakeSubs) Operation(context.Context, string) (store.Operation, error) { return f.op, f.err }
func (f *fakeSubs) Retry(_ context.Context, a subscriber.Actor, _ string) (store.Operation, error) {
	f.calls++
	f.actor = a
	return f.op, f.err
}
func (f *fakeSubs) Dismiss(_ context.Context, a subscriber.Actor, _ string) (store.Operation, error) {
	f.calls++
	f.actor = a
	return f.op, f.err
}
func (f *fakeSubs) CheckUnresolved(context.Context, string) error { return f.unresolved }
func (f *fakeSubs) OperationCounts(context.Context) (map[string]int, error) {
	return map[string]int{"running": f.counts[0], "retrying": f.counts[1], "failed": f.counts[2]}, f.countsErr
}

var completedRes = subscriber.Result{OperationID: "op-1", Status: store.OpCompleted, Steps: []store.OperationStep{
	{Name: "subscriber.create", Downstream: "prov", State: "done"}, {Name: "policy.put", Downstream: "prov", State: "done"},
}}

const validCreate = `{"imsi":"001010000000001","ki":"465B5CE8B199B49FAA5F0A2EE238A6BC","opc":"cd63cb71954a9f4e48a5994e37a02baf",
	"amf":"b9b9","policy":{"default":"DENY","rules":[{"nasId":" AP-01 ","allowedSsids":[" CORP "],"vlanId":"100","sessionTimeout":3600}]}}`

var jsonHeader = map[string]string{"Content-Type": "application/json", "X-Operator-Id": "alice", "X-Trace-ID": "trace-s1"}

func TestCreateSubscriber(t *testing.T) {
	e := newEnv(t, true)
	e.subs.res = completedRes
	e.subs.sub = subscriber.Subscriber{IMSI: "001010000000001", KeyStore: plmn.KeyStorePoC,
		Key:    &subscriber.Key{AMF: "b9b9", SQN: "000000000000"},
		Policy: &provapi.PolicyPut{Default: "deny"}, Status: "active", Issues: []subscriber.Issue{}}
	w := e.doBody("POST", "/admin/v1/subscribers", jsonHeader, validCreate)
	if w.Code != 201 || w.Header().Get("Location") != "/admin/v1/subscribers/001010000000001" {
		t.Fatalf("response = %d %s", w.Code, w.Body)
	}
	// 応答: aka だけの項目は出さない。ポリシーの rules は空でも配列。状態はポリシーの外に出す。
	if got := w.Body.String(); got != `{"imsi":"001010000000001","keyStore":"poc","key":{"amf":"b9b9","sqn":"000000000000"},"policy":{"default":"deny","rules":[]},"status":"active","issues":[]}` {
		t.Errorf("body = %s", got)
	}
	// 入力は本PoCと同じく正規化して渡す（default は小文字、nasId と SSID は前後の空白を除く）。
	in := e.subs.create
	want := provapi.PolicyPut{Default: "deny", Rules: []provapi.PolicyRule{{NASID: "AP-01", AllowedSSIDs: []string{"CORP"}, VLANID: "100", SessionTimeout: 3600}}}
	if in.IMSI != "001010000000001" || in.Ki != "465B5CE8B199B49FAA5F0A2EE238A6BC" || in.AMF != "b9b9" || in.SQN != "" ||
		in.AllowPlain != nil || !policyEqual(in.Policy, want) {
		t.Errorf("input = %+v", in)
	}
	if e.subs.actor != (subscriber.Actor{Operator: "alice", MgmtClient: "bff-01", TraceID: "trace-s1"}) {
		t.Errorf("actor = %+v", e.subs.actor)
	}
	// 監査ログ: 操作の記録の ID と手順。Ki / OPc は残さない。
	audits := e.st.auditEntries()
	if len(audits) != 1 || audits[0].Action != "subscriber.create" || audits[0].Target != "001010000000001" || audits[0].OperationID != "op-1" ||
		audits[0].Result != "completed" ||
		audits[0].Details != `{"keyStore":"poc","steps":[{"downstream":"prov","name":"subscriber.create","state":"done"},{"downstream":"prov","name":"policy.put","state":"done"}]}` {
		t.Errorf("audits = %+v", audits)
	}
	if strings.Contains(string(e.logs.Bytes()), "465B5CE8") {
		t.Error("ki is logged")
	}

	// aka の加入者には aka だけの項目を指定できる。keyStore を指定すれば PLMN マップと照合する。
	body := `{"imsi":"001020000000001","keyStore":"aka","ki":"465b5ce8b199b49faa5f0a2ee238a6bc","opc":"cd63cb71954a9f4e48a5994e37a02baf",
		"sqnType":"inc33","allowPlain":true,"policy":{"default":"allow","rules":[]}}`
	if w := e.doBody("POST", "/admin/v1/subscribers", jsonHeader, body); w.Code != 201 || e.subs.create.SQNType != "inc33" ||
		e.subs.create.AllowPlain == nil || !*e.subs.create.AllowPlain {
		t.Errorf("aka = %d %s, input %+v", w.Code, w.Body, e.subs.create)
	}
}

func policyEqual(a, b provapi.PolicyPut) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestCreateSubscriberValidation(t *testing.T) {
	for name, c := range map[string]struct {
		body, contentType, cause string
		params                   []string
	}{
		"empty": {`{}`, "", "MANDATORY_IE_MISSING", []string{"imsi", "ki", "opc", "policy"}},
		"bad mandatory": {`{"imsi":"0010100000000","ki":"xyz","opc":"cd63cb71954a9f4e48a5994e37a02baf","policy":{"default":"maybe","rules":[]}}`,
			"", "MANDATORY_IE_INCORRECT", []string{"imsi", "ki", "policy.default"}},
		"bad optional": {`{"imsi":"001010000000001","ki":"465b5ce8b199b49faa5f0a2ee238a6bc","opc":"cd63cb71954a9f4e48a5994e37a02baf",
			"amf":"80000","sqn":"1","keyStore":"x","policy":{"default":"deny","rules":[{"nasId":"*","allowedSsids":["a"],"vlanId":"+5","sessionTimeout":90000}]}}`,
			"", "OPTIONAL_IE_INCORRECT", []string{"amf", "sqn", "keyStore", "policy.rules[0].vlanId", "policy.rules[0].sessionTimeout"}},
		"rule errors": {`{"imsi":"001010000000001","ki":"465b5ce8b199b49faa5f0a2ee238a6bc","opc":"cd63cb71954a9f4e48a5994e37a02baf",
			"policy":{"default":"deny","rules":[{"allowedSsids":[]},{"nasId":"*","allowedSsids":["ok",""," "]}]}}`,
			"", "MANDATORY_IE_MISSING", []string{"policy.rules[0].nasId", "policy.rules[0].allowedSsids", "policy.rules[1].allowedSsids[1]", "policy.rules[1].allowedSsids[2]"}},
		"policy rules missing": {`{"imsi":"001010000000001","ki":"465b5ce8b199b49faa5f0a2ee238a6bc","opc":"cd63cb71954a9f4e48a5994e37a02baf","policy":{"default":"deny"}}`,
			"", "MANDATORY_IE_MISSING", []string{"policy.rules"}},
		"aka only fields for poc": {`{"imsi":"001010000000001","ki":"465b5ce8b199b49faa5f0a2ee238a6bc","opc":"cd63cb71954a9f4e48a5994e37a02baf",
			"sqnType":"inc32","allowPlain":false,"policy":{"default":"deny","rules":[]}}`, "", "OPTIONAL_IE_INCORRECT", []string{"sqnType", "allowPlain"}},
		"key store mismatch": {`{"imsi":"001010000000001","keyStore":"aka","ki":"465b5ce8b199b49faa5f0a2ee238a6bc","opc":"cd63cb71954a9f4e48a5994e37a02baf",
			"policy":{"default":"deny","rules":[]}}`, "", "KEY_STORE_MISMATCH", []string{"keyStore"}},
		"unknown field":      {`{"imsi":"001010000000001","extra":1}`, "", "INVALID_MSG_FORMAT", nil},
		"unknown rule field": {`{"policy":{"default":"deny","rules":[{"nasId":"*","allowedSsids":["a"],"x":1}]}}`, "", "INVALID_MSG_FORMAT", nil},
		// 状態は作成では指定できない（常に active。変更は PUT /policies/{imsi}/status）。
		"policy status": {`{"imsi":"001010000000001","ki":"465b5ce8b199b49faa5f0a2ee238a6bc","opc":"cd63cb71954a9f4e48a5994e37a02baf",
			"policy":{"default":"deny","rules":[],"status":"suspended"}}`, "", "INVALID_MSG_FORMAT", nil},
		"status": {`{"imsi":"001010000000001","ki":"465b5ce8b199b49faa5f0a2ee238a6bc","opc":"cd63cb71954a9f4e48a5994e37a02baf",
			"policy":{"default":"deny","rules":[]},"status":"suspended"}`, "", "INVALID_MSG_FORMAT", nil},
		"wrong type":          {`{"imsi":1}`, "", "INVALID_MSG_FORMAT", nil},
		"trailing data":       {`{} {}`, "", "INVALID_MSG_FORMAT", nil},
		"not json":            {`imsi=1`, "", "INVALID_MSG_FORMAT", nil},
		"wrong content type":  {validCreate, "text/plain", "INVALID_MSG_FORMAT", nil},
		"merge patch content": {validCreate, "application/merge-patch+json", "INVALID_MSG_FORMAT", nil},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, true)
			hdr := map[string]string{"Content-Type": "application/json; charset=utf-8"}
			if c.contentType != "" {
				hdr["Content-Type"] = c.contentType
			}
			p := decode[problem](t, e.doBody("POST", "/admin/v1/subscribers", hdr, c.body), 400)
			var params []string
			for _, ip := range p.InvalidParams {
				params = append(params, ip.Param)
			}
			if p.Cause != c.cause || !slices.Equal(params, c.params) {
				t.Errorf("problem = %s %v", p.Cause, p.InvalidParams)
			}
			if e.subs.calls != 0 || len(e.st.auditEntries()) != 0 {
				t.Error("service was called or audited")
			}
		})
	}
}

func TestSubscriberErrors(t *testing.T) {
	dsErr := func(name downstream.Name, prefix string, err error) error {
		return &subscriber.DownstreamError{Name: name, ParamPrefix: prefix, Err: err}
	}
	apiErr := func(status int, cause string, params ...downstream.InvalidParam) error {
		return &downstream.Error{Status: status, Problem: downstream.Problem{Cause: cause, InvalidParams: params}}
	}
	for name, c := range map[string]struct {
		err  error
		res  subscriber.Result
		want string // problem の主な項目を JSON にしたもの
	}{
		"locked":    {store.ErrLocked, subscriber.Result{}, `409 OPERATION_IN_PROGRESS`},
		"not found": {subscriber.ErrNotFound, subscriber.Result{}, `404 USER_NOT_FOUND`},
		"conflict":  {&subscriber.ConflictError{Places: []string{"poc", "policy"}}, subscriber.Result{}, `409 SUBSCRIBER_ALREADY_EXISTS conflicts=[poc policy]`},
		"unavailable": {dsErr(downstream.Aka, "", errors.New("dial tcp: timeout")), subscriber.Result{},
			`503 DOWNSTREAM_UNAVAILABLE downstream=aka`},
		"input error leaked to downstream": {dsErr(downstream.Prov, "policy.", apiErr(400, "MANDATORY_IE_INCORRECT",
			downstream.InvalidParam{Param: "rules[0].nasId", Reason: "x"})), subscriber.Result{},
			`400 MANDATORY_IE_INCORRECT downstream=prov params=[policy.rules[0].nasId]`},
		"av client not found": {dsErr(downstream.Aka, "", apiErr(400, "CLIENT_NOT_FOUND")), subscriber.Result{},
			`502 DOWNSTREAM_ERROR downstream=aka status=400 cause=CLIENT_NOT_FOUND`},
		"server error": {dsErr(downstream.Prov, "", apiErr(500, "SYSTEM_FAILURE")), subscriber.Result{},
			`502 DOWNSTREAM_ERROR downstream=prov status=500 cause=SYSTEM_FAILURE`},
		"created outside": {dsErr(downstream.Aka, "", apiErr(409, "SUBSCRIBER_ALREADY_EXISTS")), subscriber.Result{},
			`409 SUBSCRIBER_ALREADY_EXISTS downstream=aka conflicts=[aka]`},
		"rolled back": {&subscriber.OperationError{OperationID: "op-9", RolledBack: true,
			Err: dsErr(downstream.Prov, "", errors.New("timeout"))}, subscriber.Result{OperationID: "op-9", Status: "rolled_back"},
			`503 DOWNSTREAM_UNAVAILABLE downstream=prov op=op-9 rolledBack=true`},
		"incomplete": {&subscriber.OperationError{OperationID: "op-9", Incomplete: true,
			Err: dsErr(downstream.Aka, "", apiErr(500, "SYSTEM_FAILURE"))}, subscriber.Result{OperationID: "op-9", Status: "retrying"},
			`500 OPERATION_INCOMPLETE downstream=aka status=500 cause=SYSTEM_FAILURE op=op-9 rolledBack=false`},
		"store error after writes": {&subscriber.OperationError{OperationID: "op-9", RolledBack: true, Err: errors.New("valkey down")},
			subscriber.Result{OperationID: "op-9", Status: "rolled_back"}, `500 SYSTEM_FAILURE op=op-9 rolledBack=true`},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, true)
			e.subs.err, e.subs.res = c.err, c.res
			w := e.do("DELETE", "/admin/v1/subscribers/001010000000001", nil)
			var p problem
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			if got := summarize(p); got != c.want || w.Code != p.Status {
				t.Errorf("got %q (status %d), want %q", got, w.Code, c.want)
			}
			// 操作の記録があれば、失敗でも監査ログに残す。
			audits := e.st.auditEntries()
			if (len(audits) == 1) != (c.res.OperationID != "") || (len(audits) == 1 && audits[0].Result != c.res.Status) {
				t.Errorf("audits = %+v", audits)
			}
		})
	}
}

func summarize(p problem) string {
	s := strings.TrimSpace(strings.Join([]string{itoa(p.Status), p.Cause}, " "))
	if p.Downstream != "" {
		s += " downstream=" + p.Downstream
	}
	if p.DownstreamStatus != 0 {
		s += " status=" + itoa(p.DownstreamStatus)
	}
	if p.DownstreamCause != "" {
		s += " cause=" + p.DownstreamCause
	}
	if len(p.InvalidParams) > 0 {
		var ps []string
		for _, ip := range p.InvalidParams {
			ps = append(ps, ip.Param)
		}
		s += " params=[" + strings.Join(ps, " ") + "]"
	}
	if len(p.Conflicts) > 0 {
		s += " conflicts=[" + strings.Join(p.Conflicts, " ") + "]"
	}
	if p.OperationID != "" {
		s += " op=" + p.OperationID
	}
	if p.RolledBack != nil {
		s += " rolledBack=" + map[bool]string{true: "true", false: "false"}[*p.RolledBack]
	}
	return s
}

func TestUpdateSubscriber(t *testing.T) {
	e := newEnv(t, true)
	e.subs.res = subscriber.Result{OperationID: "op-2", Status: store.OpCompleted}
	e.subs.sub = subscriber.Subscriber{IMSI: "001020000000001", KeyStore: plmn.KeyStoreAKA,
		Key: &subscriber.Key{AMF: "8000", SQN: "000000000000", SQNType: "inc32", AllowPlain: new(true), AllowedClientIDs: []int64{}}}
	hdr := map[string]string{"Content-Type": "application/merge-patch+json"}
	w := e.doBody("PATCH", "/admin/v1/subscribers/001020000000001", hdr, `{"allowPlain":true,"policy":{"default":"allow","rules":[]},"sqn":"00000000001F"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"allowedClientIds":[]`) || !strings.Contains(w.Body.String(), `"issues":[]`) {
		t.Fatalf("response = %d %s", w.Code, w.Body)
	}
	in := e.subs.update
	if in.AllowPlain == nil || !*in.AllowPlain || in.SQN == nil || *in.SQN != "00000000001F" || in.Policy == nil || in.Policy.Default != "allow" ||
		in.Ki != nil || in.AMF != nil {
		t.Errorf("input = %+v", in)
	}
	// 監査ログには変えた項目名を残す。
	if a := e.st.auditEntries(); len(a) != 1 || !strings.Contains(a[0].Details, `"fields":["allowPlain","policy","sqn"]`) {
		t.Errorf("audits = %+v", a)
	}

	for name, c := range map[string]struct {
		imsi, body, contentType, cause string
		params                         []string
	}{
		"empty":          {"001020000000001", `{}`, "", "MANDATORY_IE_MISSING", nil},
		"null":           {"001020000000001", `{"amf":null,"ki":"x"}`, "", "OPTIONAL_IE_INCORRECT", []string{"amf", "ki"}},
		"unknown":        {"001020000000001", `{"keyStore":"poc"}`, "", "INVALID_MSG_FORMAT", nil},
		"wrong type":     {"001020000000001", `{"allowPlain":"yes"}`, "", "INVALID_MSG_FORMAT", nil},
		"aka only (poc)": {"001010000000001", `{"sqnType":"inc1"}`, "", "OPTIONAL_IE_INCORRECT", []string{"sqnType"}},
		"bad sqn type":   {"001020000000001", `{"sqnType":"inc2"}`, "", "OPTIONAL_IE_INCORRECT", []string{"sqnType"}},
		"policy rules":   {"001020000000001", `{"policy":{"default":"allow"}}`, "", "MANDATORY_IE_MISSING", []string{"policy.rules"}},
		"policy status":  {"001020000000001", `{"policy":{"default":"allow","rules":[],"status":"active"}}`, "", "INVALID_MSG_FORMAT", nil},
		"status":         {"001020000000001", `{"status":"suspended"}`, "", "INVALID_MSG_FORMAT", nil},
		"content type":   {"001020000000001", `{"amf":"8000"}`, "text/plain", "INVALID_MSG_FORMAT", nil},
		"bad imsi":       {"00102000000000x", `{"amf":"8000"}`, "", "MANDATORY_IE_INCORRECT", []string{"imsi"}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, true)
			hdr := map[string]string{"Content-Type": "application/merge-patch+json"}
			if c.contentType != "" {
				hdr["Content-Type"] = c.contentType
			}
			p := decode[problem](t, e.doBody("PATCH", "/admin/v1/subscribers/"+c.imsi, hdr, c.body), 400)
			var params []string
			for _, ip := range p.InvalidParams {
				params = append(params, ip.Param)
			}
			slices.Sort(params)
			if p.Cause != c.cause || !slices.Equal(params, c.params) || e.subs.calls != 0 {
				t.Errorf("problem = %s %v (calls %d)", p.Cause, p.InvalidParams, e.subs.calls)
			}
		})
	}
	// application/json も受け付ける。
	e = newEnv(t, true)
	if w := e.doBody("PATCH", "/admin/v1/subscribers/001010000000001", map[string]string{"Content-Type": "application/json"}, `{"amf":"8000"}`); w.Code != 200 {
		t.Errorf("application/json = %d %s", w.Code, w.Body)
	}
}

func TestDeleteAndKeys(t *testing.T) {
	e := newEnv(t, true)
	e.subs.res = subscriber.Result{OperationID: "op-3", Status: store.OpCompleted}
	if w := e.do("DELETE", "/admin/v1/subscribers/001010000000001", map[string]string{"X-Operator-Id": "bob"}); w.Code != 204 || w.Body.Len() != 0 {
		t.Errorf("delete = %d %s", w.Code, w.Body)
	}
	w := e.do("GET", "/admin/v1/subscribers/001020000000001/keys", nil)
	if w.Code != 200 || w.Body.String() != `{"ki":"465b5ce8b199b49faa5f0a2ee238a6bc","opc":"cd63cb71954a9f4e48a5994e37a02baf"}` {
		t.Errorf("keys = %d %s", w.Code, w.Body)
	}
	a := e.st.auditEntries()
	if len(a) != 2 || a[0].Action != "subscriber.delete" || a[0].Operator != "bob" || a[1].Action != "subscriber.keys.read" ||
		a[1].Details != `{"keyStore":"aka"}` || strings.Contains(a[1].Details, "465b") {
		t.Errorf("audits = %+v", a)
	}
	// 鍵がなければ 404 で、監査ログには残さない。
	e.subs.err = subscriber.ErrNotFound
	if p := decode[problem](t, e.do("GET", "/admin/v1/subscribers/001020000000001/keys", nil), 404); p.Cause != "USER_NOT_FOUND" || len(e.st.auditEntries()) != 2 {
		t.Errorf("not found = %+v", p)
	}
}

func TestGetAndListSubscribers(t *testing.T) {
	e := newEnv(t, true)
	e.subs.sub = subscriber.Subscriber{IMSI: "001020000000001", KeyStore: plmn.KeyStoreAKA,
		Issues: []subscriber.Issue{subscriber.IssueKeyMissing}, Policy: &provapi.PolicyPut{Default: "deny", Rules: []provapi.PolicyRule{}}, Status: "suspended"}
	if w := e.do("GET", "/admin/v1/subscribers/001020000000001", nil); w.Code != 200 ||
		w.Body.String() != `{"imsi":"001020000000001","keyStore":"aka","policy":{"default":"deny","rules":[]},"status":"suspended","issues":["KEY_MISSING"]}` {
		t.Errorf("get = %d %s", w.Code, w.Body)
	}

	e.subs.list = subscriber.List{Items: []subscriber.Subscriber{e.subs.sub}, NextCursor: "001020000000001"}
	w := e.do("GET", "/admin/v1/subscribers?prefix=00102&cursor=001010000000009&limit=1", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"nextCursor":"001020000000001"`) ||
		e.subs.listArg != (downstream.ListParams{Prefix: "00102", Cursor: "001010000000009", Limit: 1}) {
		t.Errorf("list = %d %s (%+v)", w.Code, w.Body, e.subs.listArg)
	}
	e.do("GET", "/admin/v1/subscribers", nil)
	if e.subs.listArg.Limit != 50 {
		t.Errorf("default limit = %d", e.subs.listArg.Limit)
	}
	for _, q := range []string{"prefix=abc", "prefix=0010100000000011", "cursor=x", "limit=0", "limit=501"} {
		if p := decode[problem](t, e.do("GET", "/admin/v1/subscribers?"+q, nil), 400); p.Cause != "INVALID_QUERY_PARAM" {
			t.Errorf("%s: %+v", q, p)
		}
	}
}

func TestCreateSubscriberIdempotent(t *testing.T) {
	e := newEnv(t, true)
	e.subs.res = completedRes
	e.subs.sub = subscriber.Subscriber{IMSI: "001010000000001", KeyStore: plmn.KeyStorePoC, Issues: []subscriber.Issue{}}
	hdr := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "create-1"}
	first := e.doBody("POST", "/admin/v1/subscribers", hdr, validCreate)
	again := e.doBody("POST", "/admin/v1/subscribers", hdr, validCreate)
	if first.Code != 201 || again.Code != 201 || again.Header().Get("Idempotent-Replayed") != "true" || e.subs.calls != 1 ||
		again.Header().Get("Location") != "/admin/v1/subscribers/001010000000001" {
		t.Errorf("first %d, again %d %v (calls %d)", first.Code, again.Code, again.Header(), e.subs.calls)
	}
	// 処理中（同じ IMSI の操作）の 409 は覚えない。
	e.subs.err = store.ErrLocked
	hdr["Idempotency-Key"] = "create-2"
	e.doBody("POST", "/admin/v1/subscribers", hdr, validCreate)
	e.subs.err = nil
	if w := e.doBody("POST", "/admin/v1/subscribers", hdr, validCreate); w.Code != 201 || w.Header().Get("Idempotent-Replayed") != "" {
		t.Errorf("after locked = %d %v", w.Code, w.Header())
	}
}

func TestUnresolvedOperation(t *testing.T) {
	e := newEnv(t, true)
	unresolved := &subscriber.UnresolvedError{OperationID: "0199c8a2-0000-7000-8000-000000000001", Status: store.OpFailed}
	hdr := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "create-u"}

	// 加入者の作成・変更・削除: 409 OPERATION_UNRESOLVED と operationId。
	e.subs.err = unresolved
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/admin/v1/subscribers", validCreate},
		{"PATCH", "/admin/v1/subscribers/001010000000001", `{"amf":"8000"}`},
		{"DELETE", "/admin/v1/subscribers/001010000000001", ""},
	} {
		p := decode[problem](t, e.doBody(c.method, c.path, hdr, c.body), 409)
		if p.Cause != "OPERATION_UNRESOLVED" || p.OperationID != unresolved.OperationID || !strings.Contains(p.Detail, "/operations/"+unresolved.OperationID) {
			t.Errorf("%s %s = %+v", c.method, c.path, p)
		}
		hdr["Idempotency-Key"] += "x"
	}
	// Idempotency-Key では覚えない（片付けた後に同じキーで送り直せる）。
	hdr["Idempotency-Key"] = "create-u"
	e.subs.err, e.subs.res = nil, completedRes
	if w := e.doBody("POST", "/admin/v1/subscribers", hdr, validCreate); w.Code != 201 || w.Header().Get("Idempotent-Replayed") != "" {
		t.Errorf("after resolved = %d %s", w.Code, w.Body)
	}

	// ポリシーの PUT・DELETE と停止・再開も断り、下流を呼ばない。取得は通る。
	e.subs.unresolved = unresolved
	before := e.prov.calls()
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/admin/v1/policies/001010000000001", `{"default":"allow","rules":[]}`},
		{"DELETE", "/admin/v1/policies/001010000000001", ""},
		{"PUT", "/admin/v1/policies/001010000000001/status", `{"status":"suspended"}`},
	} {
		p := decode[problem](t, e.doBody(c.method, c.path, jsonHeader, c.body), 409)
		if p.Cause != "OPERATION_UNRESOLVED" || p.OperationID != unresolved.OperationID {
			t.Errorf("%s %s = %+v", c.method, c.path, p)
		}
	}
	if e.prov.calls() != before {
		t.Errorf("downstream was called: %d -> %d", before, e.prov.calls())
	}
	if w := e.do("GET", "/admin/v1/policies/001010000000001", nil); w.Code != 200 {
		t.Errorf("get policy = %d", w.Code)
	}
	// 確かめられない（Valkey のエラー）なら 500 で、下流は呼ばない。
	e.subs.unresolved = errors.New("valkey down")
	if p := decode[problem](t, e.doBody("PUT", "/admin/v1/policies/001010000000001", jsonHeader, `{}`), 500); p.Cause != "SYSTEM_FAILURE" {
		t.Errorf("store error = %+v", p)
	}
	if e.prov.calls() != before+1 {
		t.Errorf("downstream calls = %d, want %d (only the GET)", e.prov.calls(), before+1)
	}
}
