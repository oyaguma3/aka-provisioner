// Package api は provisioner の API（/admin/v1）のハンドラー。
// API 仕様は docs/openapi/provisioner-api.yaml を参照。
package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/akaapi"
	"github.com/oyaguma3/aka-provisioner/internal/audit"
	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/plmn"
	"github.com/oyaguma3/aka-provisioner/internal/provapi"
	"github.com/oyaguma3/aka-provisioner/internal/store"
	"github.com/oyaguma3/aka-provisioner/internal/trace"
)

// basePath は API のパスの接頭辞。
const basePath = "/admin/v1"

// Relayer は下流に要求をそのまま中継する操作。*downstream.Client が満たす。
type Relayer interface {
	BaseURL() string
	Relay(ctx context.Context, req downstream.RelayRequest) (downstream.RelayResponse, error)
}

// ProvAPI は provisioner が使う provisioning-api の操作。*provapi.Client が満たす。
type ProvAPI interface {
	Relayer
	Status(ctx context.Context) (provapi.Status, error)
}

// AkaAPI は provisioner が使う aka-vecgen の管理API の操作。*akaapi.Client が満たす。
type AkaAPI interface {
	Relayer
	Status(ctx context.Context) (akaapi.Status, error)
	GetAVClient(ctx context.Context, id int64) (akaapi.AVClient, error)
}

// Store は provisioner 専用の Valkey の操作。*store.Store が満たす。
type Store interface {
	Ping(ctx context.Context) error

	ListAudit(ctx context.Context, before string, limit int) ([]store.AuditEntry, string, error)

	AcquireIMSILock(ctx context.Context, imsi string, ttl time.Duration) (token string, err error)
	ReleaseIMSILock(ctx context.Context, imsi, token string) error

	BeginIdempotent(ctx context.Context, mgmtClient, key, reqHash string, pendingTTL time.Duration) (store.IdempotencyState, store.StoredResponse, error)
	CompleteIdempotent(ctx context.Context, mgmtClient, key, reqHash string, resp store.StoredResponse, ttl time.Duration) error
	AbandonIdempotent(ctx context.Context, mgmtClient, key, reqHash string) error
}

// Handler は API のハンドラー。
type Handler struct {
	Log *slog.Logger
	// MgmtClient はリクエストから管理クライアントの識別名を取り出す。
	MgmtClient func(*http.Request) string

	Prov ProvAPI
	// Aka は aka-vecgen を扱わない設定（PROVISIONER_AKA_URL が空）なら nil。
	Aka AkaAPI
	// AkaAVClientID は、本PoCの vector-gateway の AVクライアントID（Aka が nil なら 0）。
	AkaAVClientID int64
	PLMNMap       plmn.Map
	Store         Store
	// Subscribers は加入者の統合操作。
	Subscribers SubscriberService
	// DownstreamTimeout は、状態の確認で下流を呼ぶときの上限時間。
	DownstreamTimeout time.Duration
	// Audit は監査ログを残す。
	Audit *audit.Recorder

	Version   string
	StartedAt time.Time
}

// Routes は API のルーティングを返す。書き込みは idempotent で Idempotency-Key を扱う。
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+basePath+"/status", h.getStatus)
	mux.HandleFunc("GET "+basePath+"/audit-logs", h.listAuditLogs)

	// 加入者の統合操作。
	mux.HandleFunc("GET "+basePath+"/subscribers", h.listSubscribers)
	mux.HandleFunc("POST "+basePath+"/subscribers", h.idempotent(h.createSubscriber))
	mux.HandleFunc("GET "+basePath+"/subscribers/{imsi}", h.getSubscriber)
	mux.HandleFunc("PATCH "+basePath+"/subscribers/{imsi}", h.idempotent(h.updateSubscriber))
	mux.HandleFunc("DELETE "+basePath+"/subscribers/{imsi}", h.idempotent(h.deleteSubscriber))
	mux.HandleFunc("GET "+basePath+"/subscribers/{imsi}/keys", h.getSubscriberKeys)

	// 操作の記録。
	mux.HandleFunc("GET "+basePath+"/operations", h.listOperations)
	mux.HandleFunc("GET "+basePath+"/operations/{operationId}", h.getOperation)
	mux.HandleFunc("POST "+basePath+"/operations/{operationId}/retry", h.idempotent(h.retryOperation))
	mux.HandleFunc("POST "+basePath+"/operations/{operationId}/dismiss", h.idempotent(h.dismissOperation))

	// 中継（prov）。
	mux.HandleFunc("GET "+basePath+"/clients", h.relayClients)
	mux.HandleFunc("POST "+basePath+"/clients", h.idempotent(h.relayClients))
	mux.HandleFunc("GET "+basePath+"/clients/{clientId}", h.relayClient)
	mux.HandleFunc("PATCH "+basePath+"/clients/{clientId}", h.idempotent(h.relayClient))
	mux.HandleFunc("DELETE "+basePath+"/clients/{clientId}", h.idempotent(h.relayClient))
	mux.HandleFunc("GET "+basePath+"/clients/{clientId}/secret", h.relayClientSecret)
	mux.HandleFunc("GET "+basePath+"/policies", h.relayPolicies)
	mux.HandleFunc("GET "+basePath+"/policies/{imsi}", h.relayPolicy)
	mux.HandleFunc("PUT "+basePath+"/policies/{imsi}", h.idempotent(h.relayPolicy))
	mux.HandleFunc("DELETE "+basePath+"/policies/{imsi}", h.idempotent(h.relayPolicy))
	mux.HandleFunc("PUT "+basePath+"/policies/{imsi}/status", h.idempotent(h.relayPolicyStatus))
	mux.HandleFunc("GET "+basePath+"/sessions", h.relaySessions)
	mux.HandleFunc("GET "+basePath+"/prov/audit-logs", h.relayProvAuditLogs)

	// 中継（aka。読み取りだけ）。
	mux.HandleFunc("GET "+basePath+"/aka/audit-logs", h.relayAkaAuditLogs)
	mux.HandleFunc("GET "+basePath+"/aka/av-clients/{clientId}", h.relayAkaAVClient)

	return h.observe(checkOperator(withFallback(mux)))
}

// ---- ミドルウェア ----

type mgmtClientKey struct{}

// mgmtClientFrom は、リクエストを送ってきた管理クライアントの識別名を返す。
func mgmtClientFrom(ctx context.Context) string {
	name, _ := ctx.Value(mgmtClientKey{}).(string)
	return name
}

// observe は X-Trace-ID ヘッダーのトレースID を使い（なければ採番し）、コンテキストと応答の X-Trace-ID ヘッダーに入れる。
// 管理クライアントの識別名もコンテキストに入れる。処理を終えたリクエストは request completed として記録する。
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

		operator := r.Header.Get(operatorHeader)
		if !operatorPattern.MatchString(operator) {
			operator = "" // 形式の違う値は 400 で断っており、そのままは記録しない
		}
		srcIP, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			srcIP = r.RemoteAddr
		}
		// クエリ文字列は出さない（検索条件などを残さないため）。
		h.Log.Info("request completed",
			"trace_id", id,
			"method", r.Method,
			"path", r.URL.Path,
			"http_status", rec.status,
			"latency_ms", time.Since(start).Milliseconds(),
			"mgmt_client", mgmtClient,
			"operator", operator,
			"src_ip", srcIP,
		)
	})
}

// statusRecorder は応答のステータスを記録する。
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

// Unwrap は http.ResponseController が元の ResponseWriter を使えるようにする。
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

const operatorHeader = "X-Operator-Id"

// operatorPattern は X-Operator-Id の形式（下流と同じ）。
var operatorPattern = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

// checkOperator は X-Operator-Id ヘッダーの形式を確かめ、下流に渡すためにコンテキストに入れる。ヘッダーは省略できる。
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

// fallbackMethods は、該当するパターンがないときに、メソッドだけが違うのかを調べるメソッド。
var fallbackMethods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// withFallback は、該当しないパスを 404、メソッドを 405 の ProblemDetails（cause なし）で返す（下流と同じ）。
func withFallback(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
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
