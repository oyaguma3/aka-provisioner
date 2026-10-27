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
)

const (
	// maxBodyBytes は要求の本文の上限（下流と同じ）。
	maxBodyBytes = 256 << 10
	// LockTTL は IMSI ごとのロックの有効期限。1 つの操作にかかる最大の時間より長くする（設計概要 §9.1）。
	// 加入者の統合操作（internal/subscriber）も同じ値を使う。
	LockTTL = 60 * time.Second
	// idempotencyTTL は、Idempotency-Key の要求の応答を覚えておく時間（設計概要 §9.2）。
	idempotencyTTL = 24 * time.Hour
	// idempotencyPendingTTL は、処理中の記録の有効期限。プロセスが落ちても、この時間が過ぎれば同じキーでやり直せる。
	idempotencyPendingTTL = LockTTL

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

// idempotencyControl は、処理中のハンドラーが「この応答は覚えない」と伝えるためのもの。
type idempotencyControl struct{ noStore bool }

type idempotencyControlKey struct{}

// doNotRemember は、この応答を Idempotency-Key で覚えないようにする（同じ IMSI の操作が処理中、のような一時的な応答）。
func doNotRemember(r *http.Request) {
	if c, ok := r.Context().Value(idempotencyControlKey{}).(*idempotencyControl); ok {
		c.noStore = true
	}
}

// idempotent は書き込みの要求の Idempotency-Key を扱う（設計概要 §9.2）。
// 同じ管理クライアントから同じキーで届いた要求には、最初の応答をそのまま返し、ハンドラーは呼ばない。
// 応答を覚えるのは 4xx までで、5xx（下流に接続できない等）は覚えずに、同じキーでやり直せるようにする。
func (h *Handler) idempotent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(idempotencyKeyHeader)
		if key == "" {
			next(w, r)
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
			w.WriteHeader(stored.Status)
			w.Write(stored.Body)
			return
		}

		ctrl := &idempotencyControl{}
		rec := &captureWriter{ResponseWriter: w, status: http.StatusOK}
		next(rec, r.WithContext(context.WithValue(r.Context(), idempotencyControlKey{}, ctrl)))

		// 要求が途中で切れても、記録は残す。
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), storeTimeout)
		defer cancel()
		if rec.status >= 500 || ctrl.noStore {
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

// lockIMSI は IMSI のロックを取る。取れなければ応答を書いて false を返す（同じ IMSI の操作が処理中なら 409）。
// 取れたら、解放する関数を返す。
func (h *Handler) lockIMSI(w http.ResponseWriter, r *http.Request, imsi string) (unlock func(), ok bool) {
	token, err := h.Store.AcquireIMSILock(r.Context(), imsi, LockTTL)
	switch {
	case errors.Is(err, store.ErrLocked):
		doNotRemember(r)
		newProblem(http.StatusConflict, causeOperationInProgress, "another operation on the same IMSI is in progress").write(w)
		return nil, false
	case err != nil:
		h.internalError(w, r, err)
		return nil, false
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), storeTimeout)
		defer cancel()
		if err := h.Store.ReleaseIMSILock(ctx, imsi, token); err != nil {
			h.Log.ErrorContext(ctx, "release imsi lock", "imsi", imsi, "error", err)
		}
	}, true
}
