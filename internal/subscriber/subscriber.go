// Package subscriber は加入者の統合操作（設計概要 §6）。加入者を「IMSI＋鍵の置き場所＋認可ポリシー」として扱い、
// 本PoCの Provisioning API（prov）と aka-vecgen の管理API（aka）を組み合わせて操作する。
// 複数の下流にまたがる作成・変更・削除は、操作の記録（store.Operation）を残しながら行い、途中で失敗したら
// 補償（作成・変更）またはやり直し（削除）で整える（設計概要 §9.3）。
//
// HTTP の入力の解釈と検証、エラーの応答の形は internal/api が受け持つ。
package subscriber

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/akaapi"
	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/plmn"
	"github.com/oyaguma3/aka-provisioner/internal/provapi"
	"github.com/oyaguma3/aka-provisioner/internal/store"
)

// Prov は使う provisioning-api の操作。*provapi.Client が満たす。
type Prov interface {
	GetSubscriber(ctx context.Context, imsi string) (provapi.Subscriber, error)
	CreateSubscriber(ctx context.Context, s provapi.SubscriberCreate) (provapi.Subscriber, error)
	UpdateSubscriber(ctx context.Context, imsi string, u provapi.SubscriberUpdate) (provapi.Subscriber, error)
	DeleteSubscriber(ctx context.Context, imsi string) error
	GetSubscriberKeys(ctx context.Context, imsi string) (provapi.SubscriberKeys, error)
	ListSubscribers(ctx context.Context, p downstream.ListParams) (provapi.SubscriberList, error)
	GetPolicy(ctx context.Context, imsi string) (provapi.Policy, error)
	PutPolicy(ctx context.Context, imsi string, p provapi.PolicyPut) (provapi.Policy, bool, error)
	DeletePolicy(ctx context.Context, imsi string) error
	ListPolicies(ctx context.Context, p downstream.ListParams) (provapi.PolicyList, error)
}

// Aka は使う aka-vecgen の管理API の操作。*akaapi.Client が満たす。
type Aka interface {
	GetSubscriber(ctx context.Context, imsi string) (akaapi.Subscriber, error)
	CreateSubscriber(ctx context.Context, s akaapi.SubscriberCreate) (akaapi.Subscriber, error)
	UpdateSubscriber(ctx context.Context, imsi string, u akaapi.SubscriberUpdate) (akaapi.Subscriber, error)
	DeleteSubscriber(ctx context.Context, imsi string) error
	GetSubscriberKeys(ctx context.Context, imsi string) (akaapi.SubscriberKeys, error)
	ListSubscribers(ctx context.Context, p downstream.ListParams) (akaapi.SubscriberList, error)
}

// Store は使う provisioner 専用 Valkey の操作。*store.Store が満たす。
type Store interface {
	AcquireIMSILock(ctx context.Context, imsi string, ttl time.Duration) (token string, err error)
	ReleaseIMSILock(ctx context.Context, imsi, token string) error
	SaveOperation(ctx context.Context, op store.Operation, retention time.Duration) error
	GetOperation(ctx context.Context, id string) (store.Operation, error)
	DueOperations(ctx context.Context, now time.Time, limit int) ([]string, error)
	ActiveOperations(ctx context.Context) ([]string, error)
	RemoveActive(ctx context.Context, id string) error
}

// Auditor は監査ログを残す。*audit.Recorder が満たす。
type Auditor interface {
	Record(ctx context.Context, e store.AuditEntry)
}

// Service は加入者の統合操作。
type Service struct {
	Prov Prov
	// Aka は aka-vecgen を扱わない設定なら nil（そのとき PLMN マップは aka を含まない）。
	Aka Aka
	// AVClientID は、本PoCの vector-gateway の AVクライアントID。
	AVClientID int64
	PLMN       plmn.Map
	Store      Store
	Log        *slog.Logger

	// LockTTL は IMSI ごとのロックの有効期限。実行中の操作の記録は、これを過ぎたら要求が落ちたとみなされる。
	LockTTL time.Duration
	// Retention は、完了した操作の記録を残す期間。
	Retention time.Duration
	// RetryDelay は、補償・やり直しが失敗したとき、次に試みるまでの時間。失敗するたびに倍にする。
	RetryDelay time.Duration
	// MaxRetryDelay は、次に試みるまでの時間の上限。
	MaxRetryDelay time.Duration
	// GiveUpAfter は、操作の作成からこの時間を過ぎても終わらなければ、自動のやり直しをやめて failed にする時間。
	GiveUpAfter time.Duration
	// Audit は、ワーカーと操作の記録への操作の監査ログを残す。nil なら残さない。
	Audit Auditor
	// Now は現在時刻（テストで差し替える）。nil なら time.Now。
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Actor は操作の要求者（操作の記録と監査ログに残す）。下流に渡す操作者とトレースID はコンテキストに入れる。
type Actor struct {
	Operator   string
	MgmtClient string
	TraceID    string
}

// Issue は 2 つのノードの状態の食い違い（設計概要 §6.1）。
type Issue string

const (
	IssueKeyMissing            Issue = "KEY_MISSING"
	IssuePolicyMissing         Issue = "POLICY_MISSING"
	IssueKeyInOtherStore       Issue = "KEY_IN_OTHER_STORE"
	IssueAVClientNotAllowed    Issue = "AV_CLIENT_NOT_ALLOWED"
	IssueOtherStoreUnreachable Issue = "OTHER_STORE_UNREACHABLE"
)

// Key は置き場所の加入者の属性（Ki と OPc は含まない）。SQNType 以降は aka のときだけ。
type Key struct {
	AMF              string
	SQN              string
	SQNType          string
	AllowPlain       *bool
	AllowedClientIDs []int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Subscriber は加入者（設計概要 §6.1）。
type Subscriber struct {
	IMSI     string
	KeyStore plmn.KeyStore
	// Key は置き場所の加入者。ない場合は nil。
	Key *Key
	// Policy は認可ポリシー。ない場合は nil。
	Policy *provapi.PolicyPut
	// Status は認可ポリシーの状態（active / suspended）。ポリシーがない場合は空文字列。
	Status string
	Issues []Issue
}

// ---- エラー ----

// ErrNotFound は加入者が存在しないことを表す（USER_NOT_FOUND）。
var ErrNotFound = errors.New("subscriber not found")

// ConflictError は、作成で同じ IMSI のものが既にあったことを表す（SUBSCRIBER_ALREADY_EXISTS）。
type ConflictError struct {
	// Places はあった場所（poc / aka / policy）。
	Places []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("subscriber already exists in %v", e.Places)
}

// UnresolvedError は、同じ IMSI に未完了（running / retrying / failed）の操作の記録があるため、新しい操作を断ったことを表す
// （OPERATION_UNRESOLVED）。古い操作の続き（補償・やり直し）が新しい操作の結果を消さないようにする（設計概要 §9.3）。
type UnresolvedError struct {
	// OperationID は未完了の操作の ID（複数あれば最も古いもの）。
	OperationID string
	// Status はその操作の状態。
	Status string
}

func (e *UnresolvedError) Error() string {
	return fmt.Sprintf("operation %s on the same IMSI is unresolved (%s)", e.OperationID, e.Status)
}

// DownstreamError は下流の呼び出しの失敗。
type DownstreamError struct {
	Name downstream.Name
	// ParamPrefix は、下流の invalidParams の param に付ける接頭辞（認可ポリシーなら "policy."）。
	ParamPrefix string
	Err         error
}

func (e *DownstreamError) Error() string { return e.Err.Error() }
func (e *DownstreamError) Unwrap() error { return e.Err }

// OperationError は、下流への書き込みを始めた後の失敗（操作の記録がある）。
type OperationError struct {
	OperationID string
	// RolledBack は、補償で元に戻したか。
	RolledBack bool
	// Incomplete は、補償（作成・変更）または残りの削除が終わっておらず、後でやり直すか。
	Incomplete bool
	// Err は最初の失敗。
	Err error
}

func (e *OperationError) Error() string {
	return fmt.Sprintf("operation %s (rolled back %v, incomplete %v): %v", e.OperationID, e.RolledBack, e.Incomplete, e.Err)
}
func (e *OperationError) Unwrap() error { return e.Err }

// ---- 状態の読み込み ----

// state は、ある IMSI の 3 か所（prov の加入者、aka の加入者、認可ポリシー）の状態。ないものは nil。
type state struct {
	keyStore plmn.KeyStore
	prov     *provapi.Subscriber
	aka      *akaapi.Subscriber
	policy   *provapi.Policy
	// otherUnreachable は、置き場所でない方の aka に接続できず、確かめられなかったこと。
	otherUnreachable bool
}

func isNotFound(err error) bool {
	apiErr, ok := errors.AsType[*downstream.Error](err)
	return ok && apiErr.Status == http.StatusNotFound
}

// load は 3 か所を並行して読む。置き場所でない方の aka だけが読めなければ otherUnreachable にする。
func (s *Service) load(ctx context.Context, imsi string) (state, error) {
	st := state{keyStore: s.PLMN.KeyStore(imsi)}
	var provErr, policyErr, akaErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		v, err := s.Prov.GetSubscriber(ctx, imsi)
		switch {
		case err == nil:
			st.prov = &v
		case !isNotFound(err):
			provErr = err
		}
	})
	wg.Go(func() {
		v, err := s.Prov.GetPolicy(ctx, imsi)
		switch {
		case err == nil:
			st.policy = &v
		case !isNotFound(err):
			policyErr = err
		}
	})
	if s.Aka != nil {
		wg.Go(func() {
			v, err := s.Aka.GetSubscriber(ctx, imsi)
			switch {
			case err == nil:
				st.aka = &v
			case !isNotFound(err):
				akaErr = err
			}
		})
	}
	wg.Wait()
	if err := cmpErr(provErr, policyErr); err != nil {
		return state{}, &DownstreamError{Name: downstream.Prov, Err: err}
	}
	if akaErr != nil {
		if st.keyStore == plmn.KeyStoreAKA {
			return state{}, &DownstreamError{Name: downstream.Aka, Err: akaErr}
		}
		s.Log.WarnContext(ctx, "could not check aka-vecgen for the subscriber", "imsi", imsi, "error", akaErr)
		st.otherUnreachable = true
	}
	return st, nil
}

func cmpErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// allowsAV は aka の加入者が vector-gateway の AVクライアントID を許可しているか。
func (s *Service) allowsAV(a *akaapi.Subscriber) bool {
	return a != nil && slices.Contains(a.AllowedClientIDs, s.AVClientID)
}

// exists は、3 か所のどこかに、この加入者のものがあるか。poc の IMSI の aka 側の加入者は、
// vector-gateway の AVクライアントID を許可しているものだけを数える（それ以外は本PoCと関係のない加入者とみなす）。
func (s *Service) exists(st state) bool {
	if st.prov != nil || st.policy != nil {
		return true
	}
	if st.keyStore == plmn.KeyStoreAKA {
		return st.aka != nil
	}
	return s.allowsAV(st.aka)
}

// view は状態から加入者を組み立てる。
func (s *Service) view(imsi string, st state) Subscriber {
	sub := Subscriber{IMSI: imsi, KeyStore: st.keyStore, Issues: []Issue{}}
	switch st.keyStore {
	case plmn.KeyStoreAKA:
		if st.aka != nil {
			sub.Key = akaKey(*st.aka)
			if !s.allowsAV(st.aka) {
				sub.Issues = append(sub.Issues, IssueAVClientNotAllowed)
			}
		}
		if st.prov != nil {
			sub.Issues = append(sub.Issues, IssueKeyInOtherStore)
		}
	default:
		if st.prov != nil {
			sub.Key = provKey(*st.prov)
		}
		if s.allowsAV(st.aka) {
			sub.Issues = append(sub.Issues, IssueKeyInOtherStore)
		}
		if st.otherUnreachable {
			sub.Issues = append(sub.Issues, IssueOtherStoreUnreachable)
		}
	}
	if sub.Key == nil {
		sub.Issues = append([]Issue{IssueKeyMissing}, sub.Issues...)
	}
	if st.policy != nil {
		sub.Policy = &provapi.PolicyPut{Default: st.policy.Default, Rules: st.policy.Rules}
		sub.Status = st.policy.Status
	} else {
		sub.Issues = append(sub.Issues, IssuePolicyMissing)
	}
	return sub
}

func provKey(p provapi.Subscriber) *Key {
	return &Key{AMF: p.AMF, SQN: p.SQN, CreatedAt: p.CreatedAt}
}

func akaKey(a akaapi.Subscriber) *Key {
	return &Key{
		AMF: a.AMF, SQN: a.SQN, SQNType: a.SQNType, AllowPlain: new(a.AllowPlain),
		AllowedClientIDs: append([]int64{}, a.AllowedClientIDs...), CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
}

// Get は加入者を取得する。3 か所のどこにもなければ ErrNotFound。
func (s *Service) Get(ctx context.Context, imsi string) (Subscriber, error) {
	st, err := s.load(ctx, imsi)
	if err != nil {
		return Subscriber{}, err
	}
	if !s.exists(st) {
		return Subscriber{}, ErrNotFound
	}
	return s.view(imsi, st), nil
}

// Keys は置き場所から Ki と OPc を取得する。置き場所に加入者がなければ ErrNotFound。
func (s *Service) Keys(ctx context.Context, imsi string) (ki, opc string, err error) {
	if s.PLMN.KeyStore(imsi) == plmn.KeyStoreAKA {
		k, err := s.Aka.GetSubscriberKeys(ctx, imsi)
		return k.Ki, k.OPc, keyErr(downstream.Aka, err)
	}
	k, err := s.Prov.GetSubscriberKeys(ctx, imsi)
	return k.Ki, k.OPc, keyErr(downstream.Prov, err)
}

func keyErr(name downstream.Name, err error) error {
	switch {
	case err == nil:
		return nil
	case isNotFound(err):
		return ErrNotFound
	}
	return &DownstreamError{Name: name, Err: err}
}
