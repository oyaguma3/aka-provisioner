package subscriber

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/akaapi"
	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/plmn"
	"github.com/oyaguma3/aka-provisioner/internal/provapi"
	"github.com/oyaguma3/aka-provisioner/internal/store"
)

func TestGet(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	if _, err := e.s.Get(ctx, pocIMSI); !errors.Is(err, ErrNotFound) {
		t.Errorf("none: %v", err)
	}

	e.prov.subs[pocIMSI] = provapi.Subscriber{IMSI: pocIMSI, AMF: "8000", SQN: "000000000020"}
	e.prov.policies[pocIMSI] = provapi.Policy{IMSI: pocIMSI, Default: "deny", Rules: policy.Rules, Status: provapi.PolicySuspended}
	sub, err := e.s.Get(ctx, pocIMSI)
	if err != nil || sub.KeyStore != plmn.KeyStorePoC || sub.Key == nil || sub.Key.SQN != "000000000020" || sub.Key.AllowPlain != nil ||
		sub.Policy == nil || sub.Policy.Default != "deny" || sub.Status != provapi.PolicySuspended || len(sub.Issues) != 0 {
		t.Errorf("poc = %+v, %v", sub, err)
	}

	// aka 側に、vector-gateway を許可していない同じ IMSI の加入者があっても、本PoCと関係のないものとみなす。
	e.aka.subs[pocIMSI] = akaapi.Subscriber{IMSI: pocIMSI, AllowedClientIDs: []int64{9}}
	if sub, _ := e.s.Get(ctx, pocIMSI); len(sub.Issues) != 0 {
		t.Errorf("unrelated aka = %v", sub.Issues)
	}
	// 許可していれば、置き場所でない方にもある。
	e.aka.subs[pocIMSI] = akaapi.Subscriber{IMSI: pocIMSI, AllowedClientIDs: []int64{1, 9}}
	if sub, _ := e.s.Get(ctx, pocIMSI); issues(sub) != "[KEY_IN_OTHER_STORE]" {
		t.Errorf("misplaced = %v", sub.Issues)
	}
	// 置き場所でない方に接続できなければ、その旨を示して返す。
	e.aka.plan("aka.GetSubscriber", fault{err: errUnavailable})
	if sub, err := e.s.Get(ctx, pocIMSI); err != nil || issues(sub) != "[OTHER_STORE_UNREACHABLE]" {
		t.Errorf("aka unreachable = %v, %v", sub.Issues, err)
	}
	// prov に接続できなければエラー。
	e.prov.plan("prov.GetPolicy", fault{err: errUnavailable})
	if _, err := e.s.Get(ctx, pocIMSI); !isDownstreamErr(err, downstream.Prov) {
		t.Errorf("prov unreachable: %v", err)
	}

	// aka の加入者: 許可がない、ポリシーがない、prov にもある。
	e.aka.subs[akaIMSI] = akaapi.Subscriber{IMSI: akaIMSI, SQNType: "inc32", AllowedClientIDs: []int64{}}
	e.prov.subs[akaIMSI] = provapi.Subscriber{IMSI: akaIMSI}
	sub, err = e.s.Get(ctx, akaIMSI)
	if err != nil || sub.KeyStore != plmn.KeyStoreAKA || sub.Key == nil || sub.Key.SQNType != "inc32" || sub.Key.AllowPlain == nil ||
		issues(sub) != "[AV_CLIENT_NOT_ALLOWED KEY_IN_OTHER_STORE POLICY_MISSING]" {
		t.Errorf("aka = %+v, %v", sub, err)
	}
	// 置き場所の aka に接続できなければエラー。
	e.aka.plan("aka.GetSubscriber", fault{err: errUnavailable})
	if _, err := e.s.Get(ctx, akaIMSI); !isDownstreamErr(err, downstream.Aka) {
		t.Errorf("aka unreachable: %v", err)
	}
	// ポリシーだけ。
	delete(e.aka.subs, akaIMSI)
	delete(e.prov.subs, akaIMSI)
	e.prov.policies[akaIMSI] = provapi.Policy{IMSI: akaIMSI, Default: "allow"}
	if sub, err := e.s.Get(ctx, akaIMSI); err != nil || issues(sub) != "[KEY_MISSING]" {
		t.Errorf("policy only = %+v, %v", sub, err)
	}
	// ポリシーがなければ状態もない。
	delete(e.prov.policies, akaIMSI)
	e.aka.subs[akaIMSI] = akaapi.Subscriber{IMSI: akaIMSI, AllowedClientIDs: []int64{1}}
	if sub, err := e.s.Get(ctx, akaIMSI); err != nil || sub.Status != "" || issues(sub) != "[POLICY_MISSING]" {
		t.Errorf("no policy = %+v, %v", sub, err)
	}
}

func isDownstreamErr(err error, name downstream.Name) bool {
	d, ok := errors.AsType[*DownstreamError](err)
	return ok && d.Name == name
}

func TestCreate(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()

	sub, res, err := e.s.Create(ctx, actor, CreateInput{IMSI: pocIMSI, Ki: "aa", OPc: "bb", AMF: "b9b9", Policy: policy})
	if err != nil || sub.Key == nil || sub.Key.AMF != "b9b9" || sub.Policy == nil || len(sub.Issues) != 0 {
		t.Fatalf("poc = %+v, %v", sub, err)
	}
	op := e.st.op(res.OperationID)
	if op.Status != store.OpCompleted || op.Kind != KindCreate || op.KeyStore != "poc" || op.Operator != "alice" || op.TraceID != "trace-1" ||
		stepStates(op.Steps) != "subscriber.create@prov=done policy.put@prov=done" || res.Status != store.OpCompleted {
		t.Errorf("op = %+v", op)
	}
	if _, ok := e.prov.policies[pocIMSI]; !ok {
		t.Error("policy was not put")
	}

	// aka: vector-gateway の AVクライアントID を許可する。
	plain := true
	sub, res, err = e.s.Create(ctx, actor, CreateInput{IMSI: akaIMSI, Ki: "aa", OPc: "bb", SQNType: "inc33", AllowPlain: &plain, Policy: policy})
	if err != nil || sub.KeyStore != plmn.KeyStoreAKA || !slices.Equal(e.aka.subs[akaIMSI].AllowedClientIDs, []int64{1}) ||
		sub.Key.SQNType != "inc33" || !*sub.Key.AllowPlain || len(sub.Issues) != 0 {
		t.Fatalf("aka = %+v, %v", sub, err)
	}
	if op := e.st.op(res.OperationID); stepStates(op.Steps) != "subscriber.create@aka=done policy.put@prov=done" {
		t.Errorf("aka op = %s", stepStates(op.Steps))
	}
	if _, ok := e.prov.subs[akaIMSI]; ok {
		t.Error("aka subscriber was created in prov")
	}
}

func TestCreateConflict(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(e *env)
		imsi  string
		want  []string
	}{
		"prov subscriber": {func(e *env) { e.prov.subs[pocIMSI] = provapi.Subscriber{IMSI: pocIMSI} }, pocIMSI, []string{"poc"}},
		"policy":          {func(e *env) { e.prov.policies[pocIMSI] = provapi.Policy{IMSI: pocIMSI} }, pocIMSI, []string{"policy"}},
		"aka allows av": {func(e *env) {
			e.aka.subs[pocIMSI] = akaapi.Subscriber{IMSI: pocIMSI, AllowedClientIDs: []int64{1}}
		}, pocIMSI, []string{"aka"}},
		"aka (key store)": {func(e *env) { e.aka.subs[akaIMSI] = akaapi.Subscriber{IMSI: akaIMSI} }, akaIMSI, []string{"aka"}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			c.setup(e)
			_, res, err := e.s.Create(t.Context(), actor, CreateInput{IMSI: c.imsi, Ki: "aa", OPc: "bb", Policy: policy})
			if ce, ok := errors.AsType[*ConflictError](err); !ok || !slices.Equal(ce.Places, c.want) || res.OperationID != "" {
				t.Errorf("err = %v, res = %+v", err, res)
			}
			if len(e.st.ops) != 0 {
				t.Error("operation was recorded")
			}
		})
	}

	// poc の IMSI の、vector-gateway を許可していない aka 側の加入者は、本PoCと関係がないので作成できる。
	e := newEnv(t)
	e.aka.subs[pocIMSI] = akaapi.Subscriber{IMSI: pocIMSI, AllowedClientIDs: []int64{9}}
	if _, _, err := e.s.Create(t.Context(), actor, CreateInput{IMSI: pocIMSI, Ki: "aa", OPc: "bb", Policy: policy}); err != nil {
		t.Errorf("unrelated aka: %v", err)
	}
	// ロックを持っている操作があれば、待たずに断る。
	e.st.locks[akaIMSI] = "other"
	if _, _, err := e.s.Create(t.Context(), actor, CreateInput{IMSI: akaIMSI, Policy: policy}); !errors.Is(err, store.ErrLocked) {
		t.Errorf("locked: %v", err)
	}
}

func TestCreateFailures(t *testing.T) {
	for name, c := range map[string]struct {
		imsi    string
		setup   func(e *env)
		steps   string // 操作の記録の手順の状態
		status  string
		rolled  bool
		left    string // 下流に残ったもの
		errFrom downstream.Name
		prefix  string
	}{
		"key rejected": {
			imsi: pocIMSI, setup: func(e *env) { e.prov.plan("prov.CreateSubscriber", fault{err: errBadRequest}) },
			steps:  "subscriber.create@prov=failed policy.put@prov=skipped",
			status: store.OpRolledBack, rolled: true, left: "", errFrom: downstream.Prov,
		},
		"key timeout after create": {
			imsi: akaIMSI, setup: func(e *env) { e.aka.plan("aka.CreateSubscriber", fault{err: errUnavailable, applied: true}) },
			steps:  "subscriber.create@aka=failed policy.put@prov=skipped subscriber.compensate@aka=done",
			status: store.OpRolledBack, rolled: true, left: "", errFrom: downstream.Aka,
		},
		"policy rejected": {
			imsi: pocIMSI, setup: func(e *env) { e.prov.plan("prov.PutPolicy", fault{err: errBadRequest}) },
			steps:  "subscriber.create@prov=compensated policy.put@prov=failed subscriber.compensate@prov=done",
			status: store.OpRolledBack, rolled: true, left: "", errFrom: downstream.Prov, prefix: "policy.",
		},
		"policy timeout after put": {
			imsi: akaIMSI, setup: func(e *env) { e.prov.plan("prov.PutPolicy", fault{err: errUnavailable, applied: true}) },
			steps:  "subscriber.create@aka=compensated policy.put@prov=failed policy.delete@prov=done subscriber.compensate@aka=done",
			status: store.OpRolledBack, rolled: true, left: "", errFrom: downstream.Prov, prefix: "policy.",
		},
		"policy rejected and compensation fails": {
			imsi: akaIMSI, setup: func(e *env) {
				e.prov.plan("prov.PutPolicy", fault{err: errBadRequest})
				e.aka.plan("aka.DeleteSubscriber", fault{err: errServer})
			},
			steps:  "subscriber.create@aka=done policy.put@prov=failed subscriber.compensate@aka=failed",
			status: store.OpRetrying, rolled: false, left: "aka", errFrom: downstream.Prov, prefix: "policy.",
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			c.setup(e)
			_, res, err := e.s.Create(t.Context(), actor, CreateInput{IMSI: c.imsi, Ki: "aa", OPc: "bb", Policy: policy})
			oe, ok := errors.AsType[*OperationError](err)
			if !ok || oe.OperationID != res.OperationID || oe.RolledBack != c.rolled || oe.Incomplete == c.rolled {
				t.Fatalf("err = %v", err)
			}
			if de, ok := errors.AsType[*DownstreamError](err); !ok || de.Name != c.errFrom || de.ParamPrefix != c.prefix {
				t.Errorf("cause = %v", err)
			}
			op := e.st.op(res.OperationID)
			if stepStates(op.Steps) != c.steps || op.Status != c.status || res.Status != c.status {
				t.Errorf("op = %s %s (result %s)", op.Status, stepStates(op.Steps), res.Status)
			}
			if c.status == store.OpRetrying && !op.NextAttemptAt.Equal(e.s.now().Add(30*time.Second)) {
				t.Errorf("next attempt = %v", op.NextAttemptAt)
			}
			if got := leftovers(e, c.imsi); got != c.left {
				t.Errorf("left = %q, want %q", got, c.left)
			}
			if len(e.st.locks) != 0 {
				t.Error("lock is not released")
			}
		})
	}
}

// leftovers は下流に残っているもの（prov / aka / policy）を返す。
func leftovers(e *env, imsi string) string {
	var out []string
	if _, ok := e.prov.subs[imsi]; ok {
		out = append(out, "prov")
	}
	if _, ok := e.aka.subs[imsi]; ok {
		out = append(out, "aka")
	}
	if _, ok := e.prov.policies[imsi]; ok {
		out = append(out, "policy")
	}
	return joinSpace(out)
}

func joinSpace(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += " "
		}
		out += v
	}
	return out
}

func TestCreateStoreFailures(t *testing.T) {
	storeErr := errors.New("valkey down")
	// 最初の記録が書けなければ、下流には何もしない。
	e := newEnv(t)
	e.st.saveErrs = map[int]error{1: storeErr}
	_, res, err := e.s.Create(t.Context(), actor, CreateInput{IMSI: pocIMSI, Ki: "aa", OPc: "bb", Policy: policy})
	if !errors.Is(err, storeErr) || res.OperationID != "" || len(e.prov.callList()) != 2 /* 存在確認の 2 回だけ */ {
		t.Errorf("first save: %v, calls %v", err, e.prov.callList())
	}

	// 途中の記録・最後の「完了」が書けなければ、成功として返さずに戻す。
	for n, steps := range map[int]string{
		2: "subscriber.create@prov=compensated policy.put@prov=skipped subscriber.compensate@prov=done",
		3: "subscriber.create@prov=compensated policy.put@prov=compensated policy.delete@prov=done subscriber.compensate@prov=done",
	} {
		e := newEnv(t)
		e.st.saveErrs = map[int]error{n: storeErr}
		_, res, err := e.s.Create(t.Context(), actor, CreateInput{IMSI: pocIMSI, Ki: "aa", OPc: "bb", Policy: policy})
		oe, ok := errors.AsType[*OperationError](err)
		if !ok || !oe.RolledBack || !errors.Is(err, storeErr) {
			t.Errorf("save %d: %v", n, err)
		}
		if op := e.st.op(res.OperationID); op.Status != store.OpRolledBack || stepStates(op.Steps) != steps {
			t.Errorf("save %d: op = %s %s", n, op.Status, stepStates(op.Steps))
		}
		if got := leftovers(e, pocIMSI); got != "" {
			t.Errorf("save %d: left = %q", n, got)
		}
	}
}

func TestUpdate(t *testing.T) {
	ctx := t.Context()
	prev := provapi.Policy{IMSI: pocIMSI, Default: "allow", Rules: []provapi.PolicyRule{}}
	setup := func(t *testing.T) *env {
		e := newEnv(t)
		e.prov.subs[pocIMSI] = provapi.Subscriber{IMSI: pocIMSI, AMF: "8000", SQN: "000000000020"}
		e.prov.policies[pocIMSI] = prev
		return e
	}
	amf := "B9B9"

	e := setup(t)
	sub, res, err := e.s.Update(ctx, actor, pocIMSI, UpdateInput{AMF: &amf, Policy: &policy})
	if err != nil || sub.Key.AMF != "b9b9" || sub.Key.SQN != "000000000020" || sub.Policy.Default != "deny" {
		t.Fatalf("update = %+v, %v", sub, err)
	}
	op := e.st.op(res.OperationID)
	if op.Status != store.OpCompleted || stepStates(op.Steps) != "policy.put@prov=done subscriber.update@prov=done" ||
		!op.HadPolicy || op.PrevPolicy != `{"default":"allow","rules":[]}` {
		t.Errorf("op = %+v", op)
	}

	// 鍵の変更が失敗したら、ポリシーを変更前に戻す。
	e = setup(t)
	e.prov.plan("prov.UpdateSubscriber", fault{err: errUnavailable})
	_, res, err = e.s.Update(ctx, actor, pocIMSI, UpdateInput{AMF: &amf, Policy: &policy})
	if oe, ok := errors.AsType[*OperationError](err); !ok || !oe.RolledBack {
		t.Fatalf("err = %v", err)
	}
	if op := e.st.op(res.OperationID); op.Status != store.OpRolledBack ||
		stepStates(op.Steps) != "policy.put@prov=compensated subscriber.update@prov=failed policy.restore@prov=done" {
		t.Errorf("op = %s %s", op.Status, stepStates(op.Steps))
	}
	if got := e.prov.policies[pocIMSI]; got.Default != "allow" {
		t.Errorf("policy = %+v", got)
	}

	// 停止中の加入者: 変更でも補償でも停止は解けない（ポリシーの PUT は状態を変えないため、変更前のポリシーは
	// 状態を含めずに覚える）。
	e = setup(t)
	e.prov.policies[pocIMSI] = provapi.Policy{IMSI: pocIMSI, Default: "allow", Rules: []provapi.PolicyRule{}, Status: provapi.PolicySuspended}
	if sub, _, err := e.s.Update(ctx, actor, pocIMSI, UpdateInput{Policy: &policy}); err != nil || sub.Status != provapi.PolicySuspended {
		t.Errorf("suspended update = %+v, %v", sub, err)
	}
	e.prov.plan("prov.UpdateSubscriber", fault{err: errUnavailable})
	_, res, err = e.s.Update(ctx, actor, pocIMSI, UpdateInput{AMF: &amf, Policy: &provapi.PolicyPut{Default: "allow", Rules: []provapi.PolicyRule{}}})
	if oe, ok := errors.AsType[*OperationError](err); !ok || !oe.RolledBack {
		t.Fatalf("suspended err = %v", err)
	}
	if op := e.st.op(res.OperationID); op.PrevPolicy != `{"default":"deny","rules":[{"nasId":"*","allowedSsids":["CORP"]}]}` {
		t.Errorf("prev policy = %s", op.PrevPolicy)
	}
	if got := e.prov.policies[pocIMSI]; got.Default != "deny" || got.Status != provapi.PolicySuspended {
		t.Errorf("suspended policy after rollback = %+v", got)
	}

	// 変更前にポリシーがなかったなら、戻すときは消す。
	e = setup(t)
	delete(e.prov.policies, pocIMSI)
	e.prov.plan("prov.UpdateSubscriber", fault{err: errServer})
	if _, _, err := e.s.Update(ctx, actor, pocIMSI, UpdateInput{AMF: &amf, Policy: &policy}); err == nil {
		t.Fatal("want error")
	}
	if _, ok := e.prov.policies[pocIMSI]; ok {
		t.Error("policy is left")
	}

	// 戻せなければ、後でやり直す。
	e = setup(t)
	e.prov.plan("prov.UpdateSubscriber", fault{err: errServer})
	e.prov.plan("prov.PutPolicy", fault{}, fault{err: errUnavailable})
	_, res, err = e.s.Update(ctx, actor, pocIMSI, UpdateInput{AMF: &amf, Policy: &policy})
	if oe, ok := errors.AsType[*OperationError](err); !ok || !oe.Incomplete {
		t.Fatalf("err = %v", err)
	}
	if op := e.st.op(res.OperationID); op.Status != store.OpRetrying ||
		stepStates(op.Steps) != "policy.put@prov=done subscriber.update@prov=failed policy.restore@prov=failed" {
		t.Errorf("op = %s %s", op.Status, stepStates(op.Steps))
	}

	// ポリシーの PUT が断られたら、何も変わっていないので戻すものはない。下流の項目名は policy. を付けて返す。
	e = setup(t)
	e.prov.plan("prov.PutPolicy", fault{err: errBadRequest})
	_, res, err = e.s.Update(ctx, actor, pocIMSI, UpdateInput{AMF: &amf, Policy: &policy})
	if de, ok := errors.AsType[*DownstreamError](err); !ok || de.ParamPrefix != "policy." {
		t.Fatalf("err = %v", err)
	}
	if op := e.st.op(res.OperationID); op.Status != store.OpRolledBack || stepStates(op.Steps) != "policy.put@prov=failed subscriber.update@prov=skipped" {
		t.Errorf("op = %s %s", op.Status, stepStates(op.Steps))
	}
	if e.prov.subs[pocIMSI].AMF != "8000" {
		t.Error("key was changed")
	}

	// 鍵がない加入者の鍵の変更は 404。ポリシーだけなら変えられる。どこにもなければ 404。
	e = newEnv(t)
	e.prov.policies[akaIMSI] = provapi.Policy{IMSI: akaIMSI, Default: "deny"}
	plain := true
	if _, _, err := e.s.Update(ctx, actor, akaIMSI, UpdateInput{AllowPlain: &plain}); !errors.Is(err, ErrNotFound) {
		t.Errorf("no key: %v", err)
	}
	if sub, _, err := e.s.Update(ctx, actor, akaIMSI, UpdateInput{Policy: &policy}); err != nil || issues(sub) != "[KEY_MISSING]" {
		t.Errorf("policy only: %+v, %v", sub, err)
	}
	if _, _, err := e.s.Update(ctx, actor, "001020000000002", UpdateInput{Policy: &policy}); !errors.Is(err, ErrNotFound) {
		t.Errorf("none: %v", err)
	}

	// aka の鍵の変更は aka に送り、許可クライアントには触れない。
	e = newEnv(t)
	e.aka.subs[akaIMSI] = akaapi.Subscriber{IMSI: akaIMSI, SQNType: "inc32", AllowedClientIDs: []int64{1, 5}}
	if sub, _, err := e.s.Update(ctx, actor, akaIMSI, UpdateInput{AllowPlain: &plain}); err != nil || !*sub.Key.AllowPlain ||
		!slices.Equal(sub.Key.AllowedClientIDs, []int64{1, 5}) || !slices.Contains(e.aka.callList(), "aka.UpdateSubscriber") {
		t.Errorf("aka update: %+v, %v", sub, err)
	}
}

func TestDelete(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	e.aka.subs[akaIMSI] = akaapi.Subscriber{IMSI: akaIMSI, AllowedClientIDs: []int64{1}}
	e.prov.policies[akaIMSI] = provapi.Policy{IMSI: akaIMSI}
	res, err := e.s.Delete(ctx, actor, akaIMSI)
	if err != nil || leftovers(e, akaIMSI) != "" {
		t.Fatalf("delete: %v, left %q", err, leftovers(e, akaIMSI))
	}
	if op := e.st.op(res.OperationID); op.Status != store.OpCompleted || stepStates(op.Steps) != "policy.delete@prov=done subscriber.delete@aka=done" {
		t.Errorf("op = %s %s", op.Status, stepStates(op.Steps))
	}

	// 既にないものは「不要だった」として進む。置き場所でない方の加入者は消さない。
	e = newEnv(t)
	e.prov.subs[pocIMSI] = provapi.Subscriber{IMSI: pocIMSI}
	e.aka.subs[pocIMSI] = akaapi.Subscriber{IMSI: pocIMSI, AllowedClientIDs: []int64{1}}
	res, err = e.s.Delete(ctx, actor, pocIMSI)
	if err != nil || leftovers(e, pocIMSI) != "aka" {
		t.Fatalf("delete: %v, left %q", err, leftovers(e, pocIMSI))
	}
	if op := e.st.op(res.OperationID); stepStates(op.Steps) != "policy.delete@prov=skipped subscriber.delete@prov=done" {
		t.Errorf("op = %s", stepStates(op.Steps))
	}

	// 途中で失敗したら戻さず、後で残りをやり直す。
	e = newEnv(t)
	e.prov.subs[pocIMSI] = provapi.Subscriber{IMSI: pocIMSI}
	e.prov.policies[pocIMSI] = provapi.Policy{IMSI: pocIMSI}
	e.prov.plan("prov.DeleteSubscriber", fault{err: errUnavailable})
	res, err = e.s.Delete(ctx, actor, pocIMSI)
	oe, ok := errors.AsType[*OperationError](err)
	if !ok || !oe.Incomplete || oe.RolledBack || !isDownstreamErr(err, downstream.Prov) || !downstream.IsUnavailable(errors.Unwrap(oe.Err)) {
		t.Fatalf("err = %v", err)
	}
	if op := e.st.op(res.OperationID); op.Status != store.OpRetrying || stepStates(op.Steps) != "policy.delete@prov=done subscriber.delete@prov=failed" {
		t.Errorf("op = %s %s", op.Status, stepStates(op.Steps))
	}
	if leftovers(e, pocIMSI) != "prov" {
		t.Errorf("left = %q", leftovers(e, pocIMSI))
	}

	if _, err := newEnv(t).s.Delete(ctx, actor, pocIMSI); !errors.Is(err, ErrNotFound) {
		t.Errorf("none: %v", err)
	}
}

func TestKeys(t *testing.T) {
	e := newEnv(t)
	e.prov.subs[pocIMSI] = provapi.Subscriber{IMSI: pocIMSI}
	e.aka.subs[akaIMSI] = akaapi.Subscriber{IMSI: akaIMSI}
	if ki, opc, err := e.s.Keys(t.Context(), pocIMSI); err != nil || ki != "k-"+pocIMSI || opc != "o-"+pocIMSI {
		t.Errorf("poc = %q %q %v", ki, opc, err)
	}
	if ki, _, err := e.s.Keys(t.Context(), akaIMSI); err != nil || ki != "k-"+akaIMSI {
		t.Errorf("aka = %q %v", ki, err)
	}
	if _, _, err := e.s.Keys(t.Context(), "001020000000002"); !errors.Is(err, ErrNotFound) {
		t.Errorf("none: %v", err)
	}
	e.prov.plan("prov.GetSubscriberKeys", fault{err: errServer})
	if _, _, err := e.s.Keys(t.Context(), pocIMSI); !isDownstreamErr(err, downstream.Prov) {
		t.Errorf("error: %v", err)
	}
}

func TestList(t *testing.T) {
	e := newEnv(t)
	// poc: 001010...1〜3、aka: 001020...1〜3、aka 側の本PoCと関係のない加入者: 001030...（PLMN 00103 は poc）
	for i := range 3 {
		imsi := "00101000000000" + string(rune('1'+i))
		e.prov.subs[imsi] = provapi.Subscriber{IMSI: imsi}
		if i != 1 {
			e.prov.policies[imsi] = provapi.Policy{IMSI: imsi}
		}
	}
	for i := range 3 {
		imsi := "00102000000000" + string(rune('1'+i))
		e.aka.subs[imsi] = akaapi.Subscriber{IMSI: imsi, AllowedClientIDs: []int64{1}}
		e.prov.policies[imsi] = provapi.Policy{IMSI: imsi}
	}
	for i := range 5 {
		imsi := "00103000000000" + string(rune('1'+i))
		e.aka.subs[imsi] = akaapi.Subscriber{IMSI: imsi}
	}
	e.prov.policies["001020000000009"] = provapi.Policy{IMSI: "001020000000009"} // ポリシーだけ
	e.prov.subs["001020000000003"] = provapi.Subscriber{IMSI: "001020000000003"} // 置き場所でない方にもある

	var all []string
	var pages int
	p := downstream.ListParams{Limit: 2}
	for {
		l, err := e.s.List(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, s := range l.Items {
			all = append(all, s.IMSI+string(s.KeyStore)+issues(s))
		}
		if l.NextCursor == "" {
			break
		}
		p.Cursor = l.NextCursor
	}
	want := []string{
		"001010000000001poc[]",
		"001010000000002poc[POLICY_MISSING]",
		"001010000000003poc[]",
		"001020000000001aka[]",
		"001020000000002aka[]",
		"001020000000003aka[KEY_IN_OTHER_STORE]",
		"001020000000009aka[KEY_MISSING]",
	}
	if !slices.Equal(all, want) || pages != 4 {
		t.Errorf("list (%d pages) =\n%v\nwant\n%v", pages, all, want)
	}

	// prefix で絞る。
	l, err := e.s.List(t.Context(), downstream.ListParams{Prefix: "00102", Limit: 50})
	if err != nil || len(l.Items) != 4 || l.NextCursor != "" {
		t.Errorf("prefix = %+v, %v", l, err)
	}
	// 下流に接続できなければエラー。
	e.aka.plan("aka.ListSubscribers", fault{err: errUnavailable})
	if _, err := e.s.List(t.Context(), downstream.ListParams{}); !isDownstreamErr(err, downstream.Aka) {
		t.Errorf("aka unreachable: %v", err)
	}
}
