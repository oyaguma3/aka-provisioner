package api

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/plmn"
	"github.com/oyaguma3/aka-provisioner/internal/provapi"
	"github.com/oyaguma3/aka-provisioner/internal/store"
	"github.com/oyaguma3/aka-provisioner/internal/subscriber"
	"github.com/oyaguma3/aka-provisioner/internal/trace"
)

// SubscriberService は加入者の統合操作。*subscriber.Service が満たす。
type SubscriberService interface {
	Get(ctx context.Context, imsi string) (subscriber.Subscriber, error)
	List(ctx context.Context, p downstream.ListParams) (subscriber.List, error)
	Keys(ctx context.Context, imsi string) (ki, opc string, err error)
	Create(ctx context.Context, a subscriber.Actor, in subscriber.CreateInput) (subscriber.Subscriber, subscriber.Result, error)
	Update(ctx context.Context, a subscriber.Actor, imsi string, in subscriber.UpdateInput) (subscriber.Subscriber, subscriber.Result, error)
	Delete(ctx context.Context, a subscriber.Actor, imsi string) (subscriber.Result, error)
	// CheckUnresolved は、同じ IMSI に未完了の操作があれば *subscriber.UnresolvedError を返す（ポリシーの PUT・DELETE で使う）。
	CheckUnresolved(ctx context.Context, imsi string) error

	Operations(ctx context.Context, status string, limit int) ([]store.Operation, int, error)
	Operation(ctx context.Context, id string) (store.Operation, error)
	Retry(ctx context.Context, a subscriber.Actor, id string) (store.Operation, error)
	Dismiss(ctx context.Context, a subscriber.Actor, id string) (store.Operation, error)
	OperationCounts(ctx context.Context) (map[string]int, error)
}

// ---- 応答の形 ----

type subscriberJSON struct {
	IMSI     string             `json:"imsi"`
	KeyStore plmn.KeyStore      `json:"keyStore"`
	Key      *keyJSON           `json:"key,omitempty"`
	Policy   *provapi.PolicyPut `json:"policy,omitempty"`
	Status   string             `json:"status,omitempty"`
	Issues   []subscriber.Issue `json:"issues"`
}

type keyJSON struct {
	AMF              string    `json:"amf"`
	SQN              string    `json:"sqn"`
	SQNType          string    `json:"sqnType,omitempty"`
	AllowPlain       *bool     `json:"allowPlain,omitempty"`
	AllowedClientIDs []int64   `json:"allowedClientIds,omitzero"`
	CreatedAt        time.Time `json:"createdAt,omitzero"`
	UpdatedAt        time.Time `json:"updatedAt,omitzero"`
}

func toJSON(s subscriber.Subscriber) subscriberJSON {
	out := subscriberJSON{IMSI: s.IMSI, KeyStore: s.KeyStore, Policy: s.Policy, Status: s.Status, Issues: s.Issues}
	if s.Key != nil {
		k := keyJSON(*s.Key)
		out.Key = &k
	}
	if out.Policy != nil && out.Policy.Rules == nil {
		p := *out.Policy
		p.Rules = []provapi.PolicyRule{}
		out.Policy = &p
	}
	if out.Issues == nil {
		out.Issues = []subscriber.Issue{}
	}
	return out
}

// ---- ハンドラー ----

func (h *Handler) actor(r *http.Request) subscriber.Actor {
	return subscriber.Actor{Operator: downstreamOperator(r), MgmtClient: mgmtClientFrom(r.Context()), TraceID: trace.From(r.Context())}
}

func (h *Handler) listSubscribers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := downstream.ListParams{Prefix: q.Get("prefix"), Cursor: q.Get("cursor"), Limit: 50}
	var params []invalidParam
	if p.Prefix != "" && !listIMSIPattern.MatchString(p.Prefix) {
		params = append(params, invalidParam{"prefix", "must be 1-15 digits"})
	}
	if p.Cursor != "" && !listIMSIPattern.MatchString(p.Cursor) {
		params = append(params, invalidParam{"cursor", "must be a nextCursor value from a previous response"})
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			params = append(params, invalidParam{"limit", "must be an integer between 1 and 500"})
		}
		p.Limit = n
	}
	if len(params) > 0 {
		badParams(causeInvalidQueryParam, params...).write(w)
		return
	}
	l, err := h.Subscribers.List(r.Context(), p)
	if err != nil {
		h.subscriberError(w, r, err)
		return
	}
	out := struct {
		Items      []subscriberJSON `json:"items"`
		NextCursor string           `json:"nextCursor,omitempty"`
	}{Items: make([]subscriberJSON, len(l.Items)), NextCursor: l.NextCursor}
	for i, s := range l.Items {
		out.Items[i] = toJSON(s)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) getSubscriber(w http.ResponseWriter, r *http.Request) {
	imsi, ok := pathIMSI(w, r)
	if !ok {
		return
	}
	s, err := h.Subscribers.Get(r.Context(), imsi)
	if err != nil {
		h.subscriberError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toJSON(s))
}

func (h *Handler) getSubscriberKeys(w http.ResponseWriter, r *http.Request) {
	imsi, ok := pathIMSI(w, r)
	if !ok {
		return
	}
	ki, opc, err := h.Subscribers.Keys(r.Context(), imsi)
	if err != nil {
		h.subscriberError(w, r, err)
		return
	}
	h.record(r, "subscriber.keys.read", imsi, "", store.OpCompleted,
		map[string]any{"keyStore": string(h.PLMNMap.KeyStore(imsi))})
	writeJSON(w, http.StatusOK, struct {
		Ki  string `json:"ki"`
		OPc string `json:"opc"`
	}{ki, opc})
}

// createRequest は作成の要求。項目の有無を区別するためにポインターで受ける。
type createRequest struct {
	IMSI       *string        `json:"imsi"`
	KeyStore   *string        `json:"keyStore"`
	Ki         *string        `json:"ki"`
	OPc        *string        `json:"opc"`
	AMF        *string        `json:"amf"`
	SQN        *string        `json:"sqn"`
	SQNType    *string        `json:"sqnType"`
	AllowPlain *bool          `json:"allowPlain"`
	Policy     *policyRequest `json:"policy"`
}

func (h *Handler) createSubscriber(w http.ResponseWriter, r *http.Request) {
	in, ok := h.parseCreate(w, r)
	if !ok {
		return
	}
	s, res, err := h.Subscribers.Create(r.Context(), h.actor(r), in)
	h.recordOperation(r, "subscriber.create", in.IMSI, res, nil)
	if err != nil {
		h.subscriberError(w, r, err)
		return
	}
	w.Header().Set("Location", basePath+"/subscribers/"+in.IMSI)
	writeJSON(w, http.StatusCreated, toJSON(s))
}

// parseCreate は作成の要求を読み、検証する。誤りがあれば応答を書いて false。
func (h *Handler) parseCreate(w http.ResponseWriter, r *http.Request) (subscriber.CreateInput, bool) {
	var req createRequest
	if p := decodeJSON(w, r, &req, "application/json"); p != nil {
		p.write(w)
		return subscriber.CreateInput{}, false
	}
	var v validation
	v.required("imsi", req.IMSI, imsiPattern.MatchString, "must be 15 digits")
	v.required("ki", req.Ki, hex32Pattern.MatchString, "must be 32 hex digits")
	v.required("opc", req.OPc, hex32Pattern.MatchString, "must be 32 hex digits")
	v.optionalField("amf", req.AMF, hex4Pattern.MatchString, "must be 4 hex digits")
	v.optionalField("sqn", req.SQN, hex12Pattern.MatchString, "must be 12 hex digits")
	v.optionalField("keyStore", req.KeyStore, func(s string) bool { return s == "poc" || s == "aka" }, "must be poc or aka")
	v.optionalField("sqnType", req.SQNType, func(s string) bool { return slices.Contains(sqnTypes, s) }, "must be inc1, inc32 or inc33")
	var pol provapi.PolicyPut
	if req.Policy == nil {
		v.missing = append(v.missing, invalidParam{"policy", "is required"})
	} else {
		pol = v.policy("policy.", req.Policy)
	}
	ks := plmn.KeyStorePoC
	if req.IMSI != nil {
		ks = h.PLMNMap.KeyStore(*req.IMSI)
	}
	if ks == plmn.KeyStorePoC {
		v.akaOnly(req.SQNType != nil, req.AllowPlain != nil)
	}
	if p := v.problem(); p != nil {
		p.write(w)
		return subscriber.CreateInput{}, false
	}
	if req.KeyStore != nil && *req.KeyStore != string(ks) {
		p := badParams(causeKeyStoreMismatch, invalidParam{"keyStore", "must be " + string(ks) + " for this IMSI (PLMN map)"})
		p.write(w)
		return subscriber.CreateInput{}, false
	}
	in := subscriber.CreateInput{
		IMSI: *req.IMSI, Ki: *req.Ki, OPc: *req.OPc, AMF: deref(req.AMF), SQN: deref(req.SQN), SQNType: deref(req.SQNType),
		AllowPlain: req.AllowPlain, Policy: pol,
	}
	return in, true
}

// akaOnly は、置き場所が poc の加入者に aka だけの項目が指定されていれば記録する。
func (v *validation) akaOnly(sqnType, allowPlain bool) {
	if sqnType {
		v.optional = append(v.optional, invalidParam{"sqnType", "only for keyStore aka"})
	}
	if allowPlain {
		v.optional = append(v.optional, invalidParam{"allowPlain", "only for keyStore aka"})
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// patchFields は変更の要求で受け付ける項目。
var patchFields = []string{"ki", "opc", "amf", "sqn", "sqnType", "allowPlain", "policy"}

func (h *Handler) updateSubscriber(w http.ResponseWriter, r *http.Request) {
	imsi, ok := pathIMSI(w, r)
	if !ok {
		return
	}
	in, fields, ok := h.parseUpdate(w, r, h.PLMNMap.KeyStore(imsi))
	if !ok {
		return
	}
	s, res, err := h.Subscribers.Update(r.Context(), h.actor(r), imsi, in)
	h.recordOperation(r, "subscriber.update", imsi, res, fields)
	if err != nil {
		h.subscriberError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toJSON(s))
}

// parseUpdate は変更の要求（JSON Merge Patch）を読み、検証する。変えた項目名も返す。誤りがあれば応答を書いて false。
func (h *Handler) parseUpdate(w http.ResponseWriter, r *http.Request, ks plmn.KeyStore) (subscriber.UpdateInput, []string, bool) {
	var raw map[string]jsontext.Value
	if p := decodeJSON(w, r, &raw, "application/merge-patch+json", "application/json"); p != nil {
		p.write(w)
		return subscriber.UpdateInput{}, nil, false
	}
	if len(raw) == 0 {
		newProblem(http.StatusBadRequest, causeMandatoryIEMissing, "at least one field is required").write(w)
		return subscriber.UpdateInput{}, nil, false
	}
	var v validation
	for name, val := range raw {
		if !slices.Contains(patchFields, name) {
			newProblem(http.StatusBadRequest, causeInvalidMsgFormat, "unknown field "+name).write(w)
			return subscriber.UpdateInput{}, nil, false
		}
		if val.Kind() == 'n' {
			v.optional = append(v.optional, invalidParam{name, "must not be null"})
			delete(raw, name)
		}
	}
	var req struct {
		Ki         *string        `json:"ki"`
		OPc        *string        `json:"opc"`
		AMF        *string        `json:"amf"`
		SQN        *string        `json:"sqn"`
		SQNType    *string        `json:"sqnType"`
		AllowPlain *bool          `json:"allowPlain"`
		Policy     *policyRequest `json:"policy"`
	}
	b, _ := json.Marshal(raw)
	if err := json.Unmarshal(b, &req, json.RejectUnknownMembers(true)); err != nil {
		newProblem(http.StatusBadRequest, causeInvalidMsgFormat, "request body is not valid JSON for this operation").write(w)
		return subscriber.UpdateInput{}, nil, false
	}
	v.optionalField("ki", req.Ki, hex32Pattern.MatchString, "must be 32 hex digits")
	v.optionalField("opc", req.OPc, hex32Pattern.MatchString, "must be 32 hex digits")
	v.optionalField("amf", req.AMF, hex4Pattern.MatchString, "must be 4 hex digits")
	v.optionalField("sqn", req.SQN, hex12Pattern.MatchString, "must be 12 hex digits")
	v.optionalField("sqnType", req.SQNType, func(s string) bool { return slices.Contains(sqnTypes, s) }, "must be inc1, inc32 or inc33")
	in := subscriber.UpdateInput{Ki: req.Ki, OPc: req.OPc, AMF: req.AMF, SQN: req.SQN, SQNType: req.SQNType, AllowPlain: req.AllowPlain}
	if req.Policy != nil {
		pol := v.policy("policy.", req.Policy)
		in.Policy = &pol
	}
	if ks == plmn.KeyStorePoC {
		v.akaOnly(req.SQNType != nil, req.AllowPlain != nil)
	}
	if p := v.problem(); p != nil {
		p.write(w)
		return subscriber.UpdateInput{}, nil, false
	}
	return in, slices.Sorted(maps.Keys(raw)), true
}

func (h *Handler) deleteSubscriber(w http.ResponseWriter, r *http.Request) {
	imsi, ok := pathIMSI(w, r)
	if !ok {
		return
	}
	res, err := h.Subscribers.Delete(r.Context(), h.actor(r), imsi)
	h.recordOperation(r, "subscriber.delete", imsi, res, nil)
	if err != nil {
		h.subscriberError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeJSON は要求の本文を v に読む。Content-Type が allowed でない、JSON として読めない、未知の項目がある、
// 型が違う、後ろにデータが続く場合は 400（INVALID_MSG_FORMAT）を返す（本PoCの Provisioning API と同じ）。
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, allowed ...string) *problem {
	if p := checkContentType(r, allowed...); p != nil {
		return p
	}
	body, p := readBody(w, r)
	if p != nil {
		return p
	}
	if err := json.Unmarshal(body, v, json.RejectUnknownMembers(true)); err != nil {
		return newProblem(http.StatusBadRequest, causeInvalidMsgFormat, "request body is not valid JSON for this operation")
	}
	return nil
}

// recordOperation は、加入者の作成・変更・削除を監査ログに残す。操作の記録を作る前に断った要求（入力の誤り、
// 既にある、処理中など）は残さない（設計概要 §10.2）。fields は変更で変えた項目名。
func (h *Handler) recordOperation(r *http.Request, action, imsi string, res subscriber.Result, fields []string) {
	if res.OperationID == "" {
		return
	}
	steps := make([]map[string]string, len(res.Steps))
	for i, s := range res.Steps {
		steps[i] = map[string]string{"name": s.Name, "downstream": s.Downstream, "state": s.State}
	}
	details := map[string]any{"keyStore": string(h.PLMNMap.KeyStore(imsi)), "steps": steps}
	if fields != nil {
		details["fields"] = fields
	}
	h.record(r, action, imsi, res.OperationID, res.Status, details)
}

// ---- エラー ----

// inputCauses は、下流が呼び出し側の入力の誤りとして返す cause。
var inputCauses = []string{causeInvalidMsgFormat, causeMandatoryIEMissing, causeMandatoryIEIncorrect, causeOptionalIEIncorrect}

// subscriberError は加入者の統合操作のエラーを応答にする（docs/openapi/provisioner-api.yaml の各応答）。
func (h *Handler) subscriberError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrLocked) {
		doNotRemember(r)
		newProblem(http.StatusConflict, causeOperationInProgress, "another operation on the same IMSI is in progress").write(w)
		return
	}
	if ue, ok := errors.AsType[*subscriber.UnresolvedError](err); ok {
		writeUnresolved(w, r, ue)
		return
	}
	if errors.Is(err, subscriber.ErrNotFound) {
		newProblem(http.StatusNotFound, causeUserNotFound, "").write(w)
		return
	}
	if ce, ok := errors.AsType[*subscriber.ConflictError](err); ok {
		p := newProblem(http.StatusConflict, causeSubscriberExists, "")
		p.Conflicts = ce.Places
		p.write(w)
		return
	}

	var p *problem
	if de, ok := errors.AsType[*subscriber.DownstreamError](err); ok {
		p = downstreamProblem(de)
		if p.Status >= 500 {
			h.Log.WarnContext(r.Context(), "downstream error", "downstream", string(de.Name), "error", de.Err, "hint", diagnose(de.Name, de.Err))
		}
	} else {
		h.Log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		p = newProblem(http.StatusInternalServerError, causeSystemFailure, "")
	}
	if oe, ok := errors.AsType[*subscriber.OperationError](err); ok {
		p.OperationID, p.RolledBack = oe.OperationID, new(oe.RolledBack)
		if oe.Incomplete {
			// 下流の項目（downstream 等）は最初の失敗のものを残す。
			p.Status, p.Title, p.Cause, p.InvalidParams = http.StatusInternalServerError,
				http.StatusText(http.StatusInternalServerError), causeOperationIncomplete, nil
			p.Detail = "the operation did not complete and will be retried; see /operations/" + oe.OperationID
		}
	}
	p.write(w)
}

// writeUnresolved は、同じ IMSI に未完了の操作があるため断ったことを 409 で返す（設計概要 §9.3）。
// その操作を retry か dismiss で片付ければ同じ要求が通るので、Idempotency-Key では覚えない。
func writeUnresolved(w http.ResponseWriter, r *http.Request, ue *subscriber.UnresolvedError) {
	doNotRemember(r)
	p := newProblem(http.StatusConflict, causeOperationUnresolved,
		"an unresolved operation ("+ue.Status+") remains on the same IMSI; retry or dismiss it first: /operations/"+ue.OperationID)
	p.OperationID = ue.OperationID
	p.write(w)
}

// downstreamProblem は、下流の呼び出しの失敗を応答にする（設計概要 §6.3）。
func downstreamProblem(de *subscriber.DownstreamError) *problem {
	name := string(de.Name)
	apiErr, ok := errors.AsType[*downstream.Error](de.Err)
	if !ok {
		p := newProblem(http.StatusServiceUnavailable, causeDownstreamUnavailable, "downstream "+name+" is not available")
		p.Downstream = name
		return p
	}
	cause := apiErr.Problem.Cause
	switch {
	case apiErr.Status == http.StatusBadRequest && slices.Contains(inputCauses, cause):
		// 入力の誤り（provisioner の検証をすり抜けたもの）。項目名を統合リソースの名前に付け替える。
		p := newProblem(http.StatusBadRequest, cause, apiErr.Problem.Detail)
		for _, ip := range apiErr.Problem.InvalidParams {
			p.InvalidParams = append(p.InvalidParams, invalidParam{de.ParamPrefix + ip.Param, ip.Reason})
		}
		p.Downstream = name
		return p
	case apiErr.Status == http.StatusNotFound && cause == causeUserNotFound:
		p := newProblem(http.StatusNotFound, causeUserNotFound, "")
		p.Downstream = name
		return p
	case apiErr.Status == http.StatusConflict && cause == causeSubscriberExists:
		// 存在確認の後に、provisioner の外で作られた。
		p := newProblem(http.StatusConflict, causeSubscriberExists, apiErr.Problem.Detail)
		p.Downstream = name
		p.Conflicts = []string{map[string]string{"prov": "poc", "aka": "aka"}[name]}
		return p
	}
	// 5xx、ProblemDetails でない応答、呼び出し側では直せない 4xx（例: aka の CLIENT_NOT_FOUND）。
	p := newProblem(http.StatusBadGateway, causeDownstreamError, "unexpected response from downstream "+name)
	p.Downstream, p.DownstreamStatus, p.DownstreamCause = name, apiErr.Status, cause
	return p
}
