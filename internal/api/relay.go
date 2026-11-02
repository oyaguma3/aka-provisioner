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

// relayHandler は口 rt を、種類 k の下流に中継するハンドラーを返す。パスの値と下流の有無は冪等の判定より先に確かめる
// （登録簿にないエッジの 404 を覚えると、エッジを足した後に同じキーでやり直せない）。
func (h *Handler) relayHandler(k kind, rt route) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if k == kindEdge {
			if _, ok := pathValue(w, r, "edgeId"); !ok {
				return
			}
		}
		path, ok := downstreamPath(w, r, rt.path)
		if !ok {
			return
		}
		name := k.name(r)
		// エッジは要求のときに引く（読み直しで替わる）。
		c := h.Downstream(name)
		if c == nil {
			h.notRegistered(w, r, k, name)
			return
		}
		relay := func(w http.ResponseWriter, r *http.Request) { h.relay(w, r, k, rt, name, c, path) }
		if rt.write() {
			relay = h.idempotent(relay)
		}
		relay(w, r)
	}
}

// notRegistered は登録簿にない下流への要求に応える。エッジは 404 EDGE_NOT_FOUND、radsec-ca は 503（P2）。
func (h *Handler) notRegistered(w http.ResponseWriter, r *http.Request, k kind, name downstream.Name) {
	h.Log.WarnContext(r.Context(), "downstream is not registered", "event_id", "DOWNSTREAM_NOT_REGISTERED",
		"trace_id", trace.From(r.Context()), "downstream", string(name))
	p := newProblem(http.StatusServiceUnavailable, causeDownstreamUnavailable, "downstream "+string(name)+" is not registered")
	if k == kindEdge {
		p = newProblem(http.StatusNotFound, causeEdgeNotFound, "edge "+r.PathValue("edgeId")+" is not registered")
	}
	p.Downstream = string(name)
	p.write(w)
}

func (h *Handler) relay(w http.ResponseWriter, r *http.Request, k kind, rt route, name downstream.Name, c Relayer, path []string) {
	req := downstream.RelayRequest{Method: r.Method, Path: path, RawQuery: r.URL.RawQuery}
	if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
		body, p := readBody(w, r)
		if p != nil {
			p.write(w)
			return
		}
		req.Body, req.ContentType = body, r.Header.Get("Content-Type")
	}
	// radsec-ca とエッジには受け取ったキーをそのまま渡す。aka-vecgen・aka-radius は扱わない（P7）。
	if k.radsec() && rt.write() {
		req.IdempotencyKey = r.Header.Get(idempotencyKeyHeader)
	}
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
	if loc := rewriteLocation(resp.Location, k, r); loc != "" {
		w.Header().Set("Location", loc)
	}
	// エッジの 503 TENANT_BUSY と、radsec-ca・エッジの再送の応答（P9）。
	if resp.RetryAfter != "" {
		w.Header().Set("Retry-After", resp.RetryAfter)
	}
	if resp.IdempotentReplayed != "" {
		w.Header().Set(idempotentReplayedHeader, resp.IdempotentReplayed)
	}
	w.WriteHeader(resp.Status)
	w.Write(body)

	if rt.write() && resp.Status >= 200 && resp.Status < 300 {
		h.record(r, k, rt, name, resp.Location, req.Body)
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

// rewriteLocation は下流の Location を aka-provisioner のパス（/admin/v2/<名前>/...）に書き換える。
// aka-vecgen・aka-radius の Location は /admin/v2/...、radsec-ca の Location は接頭辞なし（/cas/...、/certs/...。P9）。
// 形が違えば空文字（Location を返さない）。
func rewriteLocation(loc string, k kind, r *http.Request) string {
	rest := loc
	if !k.radsec() {
		var ok bool
		if rest, ok = strings.CutPrefix(loc, basePath); !ok {
			return ""
		}
	}
	if !strings.HasPrefix(rest, "/") {
		return ""
	}
	prefix := k.pathPrefix()
	if k == kindEdge {
		prefix = "/edges/" + r.PathValue("edgeId")
	}
	return basePath + prefix + rest
}

// record は下流を書き換えた中継を監査ログに残す（設計概要 §10。段階 3-1 の U7、段階 4-2 の P10）。
// action は <下流の種類>.<口>、target はパスの値（パスにない登録と発行は Location の最後の要素）、
// detail は下流の名前と本文の項目の名前だけ（値は残さない。本文は解釈しない）。
func (h *Handler) record(r *http.Request, k kind, rt route, name downstream.Name, location string, body []byte) {
	ctx := r.Context()
	target := ""
	if param := rt.targetParam(); param != "" {
		target = r.PathValue(param)
	} else if _, last, ok := strings.CutLast(location, "/"); ok {
		target = last
	}
	detail := map[string]any{"downstream": string(name)}
	if len(body) > 0 {
		detail["fields"] = fieldNames(body)
	}
	b, err := json.Marshal(detail, json.Deterministic(true))
	if err != nil {
		h.Log.ErrorContext(ctx, "marshal audit detail", "action", rt.action, "error", err)
	}
	h.Audit.Record(ctx, store.AuditEntry{
		Operator:   operatorOf(r),
		MgmtClient: mgmtClientFrom(ctx),
		Action:     string(k) + "." + rt.action,
		Target:     target,
		Detail:     string(b),
		TraceID:    trace.From(ctx),
	})
}

// fieldNames は本文の最上位の項目の名前を辞書順で返す（鍵の項目も名前だけ）。読めなければ空の配列。
func fieldNames(body []byte) []string {
	var obj map[string]jsontext.Value
	if json.Unmarshal(body, &obj) != nil {
		return []string{}
	}
	return slices.Sorted(maps.Keys(obj))
}
