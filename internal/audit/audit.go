// Package audit は provisioner の監査ログを残す（設計概要 §10.2）。
// 標準出力（JSON のログ。正本）に出し、あわせて Valkey の Stream に保存する。
// HTTP の要求の操作（internal/api）と、ワーカーの処理（internal/subscriber）の両方が使う。
package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/store"
)

const storeTimeout = 5 * time.Second

// Store は監査ログの保存先。*store.Store が満たす。
type Store interface {
	AppendAudit(ctx context.Context, e store.AuditEntry, maxLen int64) error
}

// Recorder は監査ログを残す。
type Recorder struct {
	Log   *slog.Logger
	Store Store
	// MaxLen は Stream に残す件数の上限。
	MaxLen int64
}

// Record は監査ログを残す。Valkey への保存に失敗しても、操作自体は成功として扱い、エラーをログに残す
// （標準出力のログが正本）。e.Details に秘密の値を入れてはならない。
func (r *Recorder) Record(ctx context.Context, e store.AuditEntry) {
	r.Log.InfoContext(ctx, "audit", "trace_id", e.TraceID, "operator", e.Operator, "mgmt_client", e.MgmtClient,
		"action", e.Action, "target", e.Target, "operation_id", e.OperationID, "result", e.Result, "details", e.Details)
	// 要求が途中で切れても記録は残す。
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	if err := r.Store.AppendAudit(sctx, e, r.MaxLen); err != nil {
		r.Log.ErrorContext(sctx, "append audit", "action", e.Action, "target", e.Target, "error", err)
	}
}
