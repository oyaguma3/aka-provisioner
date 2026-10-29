package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/store"
	"github.com/oyaguma3/aka-provisioner/internal/trace"
)

const (
	// maxBodyBytes は要求の本文の上限（一括取得の 500 件の要求を通せるように。設計概要 §6.3）。
	maxBodyBytes = 1 << 20
	// idempotencyTTL は、Idempotency-Key の要求の応答を覚えておく時間（設計概要 §7）。
	idempotencyTTL = 24 * time.Hour
	// idempotencyPendingTTL は、処理中の記録の有効期限。下流の呼び出しのタイムアウトの上限（30 秒）より長くする。
	// プロセスが落ちても、この時間が過ぎれば同じキーでやり直せる。
	idempotencyPendingTTL = 60 * time.Second

	idempotencyKeyHeader     = "Idempotency-Key"
	idempotentReplayedHeader = "Idempotent-Replayed"
	storeTimeout             = 5 * time.Second
)

// idempotencyKeyPattern は Idempotency-Key の形式（印字可能 ASCII 1〜128 文字）。
var idempotencyKeyPattern = regexp.MustCompile(`^[\x21-\x7E]{1,128}$`)

// readBody は要求の本文を上限まで読む。上限を超えたら 400 を返す。
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, *problem) {
	if r.Body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, newProblem(http.StatusBadRequest, causeInvalidMsgFormat, "request body is too large")
		}
		return nil, newProblem(http.StatusBadRequest, causeInvalidMsgFormat, "request body could not be read")
	}
	return b, nil
}

// idempotent は下流を書き換える要求の Idempotency-Key を扱う（設計概要 §7）。キーは必須で、なければ 400。
// 同じ管理クライアントから同じキーで届いた要求には、最初の応答をそのまま返し、ハンドラーは呼ばない。
// 応答を覚えるのは 4xx までで、5xx（下流に接続できない等）は覚えずに、同じキーでやり直せるようにする。
func (h *Handler) idempotent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(idempotencyKeyHeader)
		if key == "" {
			badParams(causeMandatoryIEMissing, invalidParam{idempotencyKeyHeader, "is required for a request that changes a downstream"}).write(w)
			return
		}
		if !idempotencyKeyPattern.MatchString(key) {
			badParams(causeOptionalIEIncorrect, invalidParam{idempotencyKeyHeader, "must match " + idempotencyKeyPattern.String()}).write(w)
			return
		}
		body, p := readBody(w, r)
		if p != nil {
			p.write(w)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		mgmtClient := mgmtClientFrom(r.Context())
		hash := requestHash(r, body)
		state, stored, err := h.Store.BeginIdempotent(r.Context(), mgmtClient, key, hash, idempotencyPendingTTL)
		if err != nil {
			h.internalError(w, r, err)
			return
		}
		switch state {
		case store.IdempotencyInProgress:
			newProblem(http.StatusConflict, causeOperationInProgress, "a request with the same Idempotency-Key is in progress").write(w)
			return
		case store.IdempotencyMismatch:
			newProblem(http.StatusUnprocessableEntity, causeIdempotencyKeyMismatch,
				"the Idempotency-Key was used for a different request").write(w)
			return
		case store.IdempotencyReplay:
			if stored.ContentType != "" {
				w.Header().Set("Content-Type", stored.ContentType)
			}
			if stored.Location != "" {
				w.Header().Set("Location", stored.Location)
			}
			w.Header().Set(idempotentReplayedHeader, "true")
			h.Log.InfoContext(r.Context(), "replayed the stored response", "event_id", "IDEMPOTENT_REPLAYED",
				"trace_id", trace.From(r.Context()), "mgmt_client", mgmtClient, "method", r.Method, "path", r.URL.Path, "status", stored.Status)
			w.WriteHeader(stored.Status)
			w.Write(stored.Body)
			return
		}

		rec := &captureWriter{ResponseWriter: w, status: http.StatusOK}
		next(rec, r)

		// 要求が途中で切れても、記録は残す。
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), storeTimeout)
		defer cancel()
		if rec.status >= 500 {
			err = h.Store.AbandonIdempotent(ctx, mgmtClient, key, hash)
		} else {
			err = h.Store.CompleteIdempotent(ctx, mgmtClient, key, hash, store.StoredResponse{
				Status: rec.status, ContentType: w.Header().Get("Content-Type"), Location: w.Header().Get("Location"), Body: rec.body.Bytes(),
			}, idempotencyTTL)
		}
		if err != nil {
			h.Log.ErrorContext(ctx, "store idempotent response", "error", err)
		}
	}
}

// requestHash は、Idempotency-Key の要求が同じ内容かを比べるためのハッシュ（メソッド・パス・クエリ・本文）。
func requestHash(r *http.Request, body []byte) string {
	h := sha256.New()
	for _, s := range []string{r.Method, r.URL.EscapedPath(), r.URL.RawQuery} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// captureWriter は応答を書きながら、ステータスと本文を記録する。
type captureWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (c *captureWriter) WriteHeader(code int) {
	if !c.wroteHeader {
		c.status, c.wroteHeader = code, true
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	c.wroteHeader = true
	c.body.Write(b)
	return c.ResponseWriter.Write(b)
}

// Unwrap は http.ResponseController が元の ResponseWriter を使えるようにする。
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
