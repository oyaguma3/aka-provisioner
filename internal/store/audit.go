package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// keyAudit は監査ログの Stream（aka-vecgen・aka-radius と同じ名前と項目）。
const keyAudit = "audit"

// AuditEntry は監査ログの 1 件（設計概要 §10）。
type AuditEntry struct {
	ID         string    // エントリID。記録時は空
	Time       time.Time // 記録日時。エントリID から求める
	Operator   string    // 操作者（X-Operator-Id）
	MgmtClient string    // 管理クライアントの識別名
	Action     string    // 操作（vecgen.subscriber.create 等）
	Target     string    // 対象（IMSI）
	Detail     string    // 変更の内容の JSON（下流の名前と項目の名前。値は残さない）
	TraceID    string    // トレースID
}

// AppendAudit は監査ログを Stream に追加し、エントリID を返す。件数は maxLen 程度に保つ（MAXLEN ~）。
func (s *Store) AppendAudit(ctx context.Context, e AuditEntry, maxLen int64) (string, error) {
	cmd := s.c.B().Xadd().Key(keyAudit).Maxlen().Almost().Threshold(strconv.FormatInt(maxLen, 10)).Id("*").FieldValue().
		FieldValue("operator", e.Operator).
		FieldValue("mgmt_client", e.MgmtClient).
		FieldValue("action", e.Action).
		FieldValue("target", e.Target).
		FieldValue("detail", e.Detail).
		FieldValue("trace_id", e.TraceID).Build()
	id, err := s.c.Do(ctx, cmd).ToString()
	if err != nil {
		return "", fmt.Errorf("append audit: %w", err)
	}
	return id, nil
}

// ListAudit は監査ログを新しい順に返す。before を指定すると、そのエントリより古いものだけを返す。
// 続きがあれば nextBefore に次の before を返す。
func (s *Store) ListAudit(ctx context.Context, before string, limit int) (entries []AuditEntry, nextBefore string, err error) {
	end := "+"
	if before != "" {
		end = "(" + before
	}
	res, err := s.c.Do(ctx, s.c.B().Xrevrange().Key(keyAudit).End(end).Start("-").Count(int64(limit)+1).Build()).AsXRange()
	if err != nil {
		return nil, "", fmt.Errorf("list audit: %w", err)
	}
	if len(res) > limit {
		res = res[:limit]
		nextBefore = res[limit-1].ID
	}
	entries = make([]AuditEntry, len(res))
	for i, r := range res {
		f := r.FieldValues
		entries[i] = AuditEntry{
			ID: r.ID, Time: auditTime(r.ID),
			Operator: f["operator"], MgmtClient: f["mgmt_client"], Action: f["action"], Target: f["target"],
			Detail: f["detail"], TraceID: f["trace_id"],
		}
	}
	return entries, nextBefore, nil
}

// auditTime はエントリID（<ミリ秒>-<連番>）から記録日時を求める。
func auditTime(id string) time.Time {
	ms, _, _ := strings.Cut(id, "-")
	v, err := strconv.ParseInt(ms, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}
