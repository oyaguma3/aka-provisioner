// Package audit は監査ログを、Valkey の Stream と日ごとの JSONL のファイルに書く（設計概要 §10）。
// 形は aka-vecgen・aka-radius と同じ（Stream の名前と項目、ファイルの名前と行の形、日本時間の区切り、前の日の gzip、持つ日数）。
// どちらかに書けなくても操作は成功のまま（ログに AUDIT_WRITE_ERR を出す）。
package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/store"
)

// storeTimeout は監査ログを Stream に書くときのタイムアウト。
const storeTimeout = 5 * time.Second

// Store は監査ログを書く先（Valkey の Stream）。
type Store interface {
	AppendAudit(ctx context.Context, e store.AuditEntry, maxLen int64) (string, error)
}

// Recorder は監査ログを記録する。
type Recorder struct {
	Log    *slog.Logger
	Store  Store
	MaxLen int64
	// Files は日ごとのファイル。nil なら書かない。
	Files *Files
	// Now は記録の時刻を返す（試験用）。nil なら time.Now。
	Now func() time.Time
}

// Record は監査ログを記録する。記録の時刻は呼んだ時刻にする。
func (r *Recorder) Record(ctx context.Context, e store.AuditEntry) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	e.Time = now()
	r.Log.InfoContext(ctx, "audit", "event_id", "AUDIT", "trace_id", e.TraceID, "operator", e.Operator,
		"mgmt_client", e.MgmtClient, "action", e.Action, "target", e.Target, "detail", e.Detail)
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	id, err := r.Store.AppendAudit(sctx, e, r.MaxLen)
	if err != nil {
		r.writeError(sctx, "stream", e, err)
	}
	if r.Files != nil {
		if err := r.Files.Append(e, id); err != nil {
			r.writeError(sctx, "file", e, err)
		}
	}
}

func (r *Recorder) writeError(ctx context.Context, to string, e store.AuditEntry, err error) {
	r.Log.ErrorContext(ctx, "failed to write audit log", "event_id", "AUDIT_WRITE_ERR", "to", to,
		"trace_id", e.TraceID, "action", e.Action, "target", e.Target, "error", err.Error())
}
