package api

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/store"
	"github.com/oyaguma3/aka-provisioner/internal/subscriber"
)

// 操作の記録（/operations。設計概要 §9.3）。

// operationIDPattern は操作の ID（UUID の文字列表記）。
var operationIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type stepErrorJSON struct {
	Status int       `json:"status,omitzero"`
	Cause  string    `json:"cause,omitempty"`
	Detail string    `json:"detail,omitempty"`
	Time   time.Time `json:"time"`
}

type stepJSON struct {
	Name       string         `json:"name"`
	Downstream string         `json:"downstream"`
	State      string         `json:"state"`
	Error      *stepErrorJSON `json:"error,omitempty"`
}

type operationJSON struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"`
	IMSI          string     `json:"imsi"`
	KeyStore      string     `json:"keyStore"`
	Status        string     `json:"status"`
	Steps         []stepJSON `json:"steps"`
	Attempts      int        `json:"attempts"`
	NextAttemptAt *time.Time `json:"nextAttemptAt,omitempty"`
	Operator      string     `json:"operator"`
	MgmtClient    string     `json:"mgmtClient"`
	TraceID       string     `json:"traceId"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

func operationToJSON(op store.Operation) operationJSON {
	out := operationJSON{
		ID: op.ID, Kind: op.Kind, IMSI: op.IMSI, KeyStore: op.KeyStore, Status: op.Status, Steps: make([]stepJSON, len(op.Steps)),
		Attempts: op.Attempts, Operator: op.Operator, MgmtClient: op.MgmtClient, TraceID: op.TraceID,
		CreatedAt: op.CreatedAt, UpdatedAt: op.UpdatedAt,
	}
	for i, st := range op.Steps {
		out.Steps[i] = stepJSON{Name: st.Name, Downstream: st.Downstream, State: st.State}
		if e := st.Error; e != nil {
			out.Steps[i].Error = &stepErrorJSON{Status: e.Status, Cause: e.Cause, Detail: e.Detail, Time: e.Time}
		}
	}
	// 次に自動でやり直す時刻は、retrying のときだけ返す。
	if op.Status == store.OpRetrying {
		out.NextAttemptAt = &op.NextAttemptAt
	}
	return out
}

func (h *Handler) listOperations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	var params []invalidParam
	if status != "" && status != store.OpRunning && status != store.OpRetrying && status != store.OpFailed {
		params = append(params, invalidParam{"status", "must be running, retrying or failed"})
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
	items, total, err := h.Subscribers.Operations(r.Context(), status, limit)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	out := struct {
		Items []operationJSON `json:"items"`
		Total int             `json:"total"`
	}{Items: make([]operationJSON, len(items)), Total: total}
	for i, op := range items {
		out.Items[i] = operationToJSON(op)
	}
	writeJSON(w, http.StatusOK, out)
}

// pathOperationID はパスの操作の ID を返す。形式が違えば 400 を書いて false。
func pathOperationID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("operationId")
	if !operationIDPattern.MatchString(id) {
		badParams(causeMandatoryIEIncorrect, invalidParam{"operationId", "must be a UUID"}).write(w)
		return "", false
	}
	return id, true
}

func (h *Handler) getOperation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathOperationID(w, r)
	if !ok {
		return
	}
	op, err := h.Subscribers.Operation(r.Context(), id)
	if err != nil {
		h.operationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, operationToJSON(op))
}

func (h *Handler) retryOperation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathOperationID(w, r)
	if !ok {
		return
	}
	op, err := h.Subscribers.Retry(r.Context(), h.actor(r), id)
	if err != nil {
		h.operationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, operationToJSON(op))
}

func (h *Handler) dismissOperation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathOperationID(w, r)
	if !ok {
		return
	}
	op, err := h.Subscribers.Dismiss(r.Context(), h.actor(r), id)
	if err != nil {
		h.operationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, operationToJSON(op))
}

func (h *Handler) operationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, subscriber.ErrOperationNotFound):
		newProblem(http.StatusNotFound, causeOperationNotFound, "").write(w)
	case errors.Is(err, subscriber.ErrOperationState):
		newProblem(http.StatusConflict, causeOperationStateConflict,
			"retry is only for failed operations (and not for an update whose key change is unknown); dismiss is only for retrying or failed operations").write(w)
	case errors.Is(err, store.ErrLocked):
		doNotRemember(r)
		newProblem(http.StatusConflict, causeOperationInProgress, "another operation on the same IMSI is in progress").write(w)
	default:
		h.internalError(w, r, err)
	}
}
