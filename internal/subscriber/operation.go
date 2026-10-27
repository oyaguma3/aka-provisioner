package subscriber

import (
	"context"
	"encoding/json/v2"
	"errors"
	"slices"
	"time"
	"uuid"

	"github.com/oyaguma3/aka-provisioner/internal/akaapi"
	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/plmn"
	"github.com/oyaguma3/aka-provisioner/internal/provapi"
	"github.com/oyaguma3/aka-provisioner/internal/store"
)

// 操作の種類。
const (
	KindCreate = "subscriber.create"
	KindUpdate = "subscriber.update"
	KindDelete = "subscriber.delete"
)

// 手順の名前（docs/openapi/provisioner-api.yaml の OperationStep）。
const (
	stepSubscriberCreate     = "subscriber.create"
	stepSubscriberUpdate     = "subscriber.update"
	stepSubscriberDelete     = "subscriber.delete"
	stepSubscriberCompensate = "subscriber.compensate" // 作成の補償。作った加入者を消す
	stepPolicyPut            = "policy.put"
	stepPolicyDelete         = "policy.delete"
	stepPolicyRestore        = "policy.restore" // 変更の補償。変更前のポリシーに戻す
)

const storeTimeout = 5 * time.Second

// CreateInput は加入者の作成の内容（internal/api で検証済み）。
type CreateInput struct {
	IMSI string
	Ki   string
	OPc  string
	// AMF と SQN は空なら下流の既定値。
	AMF string
	SQN string
	// SQNType と AllowPlain は置き場所が aka のときだけ（空・nil なら aka-vecgen の既定値）。
	SQNType    string
	AllowPlain *bool
	Policy     provapi.PolicyPut
}

// UpdateInput は加入者の変更の内容（internal/api で検証済み）。nil の項目は変更しない。
type UpdateInput struct {
	Ki         *string
	OPc        *string
	AMF        *string
	SQN        *string
	SQNType    *string
	AllowPlain *bool
	// Policy は認可ポリシー全体を置き換える。
	Policy *provapi.PolicyPut
}

// HasKey は鍵の項目（置き場所の加入者の変更）を含むかを返す。
func (u UpdateInput) HasKey() bool {
	return u.Ki != nil || u.OPc != nil || u.AMF != nil || u.SQN != nil || u.SQNType != nil || u.AllowPlain != nil
}

// Result は書き込みの操作の結果（監査ログに残す）。操作の記録を作る前に断った場合は空。
type Result struct {
	OperationID string
	Status      string
	Steps       []store.OperationStep
}

// ---- 操作の記録 ----

// run は 1 つの操作の実行。
type run struct {
	s   *Service
	ctx context.Context
	op  store.Operation
}

type stepSpec struct {
	name       string
	downstream downstream.Name
}

func (s *Service) newRun(ctx context.Context, a Actor, kind, imsi string, ks plmn.KeyStore, steps ...stepSpec) *run {
	now := s.now()
	op := store.Operation{
		ID: uuid.NewV7().String(), Kind: kind, IMSI: imsi, KeyStore: string(ks), Status: store.OpRunning,
		// 実行中の操作は、ロックの有効期限を過ぎたら要求が落ちたとみなされる（設計概要 §9.3）。
		NextAttemptAt: now.Add(s.LockTTL),
		GiveUpAt:      giveUpAt(now, s.GiveUpAfter),
		Operator:      a.Operator, MgmtClient: a.MgmtClient, TraceID: a.TraceID, CreatedAt: now,
	}
	for _, sp := range steps {
		op.Steps = append(op.Steps, store.OperationStep{Name: sp.name, Downstream: string(sp.downstream), State: store.StepPending})
	}
	return &run{s: s, ctx: ctx, op: op}
}

// save は操作の記録を書く。要求が途中で切れても書く。
func (r *run) save() error {
	r.op.UpdatedAt = r.s.now()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), storeTimeout)
	defer cancel()
	return r.s.Store.SaveOperation(ctx, r.op, r.s.Retention)
}

// saveLogged は操作の記録を書き、失敗したらログに残す（操作は続ける）。
func (r *run) saveLogged() {
	if err := r.save(); err != nil {
		r.s.Log.ErrorContext(r.ctx, "save operation", "operation_id", r.op.ID, "error", err)
	}
}

func (r *run) result() Result {
	return Result{OperationID: r.op.ID, Status: r.op.Status, Steps: append([]store.OperationStep(nil), r.op.Steps...)}
}

func (r *run) setDone(i int) { r.op.Steps[i].State, r.op.Steps[i].Error = store.StepDone, nil }

func (r *run) setFailed(i int, err error) {
	r.op.Steps[i].State = store.StepFailed
	r.op.Steps[i].Error = stepError(err, r.s.now())
}

// skipPending は、要求の中で失敗したとき、まだ行っていない手順を「不要だった」にする。
func (r *run) skipPending() {
	for i := range r.op.Steps {
		if r.op.Steps[i].State == store.StepPending {
			r.op.Steps[i].State = store.StepSkipped
		}
	}
}

func (r *run) add(name string, ds downstream.Name) {
	r.op.Steps = append(r.op.Steps, store.OperationStep{Name: name, Downstream: string(ds), State: store.StepPending})
}

func stepError(err error, now time.Time) *store.StepError {
	e := &store.StepError{Detail: err.Error(), Time: now}
	if apiErr, ok := errors.AsType[*downstream.Error](err); ok {
		e.Status, e.Cause = apiErr.Status, apiErr.Problem.Cause
	} else if downstream.IsUnavailable(err) {
		e.Cause = "DOWNSTREAM_UNAVAILABLE"
	}
	return e
}

// mayHaveTakenEffect は、手順が下流に反映された可能性があるかを返す。
// 下流が 4xx で断った場合だけ「反映されていない」と分かる。接続できない・タイムアウト・5xx は、
// 書き込まれたかどうか分からないので、反映された可能性があるとみなす（補償は既にないものの削除を成功とするので、何度行っても安全）。
func mayHaveTakenEffect(st store.OperationStep) bool {
	switch st.State {
	case store.StepDone, store.StepPending:
		return true
	case store.StepFailed:
		return st.Error == nil || st.Error.Status < 400 || st.Error.Status >= 500
	}
	return false
}

// compensate は、まだ終わっていない補償・やり直しの手順を順に行う。全部終われば true。
func (r *run) compensate() bool {
	for i := range r.op.Steps {
		st := &r.op.Steps[i]
		if st.State != store.StepPending && st.State != store.StepFailed {
			continue
		}
		if !isRecoveryStep(r.op.Kind, st.Name) {
			continue
		}
		skipped, err := r.s.exec(r.ctx, &r.op, st.Name, downstream.Name(st.Downstream))
		switch {
		case err != nil:
			r.setFailed(i, err)
			r.saveLogged()
			return false
		case skipped:
			st.State, st.Error = store.StepSkipped, nil
		default:
			r.setDone(i)
		}
		r.saveLogged()
	}
	return true
}

// isRecoveryStep は、補償・やり直しで行う手順かを返す（削除は、元の手順そのものをやり直す）。
func isRecoveryStep(kind, name string) bool {
	switch name {
	case stepSubscriberCompensate, stepPolicyRestore:
		return true
	case stepPolicyDelete:
		return true
	case stepSubscriberDelete:
		return kind == KindDelete
	}
	return false
}

// exec は補償・削除の手順を 1 つ行う。対象が既になければ skipped を返す（成功として扱う）。
func (s *Service) exec(ctx context.Context, op *store.Operation, name string, ds downstream.Name) (skipped bool, err error) {
	switch name {
	case stepPolicyDelete:
		err = s.Prov.DeletePolicy(ctx, op.IMSI)
	case stepSubscriberDelete, stepSubscriberCompensate:
		if ds == downstream.Aka {
			err = s.Aka.DeleteSubscriber(ctx, op.IMSI)
		} else {
			err = s.Prov.DeleteSubscriber(ctx, op.IMSI)
		}
	case stepPolicyRestore:
		if !op.HadPolicy {
			err = s.Prov.DeletePolicy(ctx, op.IMSI)
			break
		}
		var prev provapi.PolicyPut
		if err := json.Unmarshal([]byte(op.PrevPolicy), &prev); err != nil {
			return false, err
		}
		_, _, err = s.Prov.PutPolicy(ctx, op.IMSI, prev)
	}
	if isNotFound(err) {
		return true, nil
	}
	return false, err
}

// rollback は、作成・変更が要求の中で失敗したときに補償する。cause は最初の失敗。
func (r *run) rollback(cause error) (Subscriber, Result, error) {
	r.skipPending()
	r.plan()
	ok := r.compensate()
	r.finish(ok)
	if err := r.save(); err != nil {
		r.s.Log.ErrorContext(r.ctx, "save operation", "operation_id", r.op.ID, "error", err)
	}
	if !ok {
		r.s.Log.ErrorContext(r.ctx, "compensation failed; will retry", "operation_id", r.op.ID, "imsi", r.op.IMSI, "kind", r.op.Kind)
	}
	return Subscriber{}, r.result(), &OperationError{OperationID: r.op.ID, RolledBack: ok, Incomplete: !ok, Err: cause}
}

// hasRecoverySteps は、補償の手順を既に計画したかを返す。
func (r *run) hasRecoverySteps() bool {
	for _, st := range r.op.Steps {
		if st.Name == stepSubscriberCompensate || st.Name == stepPolicyRestore || (r.op.Kind == KindCreate && st.Name == stepPolicyDelete) {
			return true
		}
	}
	return false
}

// plan は、作成・変更の補償の手順を計画して加える（既に計画してあれば何もしない）。
// まだ行っていない（pending の）手順は、反映された可能性があるものとして扱う（要求の中では先に skipPending する）。
func (r *run) plan() {
	if r.hasRecoverySteps() {
		return
	}
	switch r.op.Kind {
	case KindCreate:
		// 作成は、反映された可能性のあるものを消す（ポリシー、加入者の順）。存在確認で、どちらもなかったことを確かめてある。
		if mayHaveTakenEffect(r.op.Steps[1]) {
			r.add(stepPolicyDelete, downstream.Prov)
		}
		if mayHaveTakenEffect(r.op.Steps[0]) {
			r.add(stepSubscriberCompensate, downstream.Name(r.op.Steps[0].Downstream))
		}
	case KindUpdate:
		// 変更は、ポリシーだけを変更前に戻す（変更前の Ki / OPc は残さないので、鍵の変更は戻せない。設計概要 §6.2）。
		if len(r.op.Steps) > 0 && r.op.Steps[0].Name == stepPolicyPut && mayHaveTakenEffect(r.op.Steps[0]) {
			r.add(stepPolicyRestore, downstream.Prov)
		}
	}
}

// finish は、補償・やり直しの結果から操作の状態を決める。ok なら完了（作成・変更は rolled_back、削除は completed）、
// そうでなければ retrying にして、次に試みる時刻を決める。
func (r *run) finish(ok bool) {
	if !ok {
		r.op.Status = store.OpRetrying
		r.op.NextAttemptAt = r.s.now().Add(r.s.retryDelay(r.op.Attempts))
		return
	}
	if r.op.Kind == KindDelete {
		r.op.Status = store.OpCompleted
		return
	}
	r.op.Status = store.OpRolledBack
	for i := range r.op.Steps {
		if r.op.Steps[i].State == store.StepDone && !isRecoveryStep(r.op.Kind, r.op.Steps[i].Name) {
			r.op.Steps[i].State = store.StepCompensated
		}
	}
}

// giveUpAt は、now から after 後の時刻（after が 0 なら、やめない＝ゼロ値）。
func giveUpAt(now time.Time, after time.Duration) time.Time {
	if after <= 0 {
		return time.Time{}
	}
	return now.Add(after)
}

// retryDelay は、attempts 回試みた後に次に試みるまでの時間（RetryDelay から倍にして、MaxRetryDelay まで）。
func (s *Service) retryDelay(attempts int) time.Duration {
	d := s.RetryDelay
	for range attempts {
		d *= 2
		if s.MaxRetryDelay > 0 && d >= s.MaxRetryDelay {
			return s.MaxRetryDelay
		}
	}
	return d
}

// lock は IMSI のロックを取る。取れなければ store.ErrLocked。
func (s *Service) lock(ctx context.Context, imsi string) (unlock func(), err error) {
	token, err := s.Store.AcquireIMSILock(ctx, imsi, s.LockTTL)
	if err != nil {
		return nil, err
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
		defer cancel()
		if err := s.Store.ReleaseIMSILock(ctx, imsi, token); err != nil {
			s.Log.ErrorContext(ctx, "release imsi lock", "imsi", imsi, "error", err)
		}
	}, nil
}

// lockForNew は、新しい操作のために IMSI のロックを取り、同じ IMSI に未完了の操作がないことを確かめる。
// 未完了の操作があれば、ロックを解放して *UnresolvedError を返す。
func (s *Service) lockForNew(ctx context.Context, imsi string) (unlock func(), err error) {
	unlock, err = s.lock(ctx, imsi)
	if err != nil {
		return nil, err
	}
	if err := s.CheckUnresolved(ctx, imsi); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// CheckUnresolved は、imsi に未完了（running / retrying / failed）の操作の記録があれば *UnresolvedError を返す。
// 呼び出し側は IMSI のロックを取ってから呼ぶ（ロックの間は、同じ IMSI の記録は増えない。ロックを持つ実行中の操作もない）。
// このため running の記録は、要求が落ちてワーカーが続きを行うのを待っているものである。
func (s *Service) CheckUnresolved(ctx context.Context, imsi string) error {
	ids, err := s.Store.ActiveOperations(ctx)
	if err != nil {
		return err
	}
	slices.Sort(ids) // UUID version 7 は作成の時刻の順に並ぶ
	for _, id := range ids {
		op, err := s.Store.GetOperation(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if op.IMSI == imsi && !op.Final() {
			return &UnresolvedError{OperationID: op.ID, Status: op.Status}
		}
	}
	return nil
}

func dsOf(ks plmn.KeyStore) downstream.Name {
	if ks == plmn.KeyStoreAKA {
		return downstream.Aka
	}
	return downstream.Prov
}

// ---- 作成 ----

// Create は加入者を作成する（設計概要 §6.2）。置き場所に加入者を作り、認可ポリシーを PUT する。
// 途中で失敗したら補償で戻し、*OperationError を返す。
func (s *Service) Create(ctx context.Context, a Actor, in CreateInput) (Subscriber, Result, error) {
	unlock, err := s.lockForNew(ctx, in.IMSI)
	if err != nil {
		return Subscriber{}, Result{}, err
	}
	defer unlock()
	// 書き込みを始めたら、要求が切れても最後まで行う（途中で止めると補償が要る状態が残る）。
	ctx = context.WithoutCancel(ctx)

	st, err := s.load(ctx, in.IMSI)
	if err != nil {
		return Subscriber{}, Result{}, err
	}
	var conflicts []string
	if st.prov != nil {
		conflicts = append(conflicts, "poc")
	}
	if st.aka != nil && (st.keyStore == plmn.KeyStoreAKA || s.allowsAV(st.aka)) {
		conflicts = append(conflicts, "aka")
	}
	if st.policy != nil {
		conflicts = append(conflicts, "policy")
	}
	if len(conflicts) > 0 {
		return Subscriber{}, Result{}, &ConflictError{Places: conflicts}
	}

	ds := dsOf(st.keyStore)
	r := s.newRun(ctx, a, KindCreate, in.IMSI, st.keyStore,
		stepSpec{stepSubscriberCreate, ds}, stepSpec{stepPolicyPut, downstream.Prov})
	if err := r.save(); err != nil {
		return Subscriber{}, Result{}, err // 下流には何もしていない
	}

	if err := s.createKey(ctx, &st, in); err != nil {
		r.setFailed(0, err)
		return r.rollback(&DownstreamError{Name: ds, Err: err})
	}
	r.setDone(0)
	if err := r.save(); err != nil {
		// 記録を残せないまま進めると、補償が要るかどうかが分からなくなるので、戻す。
		return r.rollback(err)
	}

	pol, _, err := s.Prov.PutPolicy(ctx, in.IMSI, in.Policy)
	if err != nil {
		r.setFailed(1, err)
		return r.rollback(&DownstreamError{Name: downstream.Prov, ParamPrefix: "policy.", Err: err})
	}
	r.setDone(1)
	st.policy = &pol
	r.op.Status = store.OpCompleted
	if err := r.save(); err != nil {
		// 「完了」を残せないと、要求が落ちた操作として後で補償されてしまうので、ここで戻す。
		r.op.Status = store.OpRunning
		return r.rollback(err)
	}
	return s.view(in.IMSI, st), r.result(), nil
}

// createKey は置き場所に加入者を作り、st に入れる。
func (s *Service) createKey(ctx context.Context, st *state, in CreateInput) error {
	if st.keyStore == plmn.KeyStoreAKA {
		c := akaapi.SubscriberCreate{
			IMSI: in.IMSI, Ki: in.Ki, OPc: in.OPc, AMF: in.AMF, SQN: in.SQN, SQNType: in.SQNType,
			AllowedClientIDs: []int64{s.AVClientID},
		}
		if in.AllowPlain != nil {
			c.AllowPlain = *in.AllowPlain
		}
		v, err := s.Aka.CreateSubscriber(ctx, c)
		if err == nil {
			st.aka = &v
		}
		return err
	}
	v, err := s.Prov.CreateSubscriber(ctx, provapi.SubscriberCreate{IMSI: in.IMSI, Ki: in.Ki, OPc: in.OPc, AMF: in.AMF, SQN: in.SQN})
	if err == nil {
		st.prov = &v
	}
	return err
}

// ---- 変更 ----

// Update は加入者を変更する（設計概要 §6.2）。認可ポリシーを先に PUT し、戻せない鍵の変更を最後に行う。
// 鍵の変更が失敗したら、ポリシーを変更前に戻し、*OperationError を返す。
func (s *Service) Update(ctx context.Context, a Actor, imsi string, in UpdateInput) (Subscriber, Result, error) {
	unlock, err := s.lockForNew(ctx, imsi)
	if err != nil {
		return Subscriber{}, Result{}, err
	}
	defer unlock()
	ctx = context.WithoutCancel(ctx)

	st, err := s.load(ctx, imsi)
	if err != nil {
		return Subscriber{}, Result{}, err
	}
	if !s.exists(st) {
		return Subscriber{}, Result{}, ErrNotFound
	}
	if in.HasKey() && ((st.keyStore == plmn.KeyStoreAKA && st.aka == nil) || (st.keyStore == plmn.KeyStorePoC && st.prov == nil)) {
		return Subscriber{}, Result{}, ErrNotFound
	}

	ds := dsOf(st.keyStore)
	var steps []stepSpec
	if in.Policy != nil {
		steps = append(steps, stepSpec{stepPolicyPut, downstream.Prov})
	}
	if in.HasKey() {
		steps = append(steps, stepSpec{stepSubscriberUpdate, ds})
	}
	r := s.newRun(ctx, a, KindUpdate, imsi, st.keyStore, steps...)
	if st.policy != nil {
		b, err := json.Marshal(provapi.PolicyPut{Default: st.policy.Default, Rules: st.policy.Rules})
		if err != nil {
			return Subscriber{}, Result{}, err
		}
		r.op.PrevPolicy, r.op.HadPolicy = string(b), true
	}
	if err := r.save(); err != nil {
		return Subscriber{}, Result{}, err
	}

	i := 0
	if in.Policy != nil {
		pol, _, err := s.Prov.PutPolicy(ctx, imsi, *in.Policy)
		if err != nil {
			r.setFailed(i, err)
			return r.rollback(&DownstreamError{Name: downstream.Prov, ParamPrefix: "policy.", Err: err})
		}
		r.setDone(i)
		st.policy = &pol
		if err := r.save(); err != nil {
			return r.rollback(err)
		}
		i++
	}
	if in.HasKey() {
		if err := s.updateKey(ctx, &st, imsi, in); err != nil {
			r.setFailed(i, err)
			return r.rollback(&DownstreamError{Name: ds, Err: err})
		}
		r.setDone(i)
	}
	r.op.Status = store.OpCompleted
	if err := r.save(); err != nil {
		if in.HasKey() {
			// 鍵の変更は戻せないので、補償せずに変更は終わったものとして返す。記録はもう 1 回だけ書き直し、
			// それでも書けなければ実行中のまま残る（ワーカーは鍵が変わったかどうか分からないものとして failed にし、
			// 手での確認を待つ。設計概要 §9.3）。
			if err := r.save(); err != nil {
				r.s.Log.ErrorContext(ctx, "save operation after key update; manual check will be required",
					"operation_id", r.op.ID, "imsi", imsi, "error", err)
			}
			return s.view(imsi, st), r.result(), nil
		}
		r.op.Status = store.OpRunning
		return r.rollback(err)
	}
	return s.view(imsi, st), r.result(), nil
}

// updateKey は置き場所の加入者を変更し、st に入れる。
func (s *Service) updateKey(ctx context.Context, st *state, imsi string, in UpdateInput) error {
	if st.keyStore == plmn.KeyStoreAKA {
		v, err := s.Aka.UpdateSubscriber(ctx, imsi, akaapi.SubscriberUpdate{
			Ki: in.Ki, OPc: in.OPc, SQN: in.SQN, AMF: in.AMF, SQNType: in.SQNType, AllowPlain: in.AllowPlain,
		})
		if err == nil {
			st.aka = &v
		}
		return err
	}
	v, err := s.Prov.UpdateSubscriber(ctx, imsi, provapi.SubscriberUpdate{Ki: in.Ki, OPc: in.OPc, AMF: in.AMF, SQN: in.SQN})
	if err == nil {
		st.prov = &v
	}
	return err
}

// ---- 削除 ----

// Delete は加入者を削除する（設計概要 §6.2）。認可ポリシー、置き場所の加入者の順に削除する。
// 削除は戻せないので、途中で失敗したら残りを後でやり直す（*OperationError の Incomplete）。
func (s *Service) Delete(ctx context.Context, a Actor, imsi string) (Result, error) {
	unlock, err := s.lockForNew(ctx, imsi)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	ctx = context.WithoutCancel(ctx)

	st, err := s.load(ctx, imsi)
	if err != nil {
		return Result{}, err
	}
	if !s.exists(st) {
		return Result{}, ErrNotFound
	}
	ds := dsOf(st.keyStore)
	r := s.newRun(ctx, a, KindDelete, imsi, st.keyStore,
		stepSpec{stepPolicyDelete, downstream.Prov}, stepSpec{stepSubscriberDelete, ds})
	if err := r.save(); err != nil {
		return Result{}, err
	}
	if !r.compensate() {
		r.finish(false)
		r.saveLogged()
		s.Log.ErrorContext(ctx, "delete failed; will retry", "operation_id", r.op.ID, "imsi", imsi)
		var cause error = errors.New("delete failed")
		for _, step := range r.op.Steps {
			if step.State == store.StepFailed && step.Error != nil {
				cause = &DownstreamError{Name: downstream.Name(step.Downstream), Err: stepErrorAsError(step)}
			}
		}
		return r.result(), &OperationError{OperationID: r.op.ID, Incomplete: true, Err: cause}
	}
	r.finish(true)
	// 「完了」を残せなくても、削除は済んでいる（後でやり直されても、既にないものの削除は成功になる）。
	r.saveLogged()
	return r.result(), nil
}

// stepErrorAsError は、記録した手順の失敗を、応答のためのエラーに戻す。
func stepErrorAsError(step store.OperationStep) error {
	e := step.Error
	if e.Status == 0 {
		return errors.New(e.Detail) // 接続できなかった（IsUnavailable で true になる）
	}
	return &downstream.Error{Downstream: downstream.Name(step.Downstream), Status: e.Status,
		Problem: downstream.Problem{Status: e.Status, Cause: e.Cause, Detail: e.Detail}}
}
