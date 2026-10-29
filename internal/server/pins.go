package server

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/certs"
)

var (
	clientNamePattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Clients は管理クライアントの一覧。フィンガープリント（SHA-256、16進小文字）から識別名を引く。
type Clients map[string]string

// ParseClients はピンのファイルの中身を解釈する。1 行に「識別名=フィンガープリント」を書く（aka-vecgen・aka-radius と同じ形）。
// # で始まる行と空行は無視する。フィンガープリントは SHA-256 の 16 進で、大文字やコロン区切り（openssl の出力形式）も受け付ける。
// 同じ識別名を 2 行以上書ける（証明書の入れ替えの間は新旧の両方を受ける）。同じフィンガープリントが 2 回出たら誤り。
func ParseClients(v string) (Clients, error) {
	clients := Clients{}
	for i, line := range strings.Split(v, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, fp, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || !clientNamePattern.MatchString(name) {
			return nil, fmt.Errorf("line %d: want <name>=<sha256 fingerprint>", i+1)
		}
		fp = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(fp), ":", ""))
		if !fingerprintPattern.MatchString(fp) {
			return nil, fmt.Errorf("line %d: bad fingerprint for %q: want 64 hex digits", i+1, name)
		}
		if other, dup := clients[fp]; dup {
			return nil, fmt.Errorf("line %d: fingerprint of %q is already used by %q", i+1, name, other)
		}
		clients[fp] = name
	}
	return clients, nil
}

// LoadClients はピンのファイルを読む。管理クライアントが 1 つもなければ誤りにする（だれも API を使えなくなるため）。
func LoadClients(path string) (Clients, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := ParseClients(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(c) == 0 {
		return nil, fmt.Errorf("%s: at least one admin client is required", path)
	}
	return c, nil
}

// Names は識別名を重ねずに辞書順で返す。
func (c Clients) Names() []string {
	var names []string
	for _, n := range c {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names
}

// Pins は管理クライアントのピン。ファイルから読み、SIGHUP で読み直す（Reload）。
type Pins struct {
	path string
	mu   sync.Mutex // Reload を 1 つずつにする
	cur  atomic.Pointer[pinSet]
}

type pinSet struct {
	clients  Clients
	loadedAt time.Time
}

// LoadPins は path のピンのファイルを読む。読めない・形が違う・1 つもないときはエラーを返す。
func LoadPins(path string) (*Pins, error) {
	p := &Pins{path: path}
	return p, p.Reload()
}

// NewPins は決まった管理クライアントの Pins を作る（試験用。Reload はできない）。
func NewPins(c Clients) *Pins {
	p := &Pins{}
	p.cur.Store(&pinSet{clients: c, loadedAt: time.Now()})
	return p
}

// Reload はピンのファイルを読み直す。読めない・形が違う・1 つもないときは前のピンのまま続け、エラーを返す。
// 読み直した後のハンドシェイクから新しいピンで照合する（張ってある接続は切らない）。
func (p *Pins) Reload() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.path == "" {
		return errors.New("admin clients file is not set")
	}
	c, err := LoadClients(p.path)
	if err != nil {
		return err
	}
	p.cur.Store(&pinSet{clients: c, loadedAt: time.Now()})
	return nil
}

// Names は管理クライアントの識別名と、ピンを読んだ時刻を返す（/status）。
func (p *Pins) Names() ([]string, time.Time) {
	s := p.cur.Load()
	return s.clients.Names(), s.loadedAt
}

// Fingerprints はピンのフィンガープリントの数を返す（ログ用）。
func (p *Pins) Fingerprints() int {
	return len(p.cur.Load().clients)
}

// NameFromRequest はリクエストのクライアント証明書に対応する識別名を返す。
// TLS でない、または証明書がない・登録されていない場合は空文字を返す。
func (p *Pins) NameFromRequest(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	return p.cur.Load().clients[certs.Fingerprint(r.TLS.PeerCertificates[0])]
}

// verifier は TLS ハンドシェイクでクライアント証明書をピンと照合する。
type verifier struct {
	pins *Pins
	log  *slog.Logger
	now  func() time.Time
}

// verify は証明書がない、登録されていない、有効期間外のいずれかなら拒否する（HTTP の応答は返さない）。
// srcIP は拒否したときのログに使う送信元IP（不明なら空文字）。
func (v *verifier) verify(cs tls.ConnectionState, srcIP string) error {
	if len(cs.PeerCertificates) == 0 {
		v.reject("no client certificate", "", srcIP)
		return errors.New("client certificate required")
	}
	cert := cs.PeerCertificates[0]
	fp := certs.Fingerprint(cert)
	if _, ok := v.pins.cur.Load().clients[fp]; !ok {
		v.reject("not configured", fp, srcIP)
		return errors.New("client certificate is not a configured admin client")
	}
	if t := v.now(); t.Before(cert.NotBefore) || t.After(cert.NotAfter) {
		v.reject("outside validity period", fp, srcIP)
		return errors.New("client certificate is outside its validity period")
	}
	return nil
}

// reject は拒否した接続をログに残す（未登録の証明書を登録するときにフィンガープリントを確かめられるようにする）。
func (v *verifier) reject(reason, fp, srcIP string) {
	v.log.Warn("admin client certificate rejected",
		"event_id", "ADMIN_CLIENT_REJECTED",
		"reason", reason,
		"fingerprint", fp,
		"src_ip", srcIP,
	)
}

// hostOf はアドレスからホスト部分を取り出す。
func hostOf(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}
