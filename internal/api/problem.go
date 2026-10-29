package api

import (
	"encoding/json/v2"
	"net/http"
)

// ProblemDetails の cause（設計概要 §6.3・§7）。下流の誤りは、下流の cause のままそのまま返す。
const (
	causeInvalidMsgFormat       = "INVALID_MSG_FORMAT"
	causeInvalidQueryParam      = "INVALID_QUERY_PARAM"
	causeMandatoryIEMissing     = "MANDATORY_IE_MISSING"
	causeMandatoryIEIncorrect   = "MANDATORY_IE_INCORRECT"
	causeOptionalIEIncorrect    = "OPTIONAL_IE_INCORRECT"
	causeSystemFailure          = "SYSTEM_FAILURE"
	causeRouteNotAllowed        = "ROUTE_NOT_ALLOWED"
	causeOperationInProgress    = "OPERATION_IN_PROGRESS"
	causeIdempotencyKeyMismatch = "IDEMPOTENCY_KEY_MISMATCH"
	causeDownstreamError        = "DOWNSTREAM_ERROR"
	causeDownstreamUnavailable  = "DOWNSTREAM_UNAVAILABLE"
)

type invalidParam struct {
	Param  string `json:"param"`
	Reason string `json:"reason,omitempty"`
}

// problem は RFC 9457 の ProblemDetails。
type problem struct {
	Title         string         `json:"title"`
	Status        int            `json:"status"`
	Detail        string         `json:"detail,omitempty"`
	Cause         string         `json:"cause,omitempty"`
	InvalidParams []invalidParam `json:"invalidParams,omitempty"`

	// Downstream は誤りの起きた下流（vecgen / radius）。provisioner 自身の誤りでは空。
	Downstream string `json:"downstream,omitempty"`
	// DownstreamStatus は、下流が ProblemDetails でない応答を返したときの、下流の HTTP のステータス（502 のとき）。
	DownstreamStatus int `json:"downstreamStatus,omitzero"`
}

func newProblem(status int, cause, detail string) *problem {
	return &problem{Title: http.StatusText(status), Status: status, Cause: cause, Detail: detail}
}

func badParams(cause string, params ...invalidParam) *problem {
	p := newProblem(http.StatusBadRequest, cause, "")
	p.InvalidParams = params
	return p
}

func (p *problem) write(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	json.MarshalWrite(w, p)
}

func (h *Handler) internalError(w http.ResponseWriter, r *http.Request, err error) {
	h.Log.ErrorContext(r.Context(), "request failed", "event_id", "SYSTEM_ERROR", "method", r.Method, "path", r.URL.Path, "error", err)
	newProblem(http.StatusInternalServerError, causeSystemFailure, "").write(w)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.MarshalWrite(w, v)
}
