package audit

import (
	"compress/gzip"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/store"
)

// jst は日ごとのファイルの区切りに使う日本時間（夏時間はないので固定の +09:00 にする）。
var jst = time.FixedZone("JST", 9*60*60)

// compressAfter は、前の日のファイルを圧縮するまで待つ時間（日の変わり目に、別のプロセスが書き終えるのを待つ）。
const compressAfter = 10 * time.Minute

var fileName = regexp.MustCompile(`^audit-([0-9]{8})\.jsonl(\.gz)?$`)

// Files は日ごとの監査ログのファイル。
type Files struct {
	dir       string
	retention int // 日数
}

// NewFiles は dir に日ごとのファイルを書く Files を作る。dir が空なら nil を返す（書かない）。
func NewFiles(dir string, retentionDays int) *Files {
	if dir == "" {
		return nil
	}
	return &Files{dir: dir, retention: retentionDays}
}

// line はファイルの 1 行。
type line struct {
	Time       string         `json:"time"`
	Operator   string         `json:"operator"`
	MgmtClient string         `json:"mgmtClient"`
	Action     string         `json:"action"`
	Target     string         `json:"target"`
	Detail     jsontext.Value `json:"detail,omitempty"`
	TraceID    string         `json:"traceId,omitempty"`
	StreamID   string         `json:"streamId,omitempty"`
}

// Append は監査ログを 1 行追記する。ファイルは e.Time の日本時間の日付で選ぶ。
// streamID は Stream のエントリID（Stream に書けなかったときは空）。
func (f *Files) Append(e store.AuditEntry, streamID string) error {
	l := line{
		Time:       e.Time.In(jst).Format(time.RFC3339Nano),
		Operator:   e.Operator,
		MgmtClient: e.MgmtClient,
		Action:     e.Action,
		Target:     e.Target,
		TraceID:    e.TraceID,
		StreamID:   streamID,
	}
	if e.Detail != "" {
		if v := jsontext.Value(e.Detail); v.IsValid() {
			l.Detail = v
		} else {
			b, _ := json.Marshal(e.Detail)
			l.Detail = b
		}
	}
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	path := filepath.Join(f.dir, "audit-"+e.Time.In(jst).Format("20060102")+".jsonl")
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return err
	}
	if _, err := fh.Write(append(b, '\n')); err != nil {
		_ = fh.Close()
		return err
	}
	return fh.Close()
}

// Maintain は、前の日までのファイルを圧縮し、保存の日数を過ぎたファイルを消す。
// admin が起動のときと 1 時間ごとに呼ぶ。
func (f *Files) Maintain(now time.Time) error {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return err
	}
	today := now.In(jst).Format("20060102")
	oldest := now.In(jst).AddDate(0, 0, -f.retention).Format("20060102")
	var errs []error
	for _, de := range entries {
		m := fileName.FindStringSubmatch(de.Name())
		if m == nil || de.IsDir() {
			continue
		}
		date, gz := m[1], m[2] != ""
		path := filepath.Join(f.dir, de.Name())
		switch {
		case date < oldest:
			if err := os.Remove(path); err != nil {
				errs = append(errs, err)
			}
		case !gz && date < today:
			info, err := de.Info()
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if now.Sub(info.ModTime()) < compressAfter {
				continue
			}
			if err := compress(path); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// compress は path を path.gz に圧縮し、もとのファイルを消す。途中で失敗したら、もとのファイルを残す。
func compress(path string) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	tmp := path + ".gz.tmp"
	dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(dst)
	zw.Name = filepath.Base(path)
	_, err = io.Copy(zw, src)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, strings.TrimSuffix(tmp, ".tmp"))
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("compress %s: %w", path, err)
	}
	return os.Remove(path)
}

// CheckWritableDir は、ディレクトリを作り（なければ）、書けることを確かめる。
// compose でホストのディレクトリをマウントしたとき、UID 65532 で書けないまま動き出すのを防ぐ（aka-vecgen と同じ）。
func CheckWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("audit directory %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".write-check-*")
	if err != nil {
		return fmt.Errorf("audit directory %s is not writable: %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}
