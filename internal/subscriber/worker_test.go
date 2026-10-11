package subscriber

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oyaguma3/eapaka-node-provisioner/internal/akaapi"
	"github.com/oyaguma3/eapaka-node-provisioner/internal/provapi"
	"github.com/oyaguma3/eapaka-node-provisioner/internal/store"
)

// staleOp は、要求が落ちて実行中のまま残った（ロックの有効期限を過ぎた）操作の記録を作る。
func (e *env) staleOp(id, kind, imsi, ks string, steps ...store.OperationStep) store.Operation {
	op := store.Operation{
		ID: id, Kind: kind, IMSI: imsi, KeyStore: ks, Status: store.OpRunning, Steps: steps,
		NextAttemptAt: e.now.Add(-time.Second), GiveUpAt: e.now.Add(23 * time.Hour),
		Operator: "alice", MgmtClient: "bff", TraceID: "trace-op", CreatedAt: e.now.Add(-time.Hour),
	}
	e.st.put(op)
	return op
}

func step(name, ds, state string) store.OperationStep {
	return store.OperationStep{Name: name, Downstream: ds, State: state}
}

func TestWorkerResumesStaleCreate(t *testing.T) {
	// 加入者を作った後、ポリシーの PUT の途中で落ちた（PUT が反映されたかは分からない）。
	e := newEnv(t)
	e.aka.subs[akaIMSI] = akaapi.Subscriber{IMSI: akaIMSI, AllowedClientIDs: []int64{1}}
	e.prov.policies[akaIMSI] = provapi.Policy{IMSI: akaIMSI}
	e.staleOp("op-1", KindCreate, akaIMSI, "aka", step("subscriber.create", "aka", "done"), step("policy.put", "prov", "pending"))

	if n := e.s.ProcessDue(t.Context()); n != 1 {
		t.Fatalf("processed = %d", n)
	}
	op := e.st.op("op-1")
	if op.Status != store.OpRolledBack || op.Attempts != 1 ||
		stepStates(op.Steps) != "subscriber.create@aka=compensated policy.put@prov=pending policy.delete@prov=done subscriber.compensate@aka=done" {
		t.Errorf("op = %s %d %s", op.Status, op.Attempts, stepStates(op.Steps))
	}
	if leftovers(e, akaIMSI) != "" {
		t.Errorf("left = %q", leftovers(e, akaIMSI))
	}
	// 監査ログ（ワーカーなので操作者・管理クライアントは空、トレースID は元の操作のもの）。
	a := e.audit.list()
	if len(a) != 1 || a[0].Action != "operation.resume" || a[0].Target != "op-1" || a[0].Result != store.OpRolledBack ||
		a[0].Operator != "" || a[0].TraceID != "trace-op" {
		t.Errorf("audit = %+v", a)
	}
	// 完了したものは、もう処理しない。
	if n := e.s.ProcessDue(t.Context()); n != 0 {
		t.Errorf("processed again = %d", n)
	}
}

func TestWorkerRetryAndGiveUp(t *testing.T) {
	e := newEnv(t)
	e.prov.subs[pocIMSI] = provapi.Subscriber{IMSI: pocIMSI}
	op := e.staleOp("op-2", KindDelete, pocIMSI, "poc", step("policy.delete", "prov", "done"), step("subscriber.delete", "prov", "failed"))
	op.Status = store.OpRetrying
	e.st.put(op)

	// 失敗するたびに間隔を倍にする（30 秒 → 1 分 → 2 分 … 10 分まで）。監査ログには途中経過を残さない。
	var delays []time.Duration
	for range 7 {
		e.prov.plan("prov.DeleteSubscriber", fault{err: errServer})
		if n := e.s.ProcessDue(t.Context()); n != 1 {
			t.Fatalf("processed = %d", n)
		}
		op := e.st.op("op-2")
		if op.Status != store.OpRetrying {
			t.Fatalf("status = %s", op.Status)
		}
		delays = append(delays, op.NextAttemptAt.Sub(e.now))
		// 時刻が来るまでは処理しない。
		if n := e.s.ProcessDue(t.Context()); n != 0 {
			t.Errorf("processed before due = %d", n)
		}
		e.now = op.NextAttemptAt
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	if !slices.Equal(delays, want) || len(e.audit.list()) != 0 {
		t.Errorf("delays = %v, audits %d", delays, len(e.audit.list()))
	}

	// 期限を過ぎたら failed にして、自動では処理しない。
	e.now = e.st.op("op-2").GiveUpAt
	e.prov.plan("prov.DeleteSubscriber", fault{err: errServer})
	e.s.ProcessDue(t.Context())
	if op := e.st.op("op-2"); op.Status != store.OpFailed {
		t.Fatalf("status = %s", op.Status)
	}
	if a := e.audit.list(); len(a) != 1 || a[0].Result != store.OpFailed {
		t.Errorf("audit = %+v", a)
	}
	e.now = e.now.Add(time.Hour)
	if n := e.s.ProcessDue(t.Context()); n != 0 {
		t.Errorf("failed was processed")
	}

	// 手でやり直す。成功すれば完了。
	got, err := e.s.Retry(t.Context(), Actor{Operator: "bob", MgmtClient: "bff", TraceID: "trace-retry"}, "op-2")
	if err != nil || got.Status != store.OpCompleted || leftovers(e, pocIMSI) != "" {
		t.Fatalf("retry = %s, %v", got.Status, err)
	}
	if a := e.audit.list(); len(a) != 2 || a[1].Action != "operation.retry" || a[1].Operator != "bob" || a[1].Result != store.OpCompleted {
		t.Errorf("audit = %+v", a)
	}
	// 完了したものは、やり直せない。
	if _, err := e.s.Retry(t.Context(), actor, "op-2"); !errors.Is(err, ErrOperationState) {
		t.Errorf("retry completed: %v", err)
	}
}

func TestRetryFailingRestartsAutomaticRetry(t *testing.T) {
	e := newEnv(t)
	e.prov.subs[pocIMSI] = provapi.Subscriber{IMSI: pocIMSI}
	op := e.staleOp("op-3", KindDelete, pocIMSI, "poc", step("policy.delete", "prov", "done"), step("subscriber.delete", "prov", "failed"))
	op.Status, op.GiveUpAt = store.OpFailed, e.now.Add(-time.Hour)
	e.st.put(op)

	// 手でやり直して失敗したら、retrying に戻り、自動のやり直しの期限を今から数え直す。
	e.prov.plan("prov.DeleteSubscriber", fault{err: errUnavailable})
	got, err := e.s.Retry(t.Context(), actor, "op-3")
	if err != nil || got.Status != store.OpRetrying || !got.GiveUpAt.Equal(e.now.Add(24*time.Hour)) || got.NextAttemptAt.Before(e.now) {
		t.Fatalf("retry = %+v, %v", got, err)
	}
	// その後は自動で続く。
	e.now = got.NextAttemptAt
	if e.s.ProcessDue(t.Context()); e.st.op("op-3").Status != store.OpCompleted {
		t.Errorf("status = %s", e.st.op("op-3").Status)
	}
}

func TestWorkerUpdate(t *testing.T) {
	prev := `{"default":"allow","rules":[]}`
	// ポリシーの PUT の途中で落ちた: 鍵には進んでいないので、ポリシーを変更前に戻す。
	e := newEnv(t)
	e.prov.subs[pocIMSI] = provapi.Subscriber{IMSI: pocIMSI}
	e.prov.policies[pocIMSI] = provapi.Policy{IMSI: pocIMSI, Default: "deny"}
	op := e.staleOp("op-4", KindUpdate, pocIMSI, "poc", step("policy.put", "prov", "pending"), step("subscriber.update", "prov", "pending"))
	op.PrevPolicy, op.HadPolicy = prev, true
	e.st.put(op)
	e.s.ProcessDue(t.Context())
	if op := e.st.op("op-4"); op.Status != store.OpRolledBack || e.prov.policies[pocIMSI].Default != "allow" {
		t.Errorf("op = %s %s, policy %+v", op.Status, stepStates(op.Steps), e.prov.policies[pocIMSI])
	}

	// ポリシーは済み、鍵の変更が済んだか分からない: 自動では何もせず failed にして、手での確認を待つ。
	for name, steps := range map[string][]store.OperationStep{
		"policy and key": {step("policy.put", "prov", "done"), step("subscriber.update", "prov", "pending")},
		"key only":       {step("subscriber.update", "aka", "pending")},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.prov.policies[pocIMSI] = provapi.Policy{IMSI: pocIMSI, Default: "deny"}
			op := e.staleOp("op-5", KindUpdate, pocIMSI, "poc", steps...)
			op.PrevPolicy, op.HadPolicy = prev, true
			e.st.put(op)
			e.s.ProcessDue(t.Context())
			if op := e.st.op("op-5"); op.Status != store.OpFailed || e.prov.policies[pocIMSI].Default != "deny" {
				t.Errorf("op = %s, policy %+v", op.Status, e.prov.policies[pocIMSI])
			}
			if calls := append(e.prov.callList(), e.aka.callList()...); len(calls) != 0 {
				t.Errorf("downstream was called: %v", calls)
			}
			if a := e.audit.list(); len(a) != 1 || a[0].Result != store.OpFailed {
				t.Errorf("audit = %+v", a)
			}
			// 手でのやり直しもできない（下流の状態を見て直し、dismiss する）。
			if _, err := e.s.Retry(t.Context(), actor, "op-5"); !errors.Is(err, ErrOperationState) {
				t.Errorf("retry: %v", err)
			}
			if got, err := e.s.Dismiss(t.Context(), actor, "op-5"); err != nil || got.Status != store.OpDismissed {
				t.Errorf("dismiss = %s, %v", got.Status, err)
			}
		})
	}
}

func TestUpdateFinalSaveFailureLeavesManualCheck(t *testing.T) {
	// 鍵の変更の後に「完了」が書けない（書き直しも失敗）: 鍵は戻せないので成功として返し、記録は実行中のまま残る。
	// ワーカーはそれを failed にする（ポリシーを戻さない）。
	e := newEnv(t)
	e.prov.subs[pocIMSI] = provapi.Subscriber{IMSI: pocIMSI, AMF: "8000"}
	e.prov.policies[pocIMSI] = provapi.Policy{IMSI: pocIMSI, Default: "allow"}
	storeErr := errors.New("valkey down")
	e.st.saveErrs = map[int]error{3: storeErr, 4: storeErr} // 1: 開始、2: ポリシーの後、3: 完了、4: 書き直し
	amf := "b9b9"
	sub, res, err := e.s.Update(t.Context(), actor, pocIMSI, UpdateInput{AMF: &amf, Policy: &policy})
	if err != nil || sub.Key.AMF != "b9b9" {
		t.Fatalf("update = %+v, %v", sub, err)
	}
	if op := e.st.op(res.OperationID); op.Status != store.OpRunning || stepStates(op.Steps) != "policy.put@prov=done subscriber.update@prov=pending" {
		t.Fatalf("op = %s %s", op.Status, stepStates(op.Steps))
	}
	e.now = e.now.Add(2 * time.Minute)
	e.s.ProcessDue(t.Context())
	if op := e.st.op(res.OperationID); op.Status != store.OpFailed || e.prov.policies[pocIMSI].Default != "deny" {
		t.Errorf("op = %s, policy %+v", op.Status, e.prov.policies[pocIMSI])
	}

	// 1 回目だけ失敗したなら、書き直しで「完了」が残る。
	e = newEnv(t)
	e.prov.subs[pocIMSI] = provapi.Subscriber{IMSI: pocIMSI}
	e.st.saveErrs = map[int]error{3: storeErr}
	if _, res, err := e.s.Update(t.Context(), actor, pocIMSI, UpdateInput{AMF: &amf, Policy: &policy}); err != nil ||
		e.st.op(res.OperationID).Status != store.OpCompleted {
		t.Errorf("rewrite: %v, %s", err, e.st.op(res.OperationID).Status)
	}
}

func TestWorkerSkipsLockedAndMissing(t *testing.T) {
	e := newEnv(t)
	e.prov.policies[pocIMSI] = provapi.Policy{IMSI: pocIMSI}
	e.staleOp("op-6", KindDelete, pocIMSI, "poc", step("policy.delete", "prov", "pending"), step("subscriber.delete", "prov", "pending"))
	// 要求がまだロックを持っていれば飛ばす。
	e.st.locks[pocIMSI] = "request"
	if n := e.s.ProcessDue(t.Context()); n != 0 || e.st.op("op-6").Status != store.OpRunning {
		t.Errorf("locked: processed %d", n)
	}
	delete(e.st.locks, pocIMSI)
	if n := e.s.ProcessDue(t.Context()); n != 1 || e.st.op("op-6").Status != store.OpCompleted {
		t.Errorf("unlocked: processed %d, %s", n, e.st.op("op-6").Status)
	}
	if _, err := e.s.Retry(t.Context(), actor, "none"); !errors.Is(err, ErrOperationNotFound) {
		t.Errorf("retry missing: %v", err)
	}
	if _, err := e.s.Dismiss(t.Context(), actor, "none"); !errors.Is(err, ErrOperationNotFound) {
		t.Errorf("dismiss missing: %v", err)
	}
	if _, err := e.s.Operation(t.Context(), "none"); !errors.Is(err, ErrOperationNotFound) {
		t.Errorf("get missing: %v", err)
	}
}

func TestOperationsAndDismiss(t *testing.T) {
	e := newEnv(t)
	e.staleOp("0199c8a2-0000-7000-8000-000000000003", KindDelete, pocIMSI, "poc")
	op := e.staleOp("0199c8a2-0000-7000-8000-000000000001", KindCreate, akaIMSI, "aka")
	op.Status = store.OpRetrying
	e.st.put(op)
	op = e.staleOp("0199c8a2-0000-7000-8000-000000000002", KindUpdate, pocIMSI, "poc")
	op.Status = store.OpFailed
	e.st.put(op)
	op = e.staleOp("0199c8a2-0000-7000-8000-000000000004", KindCreate, pocIMSI, "poc")
	op.Status = store.OpCompleted
	e.st.put(op)

	items, total, err := e.s.Operations(t.Context(), "", 2)
	if err != nil || total != 3 || len(items) != 2 || items[0].ID != "0199c8a2-0000-7000-8000-000000000001" ||
		items[1].ID != "0199c8a2-0000-7000-8000-000000000002" {
		t.Errorf("operations = %v %d %v", items, total, err)
	}
	if items, total, _ := e.s.Operations(t.Context(), store.OpFailed, 10); total != 1 || items[0].Status != store.OpFailed {
		t.Errorf("failed = %v %d", items, total)
	}
	counts, err := e.s.OperationCounts(t.Context())
	if err != nil || counts[store.OpRunning] != 1 || counts[store.OpRetrying] != 1 || counts[store.OpFailed] != 1 {
		t.Errorf("counts = %v, %v", counts, err)
	}

	// dismiss は retrying / failed だけ。
	if _, err := e.s.Dismiss(t.Context(), actor, "0199c8a2-0000-7000-8000-000000000003"); !errors.Is(err, ErrOperationState) {
		t.Errorf("dismiss running: %v", err)
	}
	got, err := e.s.Dismiss(t.Context(), actor, "0199c8a2-0000-7000-8000-000000000001")
	if err != nil || got.Status != store.OpDismissed {
		t.Fatalf("dismiss = %+v, %v", got, err)
	}
	if a := e.audit.list(); len(a) != 1 || a[0].Action != "operation.dismiss" || a[0].Operator != "alice" || a[0].Result != store.OpDismissed {
		t.Errorf("audit = %+v", a)
	}
	if _, total, _ := e.s.Operations(t.Context(), "", 10); total != 2 {
		t.Errorf("total after dismiss = %d", total)
	}
	// 同じ IMSI の操作が処理中なら断る。
	e.st.locks[pocIMSI] = "x"
	if _, err := e.s.Dismiss(t.Context(), actor, "0199c8a2-0000-7000-8000-000000000002"); !errors.Is(err, store.ErrLocked) {
		t.Errorf("dismiss locked: %v", err)
	}
}

func TestUnresolvedOperationBlocksNewOperations(t *testing.T) {
	// 作成の補償が残った（aka-only-server に接続できず、作った加入者を消せていない）。
	e := newEnv(t)
	e.aka.subs[akaIMSI] = akaapi.Subscriber{IMSI: akaIMSI, AllowedClientIDs: []int64{1}}
	op := e.staleOp("0199c8a2-0000-7000-8000-000000000001", KindCreate, akaIMSI, "aka",
		step("subscriber.create", "aka", "done"), step("policy.put", "prov", "failed"),
		step("subscriber.compensate", "aka", "failed"))
	op.Status = store.OpRetrying
	e.st.put(op)
	// 別の IMSI の完了した操作は妨げない。
	done := e.staleOp("0199c8a2-0000-7000-8000-000000000002", KindCreate, pocIMSI, "poc")
	done.Status = store.OpCompleted
	e.st.put(done)
	ctx := t.Context()

	// 同じ IMSI の作成・変更・削除は、下流を呼ばずに断る。作り直した加入者が補償で消されないように。
	isUnresolved := func(err error) bool {
		ue, ok := errors.AsType[*UnresolvedError](err)
		return ok && ue.OperationID == op.ID && ue.Status == store.OpRetrying
	}
	if _, _, err := e.s.Create(ctx, actor, CreateInput{IMSI: akaIMSI, Ki: "aa", OPc: "bb", Policy: policy}); !isUnresolved(err) {
		t.Errorf("create: %v", err)
	}
	if _, _, err := e.s.Update(ctx, actor, akaIMSI, UpdateInput{Policy: &policy}); !isUnresolved(err) {
		t.Errorf("update: %v", err)
	}
	if _, err := e.s.Delete(ctx, actor, akaIMSI); !isUnresolved(err) {
		t.Errorf("delete: %v", err)
	}
	if err := e.s.CheckUnresolved(ctx, akaIMSI); !isUnresolved(err) {
		t.Errorf("check: %v", err)
	}
	if c := slices.Concat(e.prov.callList(), e.aka.callList()); len(c) != 0 {
		t.Errorf("downstream calls = %v", c)
	}
	if len(e.st.locks) != 0 {
		t.Errorf("locks were not released: %v", e.st.locks)
	}

	// 別の IMSI は通る。
	if _, _, err := e.s.Create(ctx, actor, CreateInput{IMSI: pocIMSI, Ki: "aa", OPc: "bb", Policy: policy}); err != nil {
		t.Errorf("other imsi: %v", err)
	}

	// failed と、要求が落ちた running も断る。閉じた（dismissed）後は通る。
	for _, status := range []string{store.OpFailed, store.OpRunning} {
		op.Status = status
		e.st.put(op)
		if err := e.s.CheckUnresolved(ctx, akaIMSI); err == nil {
			t.Errorf("%s: not refused", status)
		}
	}
	op.Status = store.OpFailed
	e.st.put(op)
	if _, err := e.s.Dismiss(ctx, actor, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Delete(ctx, actor, akaIMSI); err != nil {
		t.Errorf("delete after dismiss: %v", err)
	}
	if _, ok := e.aka.subs[akaIMSI]; ok {
		t.Error("aka subscriber remains")
	}
}

func TestWorkerRecoversFromPanic(t *testing.T) {
	// 2 つの操作のうち、先に処理した方の補償でパニックが起きても、ワーカーは止まらず、もう 1 つを処理する。
	e := newEnv(t)
	var logs bytes.Buffer
	e.s.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	for _, imsi := range []string{pocIMSI, akaIMSI} {
		e.prov.policies[imsi] = provapi.Policy{IMSI: imsi}
	}
	e.prov.subs[pocIMSI] = provapi.Subscriber{IMSI: pocIMSI}
	e.aka.subs[akaIMSI] = akaapi.Subscriber{IMSI: akaIMSI, AllowedClientIDs: []int64{1}}
	e.staleOp("op-p1", KindCreate, pocIMSI, "poc", step("subscriber.create", "prov", "done"), step("policy.put", "prov", "pending"))
	e.staleOp("op-p2", KindCreate, akaIMSI, "aka", step("subscriber.create", "aka", "done"), step("policy.put", "prov", "pending"))
	e.prov.plan("prov.DeletePolicy", fault{panics: true})

	if n := e.s.ProcessDue(t.Context()); n != 1 {
		t.Fatalf("processed = %d, want 1", n)
	}
	if !strings.Contains(logs.String(), `"msg":"worker panic"`) || !strings.Contains(logs.String(), "injected panic") {
		t.Errorf("panic was not logged: %s", logs.String())
	}
	statuses := []string{e.st.op("op-p1").Status, e.st.op("op-p2").Status}
	slices.Sort(statuses)
	if !slices.Equal(statuses, []string{store.OpRolledBack, store.OpRunning}) {
		t.Fatalf("statuses = %v", statuses)
	}
	// パニックした操作の IMSI のロックは解放されていて、次の回に処理できる。
	if n := e.s.ProcessDue(t.Context()); n != 1 {
		t.Fatalf("processed on the next round = %d, want 1", n)
	}
	for _, id := range []string{"op-p1", "op-p2"} {
		if st := e.st.op(id).Status; st != store.OpRolledBack {
			t.Errorf("%s = %s", id, st)
		}
	}
}

func TestRunWorkerFinishesCurrentOperationOnStop(t *testing.T) {
	// 停止の合図（ctx の終わり）が処理の途中で来ても、その操作は最後まで行ってから RunWorker が戻る。
	e := newEnv(t)
	e.prov.policies[pocIMSI] = provapi.Policy{IMSI: pocIMSI}
	e.prov.subs[pocIMSI] = provapi.Subscriber{IMSI: pocIMSI}
	e.staleOp("op-s1", KindCreate, pocIMSI, "poc", step("subscriber.create", "prov", "done"), step("policy.put", "prov", "pending"))
	ctx, cancel := context.WithCancel(t.Context())
	e.prov.plan("prov.DeletePolicy", fault{before: cancel, applied: true})

	done := make(chan struct{})
	go func() {
		e.s.RunWorker(ctx, time.Hour)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunWorker did not return after ctx was canceled")
	}
	if op := e.st.op("op-s1"); op.Status != store.OpRolledBack {
		t.Errorf("op = %s %s, want rolled_back (finished despite the stop)", op.Status, stepStates(op.Steps))
	}
	if leftovers(e, pocIMSI) != "" {
		t.Errorf("left = %q", leftovers(e, pocIMSI))
	}
}
