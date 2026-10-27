package api

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/store"
	"github.com/oyaguma3/aka-provisioner/internal/subscriber"
)

const opID = "0199c8a2-5b7e-7c3a-9d41-2f6e8a1b3c4d"

func sampleOp(status string) store.Operation {
	t0 := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	return store.Operation{
		ID: opID, Kind: "subscriber.create", IMSI: "001020000000001", KeyStore: "aka", Status: status, Attempts: 2,
		Steps: []store.OperationStep{
			{Name: "subscriber.create", Downstream: "aka", State: "done"},
			{Name: "policy.put", Downstream: "prov", State: "failed", Error: &store.StepError{Status: 400, Cause: "MANDATORY_IE_INCORRECT", Detail: "x", Time: t0}},
			{Name: "subscriber.compensate", Downstream: "aka", State: "failed", Error: &store.StepError{Cause: "DOWNSTREAM_UNAVAILABLE", Time: t0}},
		},
		NextAttemptAt: t0.Add(time.Minute), Operator: "alice", MgmtClient: "bff", TraceID: "t1", CreatedAt: t0, UpdatedAt: t0,
		PrevPolicy: `{"default":"deny"}`,
	}
}

func TestOperationsAPI(t *testing.T) {
	e := newEnv(t, true)
	e.subs.ops = []store.Operation{sampleOp(store.OpRetrying)}
	w := e.do("GET", "/admin/v1/operations?status=retrying&limit=5", nil)
	want := `{"items":[{"id":"` + opID + `","kind":"subscriber.create","imsi":"001020000000001","keyStore":"aka","status":"retrying",` +
		`"steps":[{"name":"subscriber.create","downstream":"aka","state":"done"},` +
		`{"name":"policy.put","downstream":"prov","state":"failed","error":{"status":400,"cause":"MANDATORY_IE_INCORRECT","detail":"x","time":"2026-10-10T00:00:00Z"}},` +
		`{"name":"subscriber.compensate","downstream":"aka","state":"failed","error":{"cause":"DOWNSTREAM_UNAVAILABLE","time":"2026-10-10T00:00:00Z"}}],` +
		`"attempts":2,"nextAttemptAt":"2026-10-10T00:01:00Z","operator":"alice","mgmtClient":"bff","traceId":"t1",` +
		`"createdAt":"2026-10-10T00:00:00Z","updatedAt":"2026-10-10T00:00:00Z"}],"total":1}`
	// 変更前のポリシーなどの内部の項目は返さない。
	if w.Code != 200 || w.Body.String() != want || !slices.Equal(e.subs.opArgs, []any{"retrying", 5}) {
		t.Errorf("list = %d %s (%v)", w.Code, w.Body, e.subs.opArgs)
	}
	e.do("GET", "/admin/v1/operations", nil)
	if !slices.Equal(e.subs.opArgs, []any{"", 100}) {
		t.Errorf("defaults = %v", e.subs.opArgs)
	}
	for _, q := range []string{"status=completed", "limit=0", "limit=501"} {
		if p := decode[problem](t, e.do("GET", "/admin/v1/operations?"+q, nil), 400); p.Cause != "INVALID_QUERY_PARAM" {
			t.Errorf("%s: %+v", q, p)
		}
	}

	// 取得。retrying 以外では nextAttemptAt を返さない。
	e.subs.op = sampleOp(store.OpFailed)
	if w := e.do("GET", "/admin/v1/operations/"+opID, nil); w.Code != 200 || strings.Contains(w.Body.String(), "nextAttemptAt") {
		t.Errorf("get = %d %s", w.Code, w.Body)
	}
	if p := decode[problem](t, e.do("GET", "/admin/v1/operations/not-a-uuid", nil), 400); p.Cause != "MANDATORY_IE_INCORRECT" {
		t.Errorf("bad id = %+v", p)
	}

	// retry / dismiss は要求者を渡す（監査ログはサービスが残す）。
	e.subs.op = sampleOp(store.OpRolledBack)
	if w := e.do("POST", "/admin/v1/operations/"+opID+"/retry", map[string]string{"X-Operator-Id": "bob"}); w.Code != 200 ||
		!strings.Contains(w.Body.String(), `"status":"rolled_back"`) || e.subs.actor.Operator != "bob" || e.subs.actor.MgmtClient != "bff-01" {
		t.Errorf("retry = %d %s %+v", w.Code, w.Body, e.subs.actor)
	}
	e.subs.op = sampleOp(store.OpDismissed)
	if w := e.do("POST", "/admin/v1/operations/"+opID+"/dismiss", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"dismissed"`) {
		t.Errorf("dismiss = %d %s", w.Code, w.Body)
	}
}

func TestOperationErrors(t *testing.T) {
	for name, c := range map[string]struct {
		err    error
		status int
		cause  string
	}{
		"not found": {subscriber.ErrOperationNotFound, 404, "OPERATION_NOT_FOUND"},
		"state":     {subscriber.ErrOperationState, 409, "OPERATION_STATE_CONFLICT"},
		"locked":    {store.ErrLocked, 409, "OPERATION_IN_PROGRESS"},
		"store":     {errors.New("valkey down"), 500, "SYSTEM_FAILURE"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, true)
			e.subs.err = c.err
			for _, req := range [][2]string{{"GET", ""}, {"POST", "/retry"}, {"POST", "/dismiss"}} {
				if p := decode[problem](t, e.do(req[0], "/admin/v1/operations/"+opID+req[1], nil), c.status); p.Cause != c.cause {
					t.Errorf("%s %s: %+v", req[0], req[1], p)
				}
			}
		})
	}
	// 処理中の 409 は Idempotency-Key で覚えない。
	e := newEnv(t, true)
	e.subs.err = store.ErrLocked
	hdr := map[string]string{"Idempotency-Key": "retry-1"}
	e.do("POST", "/admin/v1/operations/"+opID+"/retry", hdr)
	e.subs.err, e.subs.op = nil, sampleOp(store.OpCompleted)
	if w := e.do("POST", "/admin/v1/operations/"+opID+"/retry", hdr); w.Code != 200 || w.Header().Get("Idempotent-Replayed") != "" {
		t.Errorf("after locked = %d %v", w.Code, w.Header())
	}
}

func TestStatusOperationCounts(t *testing.T) {
	e := newEnv(t, true)
	e.subs.counts = [3]int{1, 2, 3}
	got := decode[statusJSON](t, e.do("GET", "/admin/v1/status", nil), 200)
	if got.Operations == nil || *got.Operations != (operationCountsJSON{Running: 1, Retrying: 2, Failed: 3}) {
		t.Errorf("operations = %+v", got.Operations)
	}
	// 数えられなければ省く。
	e.subs.countsErr = errors.New("valkey down")
	if got := decode[statusJSON](t, e.do("GET", "/admin/v1/status", nil), 200); got.Operations != nil {
		t.Errorf("operations = %+v", got.Operations)
	}
}
