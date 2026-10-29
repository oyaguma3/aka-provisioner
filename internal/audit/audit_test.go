package audit

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/store"
)

// fakeStore は Stream の代わり。err を返すように切り替えられる。
type fakeStore struct {
	entries []store.AuditEntry
	err     error
}

func (f *fakeStore) AppendAudit(_ context.Context, e store.AuditEntry, _ int64) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.entries = append(f.entries, e)
	return "1700000000000-" + string(rune('0'+len(f.entries))), nil
}

func TestRecord(t *testing.T) {
	var logs bytes.Buffer
	dir := t.TempDir()
	st := &fakeStore{}
	now := time.Date(2026, 10, 30, 23, 30, 0, 0, time.UTC) // 日本時間では 10 月 31 日
	r := &Recorder{Log: slog.New(slog.NewJSONHandler(&logs, nil)), Store: st, MaxLen: 100, Files: NewFiles(dir, 14),
		Now: func() time.Time { return now }}
	r.Record(t.Context(), store.AuditEntry{Operator: "alice", MgmtClient: "ossbss", Action: "vecgen.subscriber.create",
		Target: "440100123456789", Detail: `{"downstream":"vecgen","fields":["imsi","ki","opc"]}`, TraceID: "t-1"})
	if len(st.entries) != 1 || !st.entries[0].Time.Equal(now) || st.entries[0].Action != "vecgen.subscriber.create" {
		t.Errorf("stream = %+v", st.entries)
	}
	lines := readLines(t, filepath.Join(dir, "audit-20261031.jsonl"))
	if len(lines) != 1 || lines[0]["operator"] != "alice" || lines[0]["mgmtClient"] != "ossbss" || lines[0]["streamId"] != "1700000000000-1" ||
		lines[0]["traceId"] != "t-1" || lines[0]["detail"].(map[string]any)["downstream"] != "vecgen" ||
		lines[0]["time"] != "2026-10-31T08:30:00+09:00" {
		t.Errorf("file lines = %v", lines)
	}
	if !strings.Contains(logs.String(), `"event_id":"AUDIT"`) {
		t.Errorf("log = %s", logs.String())
	}

	// Stream に書けなくてもファイルには書く（streamId なし）。
	st.err = errors.New("valkey is down")
	r.Record(t.Context(), store.AuditEntry{Action: "radius.subscriber.delete"})
	if !strings.Contains(logs.String(), `"event_id":"AUDIT_WRITE_ERR"`) || !strings.Contains(logs.String(), `"to":"stream"`) {
		t.Error("no AUDIT_WRITE_ERR log")
	}
	if lines := readLines(t, filepath.Join(dir, "audit-20261031.jsonl")); len(lines) != 2 || lines[1]["streamId"] != nil {
		t.Errorf("file lines after the stream failure = %v", lines)
	}
}

func TestRecordFileError(t *testing.T) {
	var logs bytes.Buffer
	r := &Recorder{Log: slog.New(slog.NewJSONHandler(&logs, nil)), Store: &fakeStore{}, MaxLen: 100,
		Files: NewFiles(filepath.Join(t.TempDir(), "missing"), 14)}
	r.Record(t.Context(), store.AuditEntry{Action: "vecgen.subscriber.update"})
	if !strings.Contains(logs.String(), `"to":"file"`) {
		t.Errorf("log = %s", logs.String())
	}
	if NewFiles("", 14) != nil {
		t.Error(`NewFiles("") should be nil`)
	}
}

func TestCheckWritableDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit")
	if err := CheckWritableDir(dir); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("left files: %v", ents)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckWritableDir(file); err == nil {
		t.Error("CheckWritableDir on a file: want error")
	}
}

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		r = zr
	}
	var out []map[string]any
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

func TestMaintain(t *testing.T) {
	dir := t.TempDir()
	f := NewFiles(dir, 14)
	now := time.Date(2026, 10, 28, 3, 0, 0, 0, jst)
	write := func(name, content string, mtime time.Time) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	write("audit-20261028.jsonl", `{"a":1}`+"\n", now)                       // 今日: そのまま
	write("audit-20261027.jsonl", `{"a":2}`+"\n", now.Add(-time.Hour))       // 昨日: 圧縮する
	write("audit-20261026.jsonl", `{"a":3}`+"\n", now.Add(-5*time.Minute))   // 書いたばかり: まだ圧縮しない
	write("audit-20261014.jsonl.gz", "old", now.Add(-14*24*time.Hour))       // 14 日前: 残す
	write("audit-20261013.jsonl.gz", "older", now.Add(-15*24*time.Hour))     // 15 日前: 消す
	write("audit-20261001.jsonl", `{"a":4}`+"\n", now.Add(-27*24*time.Hour)) // 古い圧縮前: 消す
	write("other.txt", "x", now.Add(-30*24*time.Hour))                       // 関係のないファイル: 触らない

	if err := f.Maintain(now); err != nil {
		t.Fatal(err)
	}
	var names []string
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		names = append(names, e.Name())
	}
	want := []string{"audit-20261014.jsonl.gz", "audit-20261026.jsonl", "audit-20261027.jsonl.gz", "audit-20261028.jsonl", "other.txt"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("files = %v, want %v", names, want)
	}
	if lines := readLines(t, filepath.Join(dir, "audit-20261027.jsonl.gz")); len(lines) != 1 || lines[0]["a"] != float64(2) {
		t.Errorf("compressed content = %v", lines)
	}
	if err := NewFiles(filepath.Join(dir, "none"), 14).Maintain(now); err == nil {
		t.Error("Maintain on a missing directory: want error")
	}
}
