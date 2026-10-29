package api

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"mime"
	"net/http"
	"slices"
	"strings"

	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/store"
	"github.com/oyaguma3/aka-provisioner/internal/trace"
)

// 中継（設計概要 §6.3）。要求の本文は解釈せずに下流に送り、下流の応答（エラーを含む）をそのまま返す。
// パスの値（IMSI、ID）だけは、下流に送る前に形を確かめる。

// relayHandler は口 rt を下流 name に中継するハンドラーを返す。
func (h *Handler) relayHandler(name downstream.Name, rt route) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path, ok := downstreamPath(w, r, rt.path)
		if !ok {
			return
		}
		req := downstream.RelayRequest{Method: r.Method, Path: path, RawQuery: r.URL.RawQuery}
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
			body, p := readBody(w, r)
			if p != nil {
				p.write(w)
				return
			}
			req.Body, req.ContentType = body, r.Header.Get("Content-Type")
		}
		c := h.Downstream(name)
		resp, err := c.Relay(r.Context(), req)
		if err != nil {
			h.Log.WarnContext(r.Context(), "downstream is not available", "event_id", "DOWNSTREAM_UNREACHABLE",
				"trace_id", trace.From(r.Context()), "downstream", string(name), "error", err.Error(),
				"hint", downstream.Diagnose(name, err))
			p := newProblem(http.StatusServiceUnavailable, causeDownstreamUnavailable, "downstream "+string(name)+" is not available")
			p.Downstream = string(name)
			p.write(w)
			return
		}

		body := resp.Body
		if resp.Status >= 300 {
			var ok bool
			if body, ok = withDownstream(resp, name); !ok {
				h.Log.WarnContext(r.Context(), "unexpected downstream response", "event_id", "DOWNSTREAM_ERROR",
					"trace_id", trace.From(r.Context()), "downstream", string(name), "status", resp.Status, "content_type", resp.ContentType)
				p := newProblem(http.StatusBadGateway, causeDownstreamError, "unexpected response from downstream "+string(name))
				p.Downstream, p.DownstreamStatus = string(name), resp.Status
				p.write(w)
				return
			}
		}
		if resp.ContentType != "" {
			w.Header().Set("Content-Type", resp.ContentType)
		}
		if loc := rewriteLocation(resp.Location, name); loc != "" {
			w.Header().Set("Location", loc)
		}
		w.WriteHeader(resp.Status)
		w.Write(body)

		if rt.write() && resp.Status >= 200 && resp.Status < 300 {
			h.record(r, name, rt, req.Body)
		}
	}
}

// withDownstream は下流の ProblemDetails に downstream を加えた本文を返す。ProblemDetails でなければ false。
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

// rewriteLocation は下流の Location（/admin/v2/...）を aka-provisioner のパス（/admin/v2/<名前>/...）に書き換える。
// 下流の基底のパスで始まらなければ空文字（Location を返さない）。
func rewriteLocation(loc string, name downstream.Name) string {
	if rest, ok := strings.CutPrefix(loc, basePath+"/"); ok {
		return basePath + "/" + string(name) + "/" + rest
	}
	return ""
}

// record は下流を書き換えた中継を監査ログに残す（設計概要 §10。段階 3-1 の U7）。
// action は <下流>.<口>、target は IMSI（登録は本文の imsi）、detail は下流の名前と本文の項目の名前だけ（値は残さない）。
func (h *Handler) record(r *http.Request, name downstream.Name, rt route, body []byte) {
	ctx := r.Context()
	target := r.PathValue("imsi")
	if target == "" {
		target = bodyIMSI(body)
	}
	detail := map[string]any{"downstream": string(name)}
	if body != nil {
		detail["fields"] = fieldNames(body)
	}
	b, err := json.Marshal(detail, json.Deterministic(true))
	if err != nil {
		h.Log.ErrorContext(ctx, "marshal audit detail", "action", rt.action, "error", err)
	}
	h.Audit.Record(ctx, store.AuditEntry{
		Operator:   operatorOf(r),
		MgmtClient: mgmtClientFrom(ctx),
		Action:     string(name) + "." + rt.action,
		Target:     target,
		Detail:     string(b),
		TraceID:    trace.From(ctx),
	})
}

// bodyIMSI は登録の要求の本文の imsi を返す。読めなければ空文字。
func bodyIMSI(body []byte) string {
	var v struct {
		IMSI string `json:"imsi"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return v.IMSI
}

// fieldNames は本文の最上位の項目の名前を辞書順で返す（鍵の項目も名前だけ）。読めなければ空の配列。
func fieldNames(body []byte) []string {
	var obj map[string]jsontext.Value
	if json.Unmarshal(body, &obj) != nil {
		return []string{}
	}
	return slices.Sorted(maps.Keys(obj))
}
