package api

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/oyaguma3/aka-provisioner/internal/akaapi"
	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/provapi"
	"github.com/oyaguma3/aka-provisioner/internal/store"
	"github.com/oyaguma3/aka-provisioner/internal/subscriber"
)

// 中継（設計概要 §7）。要求の本文は検証せずにそのまま下流に送り、下流の応答（エラーを含む）をそのまま返す。
// パスの値（IMSI、ID）だけは、下流に送る前に形式を確かめる。

// imsiPattern は IMSI の形式（数字 15 桁。provisioning-api と同じ）。
var imsiPattern = regexp.MustCompile(`^[0-9]{15}$`)

// pathIMSI はパスの IMSI を返す。形式が違えば 400 を書いて false。
func pathIMSI(w http.ResponseWriter, r *http.Request) (string, bool) {
	imsi := r.PathValue("imsi")
	if !imsiPattern.MatchString(imsi) {
		badParams(causeMandatoryIEIncorrect, invalidParam{"imsi", "must be 15 digits"}).write(w)
		return "", false
	}
	return imsi, true
}

// pathID はパスの ID（clientId）を返す。1 以上の整数でなければ 400 を書いて false。
func pathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	v := r.PathValue("clientId")
	if id, err := strconv.ParseInt(v, 10, 64); err != nil || id < 1 || strconv.FormatInt(id, 10) != v {
		badParams(causeMandatoryIEIncorrect, invalidParam{"clientId", "must be a positive integer"}).write(w)
		return "", false
	}
	return v, true
}

// auditSpec は、中継した書き込み・秘密の値の取得を監査ログに残すときの内容。
type auditSpec struct {
	// action は 2xx の応答から操作を決める（PUT の作成と置き換え、停止と再開を分けるため）。
	action func(resp downstream.RelayResponse) string
	// target は対象。空なら応答の本文の id を使う（作成で採番される ID）。
	target string
}

func fixedAction(a string) func(downstream.RelayResponse) string {
	return func(downstream.RelayResponse) string { return a }
}

// relay は要求を下流 c（名前 name）のパス path に中継する。audit が nil でなければ、2xx のとき監査ログに残す。
func (h *Handler) relay(w http.ResponseWriter, r *http.Request, name downstream.Name, c Relayer, path []string, audit *auditSpec) {
	req := downstream.RelayRequest{Method: r.Method, Path: path, RawQuery: r.URL.RawQuery}
	if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
		body, p := readBody(w, r)
		if p != nil {
			p.write(w)
			return
		}
		req.Body, req.ContentType = body, r.Header.Get("Content-Type")
	}
	resp, err := c.Relay(r.Context(), req)
	if err != nil {
		hint := diagnose(name, err)
		h.Log.WarnContext(r.Context(), "downstream is not available", "downstream", string(name), "error", err, "hint", hint)
		p := newProblem(http.StatusServiceUnavailable, causeDownstreamUnavailable, "downstream "+string(name)+" is not available")
		p.Downstream = string(name)
		p.write(w)
		return
	}

	body := resp.Body
	if resp.Status >= 300 {
		var ok bool
		if body, ok = withDownstream(resp, name); !ok {
			h.Log.WarnContext(r.Context(), "unexpected downstream response", "downstream", string(name),
				"status", resp.Status, "content_type", resp.ContentType)
			p := newProblem(http.StatusBadGateway, causeDownstreamError, "unexpected response from downstream "+string(name))
			p.Downstream, p.DownstreamStatus = string(name), resp.Status
			p.write(w)
			return
		}
	}
	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	}
	if loc := rewriteLocation(resp.Location, c.BaseURL()); loc != "" {
		w.Header().Set("Location", loc)
	}
	w.WriteHeader(resp.Status)
	w.Write(body)

	if audit != nil && resp.Status >= 200 && resp.Status < 300 {
		target := audit.target
		if target == "" {
			target = createdID(resp.Body)
		}
		details := map[string]any{"downstream": string(name)}
		if r.Method == http.MethodPatch {
			details["fields"] = fieldNames(req.Body)
		}
		h.record(r, audit.action(resp), target, "", store.OpCompleted, details)
	}
}

// withDownstream は、下流のエラー応答（ProblemDetails）に downstream を加えた本文を返す。
// ProblemDetails（JSON のオブジェクト）でなければ false。
func withDownstream(resp downstream.RelayResponse, name downstream.Name) ([]byte, bool) {
	if mt, _, _ := mime.ParseMediaType(resp.ContentType); mt != "application/problem+json" {
		return nil, false
	}
	var obj map[string]jsontext.Value
	if err := json.Unmarshal(resp.Body, &obj); err != nil {
		return nil, false
	}
	obj["downstream"], _ = json.Marshal(string(name))
	b, err := json.Marshal(obj, json.Deterministic(true))
	if err != nil {
		return nil, false
	}
	return b, true
}

// rewriteLocation は、下流の Location（下流のベースのパスからの相対パス）を provisioner のパスに書き換える。
func rewriteLocation(loc, downstreamBase string) string {
	if loc == "" {
		return ""
	}
	base, err := url.Parse(downstreamBase)
	if err != nil {
		return ""
	}
	if rest, ok := strings.CutPrefix(loc, strings.TrimSuffix(base.Path, "/")+"/"); ok {
		return basePath + "/" + rest
	}
	return ""
}

// createdID は、作成の応答の本文の id を返す（読めなければ空文字列）。
func createdID(body []byte) string {
	var v struct {
		ID jsontext.Value `json:"id"`
	}
	if json.Unmarshal(body, &v) != nil || len(v.ID) == 0 {
		return ""
	}
	return strings.Trim(string(v.ID), `"`)
}

// fieldNames は、変更の要求の本文（JSON のオブジェクト）の項目名を返す。値は秘密の値を含みうるので返さない。
func fieldNames(body []byte) []string {
	var obj map[string]jsontext.Value
	if json.Unmarshal(body, &obj) != nil {
		return []string{}
	}
	return slices.Sorted(maps.Keys(obj))
}

func diagnose(name downstream.Name, err error) string {
	if name == downstream.Aka {
		return akaapi.Diagnose(err)
	}
	return provapi.Diagnose(err)
}

// ---- prov ----

func (h *Handler) relayClients(w http.ResponseWriter, r *http.Request) {
	var a *auditSpec
	if r.Method == http.MethodPost {
		a = &auditSpec{action: fixedAction("client.create")}
	}
	h.relay(w, r, downstream.Prov, h.Prov, []string{"clients"}, a)
}

func (h *Handler) relayClient(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var a *auditSpec
	switch r.Method {
	case http.MethodPatch:
		a = &auditSpec{action: fixedAction("client.update"), target: id}
	case http.MethodDelete:
		a = &auditSpec{action: fixedAction("client.delete"), target: id}
	}
	h.relay(w, r, downstream.Prov, h.Prov, []string{"clients", id}, a)
}

func (h *Handler) relayClientSecret(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	h.relay(w, r, downstream.Prov, h.Prov, []string{"clients", id, "secret"},
		&auditSpec{action: fixedAction("client.secret.read"), target: id})
}

func (h *Handler) relayPolicies(w http.ResponseWriter, r *http.Request) {
	h.relay(w, r, downstream.Prov, h.Prov, []string{"policies"}, nil)
}

// relayPolicy は認可ポリシーの取得・PUT・削除を中継する。PUT と削除は、加入者の操作と同じ IMSI のロックを取り、
// 同じ IMSI に未完了の操作があれば断る（その補償・やり直しがポリシーを書き換えるため）。
func (h *Handler) relayPolicy(w http.ResponseWriter, r *http.Request) {
	imsi, ok := pathIMSI(w, r)
	if !ok {
		return
	}
	var a *auditSpec
	switch r.Method {
	case http.MethodPut:
		a = &auditSpec{target: imsi, action: func(resp downstream.RelayResponse) string {
			if resp.Status == http.StatusCreated {
				return "policy.create"
			}
			return "policy.update"
		}}
	case http.MethodDelete:
		a = &auditSpec{action: fixedAction("policy.delete"), target: imsi}
	}
	if a != nil {
		unlock, ok := h.guardIMSI(w, r, imsi)
		if !ok {
			return
		}
		defer unlock()
	}
	h.relay(w, r, downstream.Prov, h.Prov, []string{"policies", imsi}, a)
}

// relayPolicyStatus は加入者の停止・再開（認可ポリシーの状態の変更）を中継する。認可ポリシーの PUT と同じく
// IMSI のロックを取り、未完了の操作があれば断る。監査ログの操作は、応答の状態（変更後）から決める。
// 本文の検証は provisioning-api に任せる（中継の作法。設計概要 §7）。
func (h *Handler) relayPolicyStatus(w http.ResponseWriter, r *http.Request) {
	imsi, ok := pathIMSI(w, r)
	if !ok {
		return
	}
	unlock, ok := h.guardIMSI(w, r, imsi)
	if !ok {
		return
	}
	defer unlock()
	h.relay(w, r, downstream.Prov, h.Prov, []string{"policies", imsi, "status"},
		&auditSpec{action: policyStatusAction, target: imsi})
}

// policyStatusAction は、停止・再開の応答（変更後の認可ポリシー）の状態から監査ログの操作を決める。
// 同じ状態への変更（provisioning-api は書き込まず、監査ログにも残さない）も、受け付けた要求として残す。
func policyStatusAction(resp downstream.RelayResponse) string {
	var p provapi.Policy
	if json.Unmarshal(resp.Body, &p) == nil {
		switch p.Status {
		case provapi.PolicySuspended:
			return "policy.suspend"
		case provapi.PolicyActive:
			return "policy.resume"
		}
	}
	return "policy.status.update" // 応答が読めない（想定外）
}

// guardIMSI は、認可ポリシーを書き換える中継の前に IMSI のロックを取り、同じ IMSI に未完了の操作があれば断る
// （その補償・やり直しがポリシーを書き換えるため）。断ったときは応答を書いて false。
func (h *Handler) guardIMSI(w http.ResponseWriter, r *http.Request, imsi string) (unlock func(), ok bool) {
	unlock, ok = h.lockIMSI(w, r, imsi)
	if !ok {
		return nil, false
	}
	if err := h.Subscribers.CheckUnresolved(r.Context(), imsi); err != nil {
		unlock()
		if ue, ok := errors.AsType[*subscriber.UnresolvedError](err); ok {
			writeUnresolved(w, r, ue)
		} else {
			h.internalError(w, r, err)
		}
		return nil, false
	}
	return unlock, true
}

func (h *Handler) relaySessions(w http.ResponseWriter, r *http.Request) {
	h.relay(w, r, downstream.Prov, h.Prov, []string{"sessions"}, nil)
}

func (h *Handler) relayProvAuditLogs(w http.ResponseWriter, r *http.Request) {
	h.relay(w, r, downstream.Prov, h.Prov, []string{"audit-logs"}, nil)
}

// ---- aka ----

// akaOrNotConfigured は aka のクライアントを返す。aka を扱わない設定なら 404 を書いて false。
func (h *Handler) akaOrNotConfigured(w http.ResponseWriter) (AkaAPI, bool) {
	if h.Aka == nil {
		newProblem(http.StatusNotFound, causeDownstreamNotConfigured, "aka-vecgen is not configured").write(w)
		return nil, false
	}
	return h.Aka, true
}

func (h *Handler) relayAkaAuditLogs(w http.ResponseWriter, r *http.Request) {
	aka, ok := h.akaOrNotConfigured(w)
	if !ok {
		return
	}
	h.relay(w, r, downstream.Aka, aka, []string{"audit-logs"}, nil)
}

func (h *Handler) relayAkaAVClient(w http.ResponseWriter, r *http.Request) {
	aka, ok := h.akaOrNotConfigured(w)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	h.relay(w, r, downstream.Aka, aka, []string{"clients", id}, nil)
}
