package subscriber

import (
	"context"
	"slices"
	"sync"

	"github.com/oyaguma3/aka-provisioner/internal/akaapi"
	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/plmn"
	"github.com/oyaguma3/aka-provisioner/internal/provapi"
)

// List は加入者の一覧の 1 ページ（設計概要 §6.4）。
type List struct {
	Items []Subscriber
	// NextCursor は次のページがある場合だけ入る（そのページの最後の IMSI）。
	NextCursor string
}

// List は、prov の加入者、aka の加入者、認可ポリシーの 3 つの一覧を、同じ cursor と prefix で取り寄せ、
// IMSI の昇順にマージする。3 つとも IMSI の昇順で返すので、それぞれから limit 件を取れば、
// 和集合の先頭 limit 件は正しく求まる。
//   - aka の加入者は、PLMN マップで aka に当たる IMSI だけを対象にする（足りなければ次のページを取り寄せる）。
//   - poc の IMSI について aka 側の加入者は調べない（KEY_IN_OTHER_STORE は Get でだけ分かる場合がある）。
func (s *Service) List(ctx context.Context, p downstream.ListParams) (List, error) {
	var (
		provList   provapi.SubscriberList
		policyList provapi.PolicyList
		akaItems   []akaapi.Subscriber
		akaMore    bool
		provErr    error
		policyErr  error
		akaErr     error
	)
	var wg sync.WaitGroup
	wg.Go(func() { provList, provErr = s.Prov.ListSubscribers(ctx, p) })
	wg.Go(func() { policyList, policyErr = s.Prov.ListPolicies(ctx, p) })
	if s.Aka != nil && s.PLMN.Uses(plmn.KeyStoreAKA) {
		wg.Go(func() { akaItems, akaMore, akaErr = s.listAka(ctx, p) })
	}
	wg.Wait()
	if err := cmpErr(provErr, policyErr); err != nil {
		return List{}, &DownstreamError{Name: downstream.Prov, Err: err}
	}
	if akaErr != nil {
		return List{}, &DownstreamError{Name: downstream.Aka, Err: akaErr}
	}

	states := map[string]*state{}
	get := func(imsi string) *state {
		st, ok := states[imsi]
		if !ok {
			st = &state{keyStore: s.PLMN.KeyStore(imsi)}
			states[imsi] = st
		}
		return st
	}
	for i := range provList.Items {
		get(provList.Items[i].IMSI).prov = &provList.Items[i]
	}
	for i := range policyList.Items {
		get(policyList.Items[i].IMSI).policy = &policyList.Items[i]
	}
	for i := range akaItems {
		get(akaItems[i].IMSI).aka = &akaItems[i]
	}

	imsis := make([]string, 0, len(states))
	for imsi := range states {
		imsis = append(imsis, imsi)
	}
	slices.Sort(imsis)
	limit := p.Limit
	if limit == 0 {
		limit = 50
	}
	more := provList.NextCursor != "" || policyList.NextCursor != "" || akaMore
	if len(imsis) > limit {
		imsis, more = imsis[:limit], true
	}

	out := List{Items: make([]Subscriber, len(imsis))}
	for i, imsi := range imsis {
		out.Items[i] = s.view(imsi, *states[imsi])
	}
	if more && len(imsis) > 0 {
		out.NextCursor = imsis[len(imsis)-1]
	}
	return out, nil
}

// listAka は、aka の加入者のうち PLMN マップで aka に当たるものを、limit 件まで集める。
// 続きがあれば more が true。
func (s *Service) listAka(ctx context.Context, p downstream.ListParams) (items []akaapi.Subscriber, more bool, err error) {
	limit := p.Limit
	if limit == 0 {
		limit = 50
	}
	q := p
	for {
		l, err := s.Aka.ListSubscribers(ctx, q)
		if err != nil {
			return nil, false, err
		}
		for _, a := range l.Items {
			if s.PLMN.KeyStore(a.IMSI) != plmn.KeyStoreAKA {
				continue
			}
			if len(items) == limit {
				return items, true, nil
			}
			items = append(items, a)
		}
		if l.NextCursor == "" {
			return items, false, nil
		}
		if len(items) == limit {
			return items, true, nil
		}
		q.Cursor = l.NextCursor
	}
}
