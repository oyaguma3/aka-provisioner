// Package api は aka-provisioner の API（/admin/v2）。下流（aka-vecgen・aka-radius）の管理API のうち、許した口だけを
// 中継する（設計概要 §6）。下流を書き換える要求では Idempotency-Key を必須にし（§7）、成功したら監査ログに残す（§10）。
package api

import (
	"context"
	"crypto/x509"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/audit"
	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/store"
	"github.com/oyaguma3/aka-provisioner/internal/trace"
)

const basePath = "/admin/v2"

// Relayer は 1 つの下流への中継（*downstream.Client）。
type Relayer interface {
	BaseURL() string
	ServerCertificate() *x509.Certificate
	Relay(ctx context.Context, req downstream.RelayRequest) (downstream.RelayResponse, error)
}

// Store は provisioner 専用の Valkey（*store.Store）。
type Store interface {
	Ping(ctx context.Context) error
	ListAudit(ctx context.Context, before string, limit int) ([]store.AuditEntry, string, error)
	BeginIdempotent(ctx context.Context, mgmtClient, key, reqHash string, pendingTTL time.Duration) (store.IdempotencyState, store.StoredResponse, error)
	CompleteIdempotent(ctx context.Context, mgmtClient, key, reqHash string, resp store.StoredResponse, ttl time.Duration) error
	AbandonIdempotent(ctx context.Context, mgmtClient, key, reqHash string) error
}

// Handler は API のハンドラー。
type Handler struct {
	Log *slog.Logger
	// MgmtClient は要求のクライアント証明書から管理クライアントの識別名を返す（監査ログとログに残す）。
	MgmtClient func(*http.Request) string
	// Downstream は下流の名前から中継のクライアントを返す（登録簿の読み直しで替わる）。
	Downstream func(downstream.Name) Relayer
	Store      Store
	Audit      *audit.Recorder
	// Status は /status に出す、provisioner 自身の情報と下流の状態。
	Status *StatusSource
}

// Routes は API のルーティングを返す。
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+basePath+"/status", h.getStatus)
	mux.HandleFunc("GET "+basePath+"/audit-logs", h.listAuditLogs)
	for _, name := range downstream.Names {
		for _, rt := range allowlist[name] {
			handler := h.relayHandler(name, rt)
			if rt.write() {
				handler = h.idempotent(handler)
			}
			mux.HandleFunc(rt.method+" "+basePath+"/"+string(name)+rt.path, handler)
		}
	}
	return h.observe(checkOperator(h.withFallback(mux)))
}

// --- 共通の処理 ---

type mgmtClientKey struct{}

// mgmtClientFrom は observe がコンテキストに入れた管理クライアントの識別名を返す。
func mgmtClientFrom(ctx context.Context) string {
	name, _ := ctx.Value(mgmtClientKey{}).(string)
	return name
}

// observe はトレースID を決めて応答ヘッダーとコンテキストに入れ、要求ごとのログ（ADMIN_REQUEST）を出す。
// 受け取った X-Trace-ID が形式に合えばそのまま使い、なければ採番する。要求と応答の本文はログに出さない（鍵を含みうるため）。
func (h *Handler) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get(trace.Header)
		if !trace.Valid(id) {
			id = trace.New()
		}
		w.Header().Set(trace.Header, id)
		mgmtClient := h.MgmtClient(r)
		ctx := context.WithValue(trace.With(r.Context(), id), mgmtClientKey{}, mgmtClient)
		r = r.WithContext(ctx)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		srcIP, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			srcIP = r.RemoteAddr
		}
		h.Log.Info("request completed",
			"event_id", "ADMIN_REQUEST",
			"trace_id", id,
			"method", r.Method,
			"path", r.URL.Path,
			"http_status", rec.status,
			"latency_ms", time.Since(start).Milliseconds(),
			"mgmt_client", mgmtClient,
			"operator", operatorOf(r),
			"src_ip", srcIP,
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status, s.wroteHeader = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

const operatorHeader = "X-Operator-Id"

// operatorPattern は X-Operator-Id の形式（下流と同じ）。
var operatorPattern = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

// operatorOf は要求の操作者を返す。形式の違う値は 400 で断っているので、記録しない。
func operatorOf(r *http.Request) string {
	if v := r.Header.Get(operatorHeader); operatorPattern.MatchString(v) {
		return v
	}
	return ""
}

// checkOperator は X-Operator-Id の形式を確かめ、下流に渡す操作者をコンテキストに入れる（任意。下流と同じ）。
func checkOperator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get(operatorHeader)
		if v == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !operatorPattern.MatchString(v) {
			badParams(causeOptionalIEIncorrect, invalidParam{operatorHeader, "must match " + operatorPattern.String()}).write(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(downstream.WithOperator(r.Context(), v)))
	})
}

// fallbackMethods は、どのメソッドなら受け付けるかを調べるときに試すメソッド。
var fallbackMethods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// withFallback は、ルーティングに合わない要求に ProblemDetails を返す。
// 下流の名前で始まるパスは、許していない口として 404（ROUTE_NOT_ALLOWED）にする（設計概要 §6.2）。メソッドが違うだけでも同じ。
// それ以外（provisioner 自身の口）は、パスがなければ 404、メソッドだけが違えば 405（Allow つき）。
func (h *Handler) withFallback(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		for _, name := range downstream.Names {
			if strings.HasPrefix(r.URL.Path, basePath+"/"+string(name)+"/") {
				h.Log.WarnContext(r.Context(), "route is not allowed", "event_id", "RELAY_DENIED", "trace_id", trace.From(r.Context()),
					"downstream", string(name), "method", r.Method, "path", r.URL.Path, "mgmt_client", mgmtClientFrom(r.Context()))
				p := newProblem(http.StatusNotFound, causeRouteNotAllowed, "the route is not allowed by aka-provisioner")
				p.Downstream = string(name)
				p.write(w)
				return
			}
		}
		var allow []string
		for _, m := range fallbackMethods {
			probe := r.Clone(r.Context())
			probe.Method = m
			if _, pattern := mux.Handler(probe); pattern != "" {
				allow = append(allow, m)
			}
		}
		if len(allow) == 0 {
			newProblem(http.StatusNotFound, "", "").write(w)
			return
		}
		if slices.Contains(allow, http.MethodGet) {
			allow = append(allow, http.MethodHead)
		}
		w.Header().Set("Allow", strings.Join(allow, ", "))
		newProblem(http.StatusMethodNotAllowed, "", "").write(w)
	})
}
