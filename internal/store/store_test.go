package store

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
)

// openTestStore は、接続先を環境変数で指定したときだけ実際の Valkey に接続する。
// 論理データベース 1 番を使う（テストで全データを消すことがある）。
func openTestStore(t *testing.T) *Store {
	t.Helper()
	addr := os.Getenv("PROVISIONER_TEST_VALKEY_ADDR")
	if addr == "" {
		t.Skip("PROVISIONER_TEST_VALKEY_ADDR is not set")
	}
	s, err := Open(t.Context(), Options{Addr: addr, Password: os.Getenv("PROVISIONER_TEST_VALKEY_PASSWORD"), DB: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.c.Do(t.Context(), s.c.B().Flushdb().Build()).Error(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPing(t *testing.T) {
	s := openTestStore(t)
	if err := s.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestAudit(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	if got, next, err := s.ListAudit(ctx, "", 10); err != nil || len(got) != 0 || next != "" {
		t.Errorf("empty: %v, %q, %v", got, next, err)
	}
	for i := range 5 {
		e := AuditEntry{Operator: "alice", MgmtClient: "ossbss", Action: "vecgen.subscriber.create", Target: fmt.Sprint(i),
			TraceID: "t" + fmt.Sprint(i), Detail: `{"downstream":"vecgen"}`}
		if id, err := s.AppendAudit(ctx, e, 1000); err != nil || id == "" {
			t.Fatal(id, err)
		}
	}
	// 新しい順に返り、nextBefore で続きをたどれる。
	var targets []string
	before := ""
	for {
		got, next, err := s.ListAudit(ctx, before, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range got {
			if e.Operator != "alice" || e.MgmtClient != "ossbss" || e.Action != "vecgen.subscriber.create" ||
				e.TraceID != "t"+e.Target || e.Detail != `{"downstream":"vecgen"}` || time.Since(e.Time) > time.Minute || e.ID == "" {
				t.Errorf("entry = %+v", e)
			}
			targets = append(targets, e.Target)
		}
		if next == "" {
			break
		}
		before = next
	}
	if want := []string{"4", "3", "2", "1", "0"}; !slices.Equal(targets, want) {
		t.Errorf("targets = %v, want %v", targets, want)
	}
}

func TestIdempotency(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	begin := func(client, key, hash string) (IdempotencyState, StoredResponse) {
		t.Helper()
		st, resp, err := s.BeginIdempotent(ctx, client, key, hash, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return st, resp
	}

	if st, _ := begin("bff", "k1", "h1"); st != IdempotencyNew {
		t.Fatalf("first: %v", st)
	}
	if st, _ := begin("bff", "k1", "h1"); st != IdempotencyInProgress {
		t.Errorf("while pending: %v", st)
	}
	if st, _ := begin("bff", "k1", "h2"); st != IdempotencyMismatch {
		t.Errorf("other request: %v", st)
	}
	// 管理クライアントが違えば別のキー。
	if st, _ := begin("other", "k1", "h2"); st != IdempotencyNew {
		t.Errorf("other client: %v", st)
	}

	want := StoredResponse{Status: 201, ContentType: "application/json", Location: "/admin/v2/vecgen/subscribers/440100123456789", Body: []byte(`{"imsi":"440100123456789"}`)}
	if err := s.CompleteIdempotent(ctx, "bff", "k1", "h1", want, time.Hour); err != nil {
		t.Fatal(err)
	}
	if st, got := begin("bff", "k1", "h1"); st != IdempotencyReplay || got.Status != 201 || got.ContentType != want.ContentType ||
		got.Location != want.Location || string(got.Body) != string(want.Body) {
		t.Errorf("replay: %v, %+v", st, got)
	}
	if st, _ := begin("bff", "k1", "h2"); st != IdempotencyMismatch {
		t.Errorf("other request after done: %v", st)
	}
	// 完了した記録は、内容の違う Complete / Abandon では変わらない。
	if err := s.AbandonIdempotent(ctx, "bff", "k1", "h1"); err != nil {
		t.Fatal(err)
	}
	if st, _ := begin("bff", "k1", "h1"); st != IdempotencyReplay {
		t.Errorf("abandon after done: %v", st)
	}

	// 処理中を取り消すと、同じキーでやり直せる。
	if st, _ := begin("bff", "k2", "h1"); st != IdempotencyNew {
		t.Fatal(st)
	}
	if err := s.AbandonIdempotent(ctx, "bff", "k2", "h1"); err != nil {
		t.Fatal(err)
	}
	if st, _ := begin("bff", "k2", "h1"); st != IdempotencyNew {
		t.Errorf("after abandon: %v", st)
	}

	// 処理中の記録は pendingTTL で消える（プロセスが落ちても、やり直せる）。
	if st, _, err := s.BeginIdempotent(ctx, "bff", "k3", "h1", 50*time.Millisecond); err != nil || st != IdempotencyNew {
		t.Fatal(st, err)
	}
	time.Sleep(100 * time.Millisecond)
	if st, _ := begin("bff", "k3", "h1"); st != IdempotencyNew {
		t.Errorf("after pending ttl: %v", st)
	}
	// 本文は空でも覚えられる（204 の応答）。
	if err := s.CompleteIdempotent(ctx, "bff", "k3", "h1", StoredResponse{Status: 204}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if st, got := begin("bff", "k3", "h1"); st != IdempotencyReplay || got.Status != 204 || len(got.Body) != 0 {
		t.Errorf("204 replay: %v, %+v", st, got)
	}
}
