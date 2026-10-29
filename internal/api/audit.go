package api

import (
	"encoding/json/jsontext"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// auditEntryJSON は GET /audit-logs の 1 件（aka-radius の監査ログの口と同じ形）。
type auditEntryJSON struct {
	ID         string         `json:"id"`
	Time       time.Time      `json:"time"`
	Operator   string         `json:"operator"`
	MgmtClient string         `json:"mgmtClient"`
	Action     string         `json:"action"`
	Target     string         `json:"target"`
	Detail     jsontext.Value `json:"detail,omitempty"`
	TraceID    string         `json:"traceId"`
}

type auditListJSON struct {
	Items      []auditEntryJSON `json:"items"`
	NextBefore string           `json:"nextBefore,omitempty"`
}

// auditIDPattern は Stream のエントリID の形式。
var auditIDPattern = regexp.MustCompile(`^[0-9]{1,20}-[0-9]{1,20}$`)

// listAuditLogs は provisioner の監査ログを新しい順に返す（GET /audit-logs）。下流の監査ログは /<下流>/audit-logs で中継する。
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
			TraceID: e.TraceID,
		}
		if v := jsontext.Value(e.Detail); e.Detail != "" && v.IsValid() {
			out.Items[i].Detail = v
		}
	}
	writeJSON(w, http.StatusOK, out)
}
