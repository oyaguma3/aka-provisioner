package api

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/store"
	"github.com/oyaguma3/aka-provisioner/internal/trace"
)

// record は操作を監査ログに残す（設計概要 §10.2）。details に秘密の値を入れてはならない。
func (h *Handler) record(r *http.Request, action, target, operationID, result string, details map[string]any) {
	ctx := r.Context()
	e := store.AuditEntry{
		Operator:    downstreamOperator(r),
		MgmtClient:  mgmtClientFrom(ctx),
		Action:      action,
		Target:      target,
		TraceID:     trace.From(ctx),
		OperationID: operationID,
		Result:      result,
	}
	if details != nil {
		b, err := json.Marshal(details, json.Deterministic(true))
		if err != nil {
			h.Log.ErrorContext(ctx, "marshal audit details", "action", action, "error", err)
		}
		e.Details = string(b)
	}
	h.Audit.Record(ctx, e)
}

// downstreamOperator は要求の X-Operator-Id（checkOperator で形式を確かめた値。省略なら空文字列）。
func downstreamOperator(r *http.Request) string {
	if v := r.Header.Get(operatorHeader); operatorPattern.MatchString(v) {
		return v
	}
	return ""
}

type auditEntryJSON struct {
	ID          string         `json:"id"`
	Time        time.Time      `json:"time"`
	Operator    string         `json:"operator"`
	MgmtClient  string         `json:"mgmtClient"`
	Action      string         `json:"action"`
	Target      string         `json:"target"`
	TraceID     string         `json:"traceId"`
	OperationID string         `json:"operationId,omitempty"`
	Result      string         `json:"result"`
	Details     jsontext.Value `json:"details,omitempty"`
}

type auditListJSON struct {
	Items      []auditEntryJSON `json:"items"`
	NextBefore string           `json:"nextBefore,omitempty"`
}

var auditIDPattern = regexp.MustCompile(`^[0-9]{1,20}-[0-9]{1,20}$`)

// listAuditLogs は provisioner の監査ログを新しい順に返す。
func (h *Handler) listAuditLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var params []invalidParam
	before := q.Get("before")
	if before != "" && !auditIDPattern.MatchString(before) {
		params = append(params, invalidParam{"before", "must be a nextBefore value from a previous response"})
	}
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			params = append(params, invalidParam{"limit", "must be an integer between 1 and 500"})
		}
		limit = n
	}
	if len(params) > 0 {
		badParams(causeInvalidQueryParam, params...).write(w)
		return
	}

	entries, next, err := h.Store.ListAudit(r.Context(), before, limit)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	out := auditListJSON{Items: make([]auditEntryJSON, len(entries)), NextBefore: next}
	for i, e := range entries {
		out.Items[i] = auditEntryJSON{
			ID: e.ID, Time: e.Time, Operator: e.Operator, MgmtClient: e.MgmtClient, Action: e.Action, Target: e.Target,
			TraceID: e.TraceID, OperationID: e.OperationID, Result: e.Result,
		}
		// 保存してある JSON をそのまま埋め込む。壊れている場合は省く。
		if v := jsontext.Value(e.Details); e.Details != "" && v.IsValid() {
			out.Items[i].Details = v
		}
	}
	writeJSON(w, http.StatusOK, out)
}
