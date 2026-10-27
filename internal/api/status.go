package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/akaapi"
	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/plmn"
	"github.com/oyaguma3/aka-provisioner/internal/provapi"
	"github.com/oyaguma3/aka-provisioner/internal/store"
)

type statusJSON struct {
	Version     string               `json:"version"`
	StartedAt   time.Time            `json:"startedAt"`
	PLMNMap     []plmnEntryJSON      `json:"plmnMap"`
	Downstreams downstreamsJSON      `json:"downstreams"`
	AVClient    avClientStatusJSON   `json:"avClient"`
	Valkey      valkeyStatusJSON     `json:"valkey"`
	Operations  *operationCountsJSON `json:"operations,omitempty"`
}

type plmnEntryJSON struct {
	PLMN     string        `json:"plmn"`
	KeyStore plmn.KeyStore `json:"keyStore"`
}

type downstreamsJSON struct {
	Prov downstreamStatusJSON `json:"prov"`
	Aka  downstreamStatusJSON `json:"aka"`
}

type downstreamStatusJSON struct {
	Configured      bool   `json:"configured"`
	URL             string `json:"url,omitempty"`
	Reachable       bool   `json:"reachable"`
	Version         string `json:"version,omitempty"`
	NodeName        string `json:"nodeName,omitempty"`
	SubscriberCount *int64 `json:"subscriberCount,omitempty"`
	Error           string `json:"error,omitempty"`
	Hint            string `json:"hint,omitempty"`
}

type avClientStatusJSON struct {
	ID int64 `json:"id"`
	// Exists と Enabled は、aka-vecgen に問い合わせられなかった場合は省略する。
	Exists  *bool  `json:"exists,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
	Name    string `json:"name,omitempty"`
}

type valkeyStatusJSON struct {
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

type operationCountsJSON struct {
	Running  int `json:"running"`
	Retrying int `json:"retrying"`
	Failed   int `json:"failed"`
}

// getStatus は provisioner と下流の状態を返す。下流や Valkey に接続できなくても 200 を返し、項目ごとに示す。
func (h *Handler) getStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.DownstreamTimeout)
	defer cancel()

	out := statusJSON{Version: h.Version, StartedAt: h.StartedAt, PLMNMap: []plmnEntryJSON{}}
	for _, e := range h.PLMNMap.Entries() {
		out.PLMNMap = append(out.PLMNMap, plmnEntryJSON{PLMN: e.PLMN, KeyStore: e.KeyStore})
	}

	var wg sync.WaitGroup
	wg.Go(func() { out.Downstreams.Prov = h.provStatus(ctx) })
	wg.Go(func() { out.Downstreams.Aka, out.AVClient = h.akaStatus(ctx) })
	wg.Go(func() {
		if err := h.Store.Ping(ctx); err != nil {
			out.Valkey.Error = err.Error()
			return
		}
		out.Valkey.Reachable = true
		counts, err := h.Subscribers.OperationCounts(ctx)
		if err != nil {
			h.Log.ErrorContext(ctx, "count operations", "error", err)
			return
		}
		out.Operations = &operationCountsJSON{Running: counts[store.OpRunning], Retrying: counts[store.OpRetrying], Failed: counts[store.OpFailed]}
	})
	wg.Wait()
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) provStatus(ctx context.Context) downstreamStatusJSON {
	st := downstreamStatusJSON{Configured: true, URL: h.Prov.BaseURL()}
	s, err := h.Prov.Status(ctx)
	if err != nil {
		st.Error, st.Hint = err.Error(), provapi.Diagnose(err)
		h.Log.WarnContext(ctx, "provisioning-api is not available", "error", err, "hint", st.Hint)
		return st
	}
	st.Reachable, st.Version, st.NodeName, st.SubscriberCount = true, s.Version, s.NodeName, &s.SubscriberCount
	return st
}

func (h *Handler) akaStatus(ctx context.Context) (downstreamStatusJSON, avClientStatusJSON) {
	av := avClientStatusJSON{ID: h.AkaAVClientID}
	if h.Aka == nil {
		return downstreamStatusJSON{}, av
	}
	st := downstreamStatusJSON{Configured: true, URL: h.Aka.BaseURL()}
	s, err := h.Aka.Status(ctx)
	if err != nil {
		st.Error, st.Hint = err.Error(), akaapi.Diagnose(err)
		h.Log.WarnContext(ctx, "aka-vecgen is not available", "error", err, "hint", st.Hint)
		return st, av
	}
	st.Reachable, st.Version, st.SubscriberCount = true, s.Version, &s.SubscriberCount

	c, err := h.Aka.GetAVClient(ctx, h.AkaAVClientID)
	switch apiErr, ok := errors.AsType[*downstream.Error](err); {
	case err == nil:
		av.Exists, av.Enabled, av.Name = new(true), new(c.Enabled), c.Name
	case ok && apiErr.Status == http.StatusNotFound && apiErr.Problem.Cause == akaapi.CauseClientNotFound:
		av.Exists = new(false)
	default:
		h.Log.WarnContext(ctx, "get av client", "av_client_id", h.AkaAVClientID, "error", err)
	}
	return st, av
}
