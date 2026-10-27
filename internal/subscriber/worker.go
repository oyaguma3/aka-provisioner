package subscriber

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/store"
	"github.com/oyaguma3/aka-provisioner/internal/trace"
)

// やり直しのワーカーと、操作の記録への操作（設計概要 §9.3）。

var (
	// ErrOperationNotFound は操作の記録がないことを表す（OPERATION_NOT_FOUND）。
	ErrOperationNotFound = errors.New("operation not found")
	// ErrOperationState は、操作の状態がその処理を受け付けないことを表す（OPERATION_STATE_CONFLICT）。
	ErrOperationState = errors.New("operation state does not allow this")
)

// dueBatch は、ワーカーが 1 回に処理する操作の上限。
const dueBatch = 100

// errSkipped は、ワーカーが操作を処理しなかった（時刻が来ていない、完了済み、自動では処理しない）ことを表す。
var errSkipped = errors.New("skipped")

// RunWorker は、interval ごとに処理してよい時刻を過ぎた未完了の操作を処理する。ctx が終わるまで戻らない。
// ctx が終わっても、処理中の操作は最後まで行ってから戻る（resume は下流の呼び出しを ctx の終わりから切り離している）。
// 呼び出し側は、停止のときにこの戻りを待つ。
func (s *Service) RunWorker(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.processDueSafely(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ProcessDue は、処理してよい時刻を過ぎた未完了の操作を処理し、処理した件数を返す。
// 実行中（running）の操作は、要求が落ちたとみなして処理する（要求がまだ IMSI のロックを持っていれば飛ばす）。
func (s *Service) ProcessDue(ctx context.Context) int {
	ids, err := s.Store.DueOperations(ctx, s.now(), dueBatch)
	if err != nil {
		s.Log.ErrorContext(ctx, "list due operations", "error", err)
		return 0
	}
	n := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		op, err := s.resumeSafely(ctx, id)
		switch {
		case errors.Is(err, store.ErrLocked), errors.Is(err, errSkipped):
			// 要求か別の処理がまだロックを持っている、または処理しなくてよい。次の回に回す。
		case err != nil:
			s.Log.ErrorContext(ctx, "resume operation", "operation_id", id, "error", err)
		default:
			n++
			s.Log.InfoContext(ctx, "resumed operation", "operation_id", id, "kind", op.Kind, "imsi", op.IMSI,
				"status", op.Status, "attempts", op.Attempts)
		}
	}
	return n
}

// errPanicked は、操作の処理中にパニックが起き、回復したことを表す。
var errPanicked = errors.New("panic while resuming operation")

// resumeSafely は、ワーカーから操作の続きを行う。処理中のパニックは回復してエラーにする（1 件の不具合でワーカーを
// 止めず、ほかの操作を続ける。パニックした操作は記録が残るので、次の回か手で対応する）。
func (s *Service) resumeSafely(ctx context.Context, id string) (op store.Operation, err error) {
	defer func() {
		if p := recover(); p != nil {
			s.Log.ErrorContext(ctx, "worker panic", "operation_id", id, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			err = errPanicked
		}
	}()
	return s.resume(ctx, id, false)
}

// processDueSafely は ProcessDue を呼び、操作の一覧の取得などで起きたパニックを回復する（ワーカーを止めない）。
func (s *Service) processDueSafely(ctx context.Context) {
	defer func() {
		if p := recover(); p != nil {
			s.Log.ErrorContext(ctx, "worker panic", "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
		}
	}()
	s.ProcessDue(ctx)
}

// resume は操作の続きを行う（作成・変更は補償、削除は残りの削除）。manual は、手でのやり直し（failed から）。
func (s *Service) resume(ctx context.Context, id string, manual bool) (store.Operation, error) {
	op, err := s.Store.GetOperation(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		// 記録が消えていた（完了して期限切れなど）。索引だけを外す。
		return store.Operation{}, errors.Join(ErrOperationNotFound, s.Store.RemoveActive(ctx, id))
	}
	if err != nil {
		return store.Operation{}, err
	}
	unlock, err := s.lock(ctx, op.IMSI)
	if err != nil {
		return store.Operation{}, err
	}
	defer unlock()
	// ロックを取る前に、要求が記録を進めていたかもしれないので読み直す。
	if op, err = s.Store.GetOperation(ctx, id); err != nil {
		return store.Operation{}, err
	}
	switch {
	case op.Final():
		if err := s.Store.RemoveActive(ctx, id); err != nil {
			return op, err
		}
		if manual {
			return op, ErrOperationState
		}
		return op, errSkipped
	case manual && op.Status != store.OpFailed:
		return op, ErrOperationState
	case !manual && (op.Status == store.OpFailed || s.now().Before(op.NextAttemptAt)):
		return op, errSkipped // 自動では処理しない、またはまだ時刻が来ていない
	}

	// 下流には、元の操作の操作者とトレースID を渡す（下流の記録と突き合わせられるように）。
	rctx := downstream.WithOperator(trace.With(context.WithoutCancel(ctx), op.TraceID), op.Operator)
	r := &run{s: s, ctx: rctx, op: op}
	if manual {
		// 手でのやり直しは、自動のやり直しの期限を今から数え直す。
		r.op.GiveUpAt = giveUpAt(s.now(), s.GiveUpAfter)
	}
	if r.op.Kind == KindUpdate && !r.hasRecoverySteps() && r.keyOutcomeUnknown() {
		// 鍵の変更が反映されたかどうか分からない（変更前の Ki / OPc は残さないので戻せない）。自動では何もせず、
		// 手での確認を待つ。
		r.op.Status = store.OpFailed
		r.saveLogged()
		s.Log.ErrorContext(ctx, "update operation needs a manual check: the key may or may not have been changed",
			"operation_id", id, "imsi", op.IMSI)
		s.audit(rctx, "operation.resume", r.op, Actor{TraceID: op.TraceID})
		return r.op, nil
	}
	r.plan()
	ok := r.compensate()
	r.op.Attempts++
	r.finish(ok)
	if !ok && !r.op.GiveUpAt.IsZero() && !s.now().Before(r.op.GiveUpAt) {
		r.op.Status = store.OpFailed
		s.Log.ErrorContext(ctx, "operation gave up retrying; manual action is required", "operation_id", id, "imsi", op.IMSI)
	}
	if err := r.save(); err != nil {
		return r.op, err
	}
	if !manual && (r.op.Final() || r.op.Status == store.OpFailed) {
		s.audit(rctx, "operation.resume", r.op, Actor{TraceID: op.TraceID})
	}
	return r.op, nil
}

// keyOutcomeUnknown は、変更の操作で、鍵の変更が反映されたかどうか分からないかを返す。
// 実行中のまま残った記録で、鍵の手順が終わっておらず、その前のポリシーの手順は済んでいる（またはない）とき。
// ポリシーの手順が終わっていなければ、鍵の手順には進んでいないので、ポリシーを戻せばよい。
func (r *run) keyOutcomeUnknown() bool {
	i := slices.IndexFunc(r.op.Steps, func(st store.OperationStep) bool { return st.Name == stepSubscriberUpdate })
	if i < 0 || r.op.Steps[i].State == store.StepDone {
		return false
	}
	return i == 0 || r.op.Steps[i-1].State == store.StepDone
}

// Retry は、failed の操作の続きをその場で 1 回行う。成功すれば完了し、失敗すれば retrying に戻って自動のやり直しを再開する。
// 鍵の変更が反映されたかどうか分からない変更は、自動ではやり直せない（ErrOperationState）。
func (s *Service) Retry(ctx context.Context, a Actor, id string) (store.Operation, error) {
	op, err := s.Store.GetOperation(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Operation{}, ErrOperationNotFound
	case err != nil:
		return store.Operation{}, err
	}
	if op.Kind == KindUpdate && op.Status == store.OpFailed {
		if r := (&run{s: s, op: op}); !r.hasRecoverySteps() && r.keyOutcomeUnknown() {
			return op, ErrOperationState
		}
	}
	op, err = s.resume(ctx, id, true)
	if err != nil {
		return op, err
	}
	s.audit(ctx, "operation.retry", op, a)
	return op, nil
}

// Dismiss は、retrying / failed の操作を、手で直した後に閉じる（dismissed）。下流には何もしない。
func (s *Service) Dismiss(ctx context.Context, a Actor, id string) (store.Operation, error) {
	op, err := s.Store.GetOperation(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Operation{}, ErrOperationNotFound
	case err != nil:
		return store.Operation{}, err
	}
	unlock, err := s.lock(ctx, op.IMSI)
	if err != nil {
		return store.Operation{}, err
	}
	defer unlock()
	if op, err = s.Store.GetOperation(ctx, id); err != nil {
		return store.Operation{}, err
	}
	if op.Status != store.OpRetrying && op.Status != store.OpFailed {
		return op, ErrOperationState
	}
	r := &run{s: s, ctx: ctx, op: op}
	r.op.Status = store.OpDismissed
	if err := r.save(); err != nil {
		return r.op, err
	}
	s.audit(ctx, "operation.dismiss", r.op, a)
	return r.op, nil
}

// Operation は操作の記録を取得する。
func (s *Service) Operation(ctx context.Context, id string) (store.Operation, error) {
	op, err := s.Store.GetOperation(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Operation{}, ErrOperationNotFound
	}
	return op, err
}

// Operations は未完了（running / retrying / failed）の操作を、作成の古い順に返す。status が空でなければその状態だけ。
// total は条件に一致する件数（items は limit 件まで）。
func (s *Service) Operations(ctx context.Context, status string, limit int) (items []store.Operation, total int, err error) {
	ids, err := s.Store.ActiveOperations(ctx)
	if err != nil {
		return nil, 0, err
	}
	slices.Sort(ids) // UUID version 7 は作成の時刻の順に並ぶ
	for _, id := range ids {
		op, err := s.Store.GetOperation(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && op.Final()) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		if status != "" && op.Status != status {
			continue
		}
		total++
		if len(items) < limit {
			items = append(items, op)
		}
	}
	return items, total, nil
}

// OperationCounts は未完了の操作の状態ごとの件数を返す（/status）。
func (s *Service) OperationCounts(ctx context.Context) (map[string]int, error) {
	items, _, err := s.Operations(ctx, "", 1<<30)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{store.OpRunning: 0, store.OpRetrying: 0, store.OpFailed: 0}
	for _, op := range items {
		counts[op.Status]++
	}
	return counts, nil
}

// audit は操作の記録への操作を監査ログに残す。
func (s *Service) audit(ctx context.Context, action string, op store.Operation, a Actor) {
	if s.Audit == nil {
		return
	}
	steps := make([]map[string]string, len(op.Steps))
	for i, st := range op.Steps {
		steps[i] = map[string]string{"name": st.Name, "downstream": st.Downstream, "state": st.State}
	}
	b, _ := json.Marshal(map[string]any{"kind": op.Kind, "imsi": op.IMSI, "steps": steps}, json.Deterministic(true))
	s.Audit.Record(ctx, store.AuditEntry{
		Operator: a.Operator, MgmtClient: a.MgmtClient, TraceID: a.TraceID, Action: action, Target: op.ID,
		OperationID: op.ID, Result: op.Status, Details: string(b),
	})
}
