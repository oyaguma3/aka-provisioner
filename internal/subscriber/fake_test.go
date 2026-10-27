package subscriber

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/akaapi"
	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/plmn"
	"github.com/oyaguma3/aka-provisioner/internal/provapi"
	"github.com/oyaguma3/aka-provisioner/internal/store"
)

// 偽物の下流と Valkey。fault で、呼び出しごとに失敗を起こせる。

// fault は 1 回の呼び出しの失敗。applied が true なら、下流には反映したうえで失敗を返す（タイムアウトの再現）。
// before は呼び出しの最初に行う処理（停止の合図の再現など）、panics は呼び出しでパニックを起こす（ワーカーの回復の確認）。
type fault struct {
	err     error
	applied bool
	before  func()
	panics  bool
}

// faults は「操作名」ごとの失敗の予定（先頭から 1 回ずつ使う）。
type faults struct {
	mu    sync.Mutex
	plans map[string][]fault
	calls []string
}

func (f *faults) next(op string) (fault, bool) {
	f.mu.Lock()
	f.calls = append(f.calls, op)
	if len(f.plans[op]) == 0 {
		f.mu.Unlock()
		return fault{}, false
	}
	ft := f.plans[op][0]
	f.plans[op] = f.plans[op][1:]
	f.mu.Unlock()
	if ft.before != nil {
		ft.before()
	}
	if ft.panics {
		panic("injected panic: " + op)
	}
	return ft, true
}

func (f *faults) plan(op string, ft ...fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.plans == nil {
		f.plans = map[string][]fault{}
	}
	f.plans[op] = append(f.plans[op], ft...)
}

func (f *faults) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

var (
	errUnavailable = errors.New("dial tcp: i/o timeout")
	errBadRequest  = &downstream.Error{Status: 400, Problem: downstream.Problem{Cause: "MANDATORY_IE_INCORRECT",
		InvalidParams: []downstream.InvalidParam{{Param: "rules[0].allowedSsids", Reason: "at least one SSID required"}}}}
	errServer = &downstream.Error{Status: 500, Problem: downstream.Problem{Cause: "SYSTEM_FAILURE"}}
)

func notFound(cause string) error {
	return &downstream.Error{Status: 404, Problem: downstream.Problem{Cause: cause}}
}

func conflict() error {
	return &downstream.Error{Status: 409, Problem: downstream.Problem{Cause: "SUBSCRIBER_ALREADY_EXISTS"}}
}

type fakeProv struct {
	faults
	mu       sync.Mutex
	subs     map[string]provapi.Subscriber
	policies map[string]provapi.Policy
}

func newFakeProv() *fakeProv {
	return &fakeProv{subs: map[string]provapi.Subscriber{}, policies: map[string]provapi.Policy{}}
}

// do は失敗の予定に従う。applied なら apply を行ってから失敗を返す。
func (f *fakeProv) do(op string, apply func() error) error {
	ft, ok := f.next(op)
	if ok && !ft.applied {
		return ft.err
	}
	f.mu.Lock()
	err := apply()
	f.mu.Unlock()
	if ok {
		return ft.err
	}
	return err
}

func (f *fakeProv) GetSubscriber(_ context.Context, imsi string) (v provapi.Subscriber, err error) {
	err = f.do("prov.GetSubscriber", func() error {
		var ok bool
		if v, ok = f.subs[imsi]; !ok {
			return notFound("USER_NOT_FOUND")
		}
		return nil
	})
	return v, err
}

func (f *fakeProv) CreateSubscriber(_ context.Context, s provapi.SubscriberCreate) (v provapi.Subscriber, err error) {
	err = f.do("prov.CreateSubscriber", func() error {
		if _, ok := f.subs[s.IMSI]; ok {
			return conflict()
		}
		v = provapi.Subscriber{IMSI: s.IMSI, AMF: cmp.Or(s.AMF, "8000"), SQN: cmp.Or(s.SQN, "000000000000"), CreatedAt: time.Unix(1, 0).UTC()}
		f.subs[s.IMSI] = v
		return nil
	})
	return v, err
}

func (f *fakeProv) UpdateSubscriber(_ context.Context, imsi string, u provapi.SubscriberUpdate) (v provapi.Subscriber, err error) {
	err = f.do("prov.UpdateSubscriber", func() error {
		var ok bool
		if v, ok = f.subs[imsi]; !ok {
			return notFound("USER_NOT_FOUND")
		}
		if u.AMF != nil {
			v.AMF = strings.ToLower(*u.AMF)
		}
		if u.SQN != nil {
			v.SQN = strings.ToLower(*u.SQN)
		}
		f.subs[imsi] = v
		return nil
	})
	return v, err
}

func (f *fakeProv) DeleteSubscriber(_ context.Context, imsi string) error {
	return f.do("prov.DeleteSubscriber", func() error {
		if _, ok := f.subs[imsi]; !ok {
			return notFound("USER_NOT_FOUND")
		}
		delete(f.subs, imsi)
		return nil
	})
}

func (f *fakeProv) GetSubscriberKeys(_ context.Context, imsi string) (k provapi.SubscriberKeys, err error) {
	err = f.do("prov.GetSubscriberKeys", func() error {
		if _, ok := f.subs[imsi]; !ok {
			return notFound("USER_NOT_FOUND")
		}
		k = provapi.SubscriberKeys{Ki: "k-" + imsi, OPc: "o-" + imsi}
		return nil
	})
	return k, err
}

func (f *fakeProv) ListSubscribers(_ context.Context, p downstream.ListParams) (l provapi.SubscriberList, err error) {
	err = f.do("prov.ListSubscribers", func() error {
		var items []provapi.Subscriber
		for _, s := range f.subs {
			items = append(items, s)
		}
		slices.SortFunc(items, func(a, b provapi.Subscriber) int { return strings.Compare(a.IMSI, b.IMSI) })
		l.Items, l.NextCursor = page(items, func(s provapi.Subscriber) string { return s.IMSI }, p)
		return nil
	})
	return l, err
}

func (f *fakeProv) GetPolicy(_ context.Context, imsi string) (v provapi.Policy, err error) {
	err = f.do("prov.GetPolicy", func() error {
		var ok bool
		if v, ok = f.policies[imsi]; !ok {
			return notFound("POLICY_NOT_FOUND")
		}
		return nil
	})
	return v, err
}

func (f *fakeProv) PutPolicy(_ context.Context, imsi string, p provapi.PolicyPut) (v provapi.Policy, created bool, err error) {
	err = f.do("prov.PutPolicy", func() error {
		// provisioning-api と同じく、置き換えでは状態を変えない（新規は active）。
		prev, existed := f.policies[imsi]
		v, created = provapi.Policy{IMSI: imsi, Default: p.Default, Rules: p.Rules, Status: cmp.Or(prev.Status, provapi.PolicyActive)}, !existed
		f.policies[imsi] = v
		return nil
	})
	return v, created, err
}

func (f *fakeProv) DeletePolicy(_ context.Context, imsi string) error {
	return f.do("prov.DeletePolicy", func() error {
		if _, ok := f.policies[imsi]; !ok {
			return notFound("POLICY_NOT_FOUND")
		}
		delete(f.policies, imsi)
		return nil
	})
}

func (f *fakeProv) ListPolicies(_ context.Context, p downstream.ListParams) (l provapi.PolicyList, err error) {
	err = f.do("prov.ListPolicies", func() error {
		var items []provapi.Policy
		for _, v := range f.policies {
			items = append(items, v)
		}
		slices.SortFunc(items, func(a, b provapi.Policy) int { return strings.Compare(a.IMSI, b.IMSI) })
		l.Items, l.NextCursor = page(items, func(v provapi.Policy) string { return v.IMSI }, p)
		return nil
	})
	return l, err
}

type fakeAka struct {
	faults
	mu   sync.Mutex
	subs map[string]akaapi.Subscriber
}

func newFakeAka() *fakeAka { return &fakeAka{subs: map[string]akaapi.Subscriber{}} }

func (f *fakeAka) do(op string, apply func() error) error {
	ft, ok := f.next(op)
	if ok && !ft.applied {
		return ft.err
	}
	f.mu.Lock()
	err := apply()
	f.mu.Unlock()
	if ok {
		return ft.err
	}
	return err
}

func (f *fakeAka) GetSubscriber(_ context.Context, imsi string) (v akaapi.Subscriber, err error) {
	err = f.do("aka.GetSubscriber", func() error {
		var ok bool
		if v, ok = f.subs[imsi]; !ok {
			return notFound("USER_NOT_FOUND")
		}
		return nil
	})
	return v, err
}

func (f *fakeAka) CreateSubscriber(_ context.Context, s akaapi.SubscriberCreate) (v akaapi.Subscriber, err error) {
	err = f.do("aka.CreateSubscriber", func() error {
		if _, ok := f.subs[s.IMSI]; ok {
			return conflict()
		}
		v = akaapi.Subscriber{IMSI: s.IMSI, AMF: cmp.Or(s.AMF, "8000"), SQN: cmp.Or(s.SQN, "000000000000"),
			SQNType: cmp.Or(s.SQNType, "inc32"), AllowPlain: s.AllowPlain, AllowedClientIDs: s.AllowedClientIDs}
		f.subs[s.IMSI] = v
		return nil
	})
	return v, err
}

func (f *fakeAka) UpdateSubscriber(_ context.Context, imsi string, u akaapi.SubscriberUpdate) (v akaapi.Subscriber, err error) {
	err = f.do("aka.UpdateSubscriber", func() error {
		var ok bool
		if v, ok = f.subs[imsi]; !ok {
			return notFound("USER_NOT_FOUND")
		}
		if u.SQNType != nil {
			v.SQNType = *u.SQNType
		}
		if u.AllowPlain != nil {
			v.AllowPlain = *u.AllowPlain
		}
		f.subs[imsi] = v
		return nil
	})
	return v, err
}

func (f *fakeAka) DeleteSubscriber(_ context.Context, imsi string) error {
	return f.do("aka.DeleteSubscriber", func() error {
		if _, ok := f.subs[imsi]; !ok {
			return notFound("USER_NOT_FOUND")
		}
		delete(f.subs, imsi)
		return nil
	})
}

func (f *fakeAka) GetSubscriberKeys(_ context.Context, imsi string) (k akaapi.SubscriberKeys, err error) {
	err = f.do("aka.GetSubscriberKeys", func() error {
		if _, ok := f.subs[imsi]; !ok {
			return notFound("USER_NOT_FOUND")
		}
		k = akaapi.SubscriberKeys{Ki: "k-" + imsi, OPc: "o-" + imsi}
		return nil
	})
	return k, err
}

func (f *fakeAka) ListSubscribers(_ context.Context, p downstream.ListParams) (l akaapi.SubscriberList, err error) {
	err = f.do("aka.ListSubscribers", func() error {
		var items []akaapi.Subscriber
		for _, s := range f.subs {
			items = append(items, s)
		}
		slices.SortFunc(items, func(a, b akaapi.Subscriber) int { return strings.Compare(a.IMSI, b.IMSI) })
		l.Items, l.NextCursor = page(items, func(s akaapi.Subscriber) string { return s.IMSI }, p)
		return nil
	})
	return l, err
}

// page は、IMSI の昇順の items から、下流と同じ作法（prefix、cursor の次から limit 件）で 1 ページを返す。
func page[T any](items []T, imsiOf func(T) string, p downstream.ListParams) ([]T, string) {
	limit := cmp.Or(p.Limit, 50)
	var out []T
	for _, it := range items {
		imsi := imsiOf(it)
		if !strings.HasPrefix(imsi, p.Prefix) || (p.Cursor != "" && imsi <= p.Cursor) {
			continue
		}
		if len(out) == limit {
			return out, imsiOf(out[limit-1])
		}
		out = append(out, it)
	}
	return out, ""
}

// memStore は Valkey の偽物（ロックと操作の記録）。saveErrs で SaveOperation の失敗を起こせる。
type memStore struct {
	mu       sync.Mutex
	locks    map[string]string
	ops      map[string]store.Operation
	saves    int
	saveErrs map[int]error // n 回目（1 から）の SaveOperation を失敗させる
}

func newMemStore() *memStore {
	return &memStore{locks: map[string]string{}, ops: map[string]store.Operation{}}
}

func (m *memStore) AcquireIMSILock(_ context.Context, imsi string, _ time.Duration) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.locks[imsi]; ok {
		return "", store.ErrLocked
	}
	m.locks[imsi] = "t"
	return "t", nil
}

func (m *memStore) ReleaseIMSILock(_ context.Context, imsi, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks[imsi] == token {
		delete(m.locks, imsi)
	}
	return nil
}

func (m *memStore) SaveOperation(_ context.Context, op store.Operation, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves++
	if err := m.saveErrs[m.saves]; err != nil {
		return err
	}
	op.Steps = slices.Clone(op.Steps)
	m.ops[op.ID] = op
	return nil
}

func (m *memStore) GetOperation(_ context.Context, id string) (store.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.ops[id]
	if !ok {
		return store.Operation{}, store.ErrNotFound
	}
	op.Steps = slices.Clone(op.Steps)
	return op, nil
}

// active は ops:active に入る（未完了の）操作か。
func active(op store.Operation) bool { return !op.Final() }

func (m *memStore) DueOperations(_ context.Context, now time.Time, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var due []store.Operation
	for _, op := range m.ops {
		if active(op) && op.Status != store.OpFailed && !op.NextAttemptAt.After(now) {
			due = append(due, op)
		}
	}
	slices.SortFunc(due, func(a, b store.Operation) int { return a.NextAttemptAt.Compare(b.NextAttemptAt) })
	var ids []string
	for _, op := range due[:min(limit, len(due))] {
		ids = append(ids, op.ID)
	}
	return ids, nil
}

func (m *memStore) ActiveOperations(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, op := range m.ops {
		if active(op) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (m *memStore) RemoveActive(context.Context, string) error { return nil }

// put は操作の記録を直接置く（テストの準備）。
func (m *memStore) put(op store.Operation) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ops[op.ID] = op
}

// memAudit は監査ログの偽物。
type memAudit struct {
	mu      sync.Mutex
	entries []store.AuditEntry
}

func (a *memAudit) Record(_ context.Context, e store.AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
}

func (a *memAudit) list() []store.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.entries)
}

func (m *memStore) op(id string) store.Operation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ops[id]
}

// ---- テスト環境 ----

type env struct {
	s     *Service
	prov  *fakeProv
	aka   *fakeAka
	st    *memStore
	audit *memAudit
	// now は Service の現在時刻（進められる）。
	now time.Time
}

// PLMN マップ: 00102 は aka、それ以外は poc。vector-gateway の AVクライアントID は 1。
const (
	pocIMSI = "001010000000001"
	akaIMSI = "001020000000001"
)

func newEnv(t *testing.T) *env {
	t.Helper()
	m, err := plmn.Parse("00102:01")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{prov: newFakeProv(), aka: newFakeAka(), st: newMemStore(), audit: &memAudit{},
		now: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)}
	e.s = &Service{
		Prov: e.prov, Aka: e.aka, AVClientID: 1, PLMN: m, Store: e.st, Audit: e.audit,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		LockTTL: time.Minute, Retention: time.Hour, RetryDelay: 30 * time.Second,
		MaxRetryDelay: 10 * time.Minute, GiveUpAfter: 24 * time.Hour,
		Now: func() time.Time { return e.now },
	}
	return e
}

var actor = Actor{Operator: "alice", MgmtClient: "bff", TraceID: "trace-1"}

var policy = provapi.PolicyPut{Default: "deny", Rules: []provapi.PolicyRule{{NASID: "*", AllowedSSIDs: []string{"CORP"}}}}

func stepStates(steps []store.OperationStep) string {
	var s []string
	for _, st := range steps {
		s = append(s, st.Name+"@"+st.Downstream+"="+st.State)
	}
	return strings.Join(s, " ")
}

func issues(sub Subscriber) string { return fmt.Sprint(sub.Issues) }
