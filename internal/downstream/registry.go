package downstream

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json/v2"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/certs"
)

// basePath は aka-vecgen・aka-radius の管理API の基底のパス（2.0.0）。登録簿の url はこれで終わる。
// radsec-ca とエッジのパスには接頭辞がない（P2）。
const basePath = "/admin/v2"

// fileFormat は下流の登録簿のファイル（PROVISIONER_DOWNSTREAMS_FILE。設計概要 §8.1）。
// ファイルのパスは登録簿のファイルからの相対で書ける（ディレクトリごとマウントする）。
type fileFormat struct {
	// ClientCert・ClientKey は、全下流に提示する provisioner のクライアント証明書と秘密鍵。
	ClientCert string       `json:"clientCert"`
	ClientKey  string       `json:"clientKey"`
	Vecgen     *entryFormat `json:"vecgen"`
	Radius     *entryFormat `json:"radius"`
	// CA は radsec-ca（省略できる。radsec-ca は aka-provisioner が作る共有のネットワークに入るので、aka-provisioner が先に起動し、
	// CA のサーバー証明書はその時点でない。足すときは SIGHUP。P2）。
	CA *entryFormat `json:"ca"`
	// Edges はエッジの一覧（省略できる）。
	Edges []edgeFormat `json:"edges"`
}

type entryFormat struct {
	// URL は下流の管理API の基底 URL。aka-vecgen・aka-radius は https://<ホスト>:<ポート>/admin/v2、radsec-ca は接頭辞なし。
	URL string `json:"url"`
	// ServerCert は下流のサーバー証明書（自己署名。信頼の起点にする）。
	ServerCert string `json:"serverCert"`
}

type edgeFormat struct {
	// ID はエッジの ID（パスの /edges/{edgeId}。slug の形）。
	ID string `json:"id"`
	// URL はエッジの管理API の基底 URL（接頭辞なし）。
	URL string `json:"url"`
	// ServerFingerprint はエッジのサーバー証明書の SHA-256 フィンガープリント。ホスト名は見ない（P3）。
	ServerFingerprint string `json:"serverFingerprint"`
}

// Timeouts は下流の 1 回の呼び出しのタイムアウト（P4）。
type Timeouts struct {
	// Downstream は aka-vecgen・aka-radius。
	Downstream time.Duration
	// Radsec は radsec-ca とエッジ（CRL の取得や発行に時間がかかる）。
	Radsec time.Duration
}

// Registry は下流の登録簿。ファイルから読み、SIGHUP で読み直す（Reload）。
type Registry struct {
	path     string
	timeouts Timeouts
	log      *slog.Logger

	mu  sync.Mutex // Reload を 1 つずつにする
	cur atomic.Pointer[registrySet]
}

type registrySet struct {
	clients    map[Name]*Client
	names      []Name
	clientCert *x509.Certificate
	loadedAt   time.Time
}

// LoadRegistry は登録簿のファイルを読む。読めない・形が違う・証明書が読めないときはエラーを返す。
func LoadRegistry(path string, timeouts Timeouts, log *slog.Logger) (*Registry, error) {
	r := &Registry{path: path, timeouts: timeouts, log: log}
	return r, r.Reload()
}

// Reload は登録簿のファイルを読み直す。読めない・形が違う・証明書が読めないときは前の登録簿のまま続け、エラーを返す。
// 読み直したら、次の要求から新しいクライアントを使い、前のクライアントの待機中の接続を閉じる。
func (r *Registry) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.build()
	if err != nil {
		return err
	}
	old := r.cur.Swap(s)
	if old != nil {
		for _, c := range old.clients {
			c.closeIdle()
		}
	}
	return nil
}

func (r *Registry) build() (*registrySet, error) {
	f, err := readRegistryFile(r.path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(r.path)
	if f.ClientCert == "" || f.ClientKey == "" {
		return nil, errors.New("clientCert and clientKey are required")
	}
	cert, err := tls.LoadX509KeyPair(resolve(dir, f.ClientCert), resolve(dir, f.ClientKey))
	if err != nil {
		return nil, fmt.Errorf("load client certificate: %w", err)
	}
	s := &registrySet{clients: map[Name]*Client{}, clientCert: cert.Leaf, loadedAt: time.Now()}
	add := func(name Name, opts clientOptions) {
		opts.name, opts.clientCert, opts.timeout, opts.log = name, cert, r.timeouts.Downstream, r.log
		if name.radsec() {
			opts.timeout = r.timeouts.Radsec
		}
		s.clients[name] = newClient(opts)
		s.names = append(s.names, name)
	}
	addEntry := func(name Name, e *entryFormat) error {
		base, err := parseBaseURL(e.URL, name)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if e.ServerCert == "" {
			return fmt.Errorf("%s: serverCert is required", name)
		}
		serverCert, err := readCertificate(resolve(dir, e.ServerCert))
		if err != nil {
			return fmt.Errorf("%s: server certificate: %w", name, err)
		}
		add(name, clientOptions{baseURL: base, serverCert: serverCert})
		return nil
	}
	for _, d := range []struct {
		name Name
		e    *entryFormat
	}{{Vecgen, f.Vecgen}, {Radius, f.Radius}} {
		if d.e == nil {
			return nil, fmt.Errorf("%s: required", d.name)
		}
		if err := addEntry(d.name, d.e); err != nil {
			return nil, err
		}
	}
	if f.CA != nil {
		if err := addEntry(CA, f.CA); err != nil {
			return nil, err
		}
	}
	for i, e := range f.Edges {
		if !ValidEdgeID(e.ID) {
			return nil, fmt.Errorf("edges[%d]: id %q: want %s", i, e.ID, slugPattern)
		}
		name := EdgeName(e.ID)
		if _, dup := s.clients[name]; dup {
			return nil, fmt.Errorf("edges[%d]: duplicate id %q", i, e.ID)
		}
		base, err := parseBaseURL(e.URL, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		fp, err := certs.ParseFingerprint(e.ServerFingerprint)
		if err != nil {
			return nil, fmt.Errorf("%s: serverFingerprint: %w", name, err)
		}
		add(name, clientOptions{baseURL: base, serverFingerprint: fp})
	}
	return s, nil
}

// Client は下流 name のクライアントを返す。登録簿になければ nil。
func (r *Registry) Client(name Name) *Client { return r.cur.Load().clients[name] }

// Names は登録簿の今の下流の名前を返す（vecgen、radius、ca、エッジの順。/status の並び）。
func (r *Registry) Names() []Name { return slices.Clone(r.cur.Load().names) }

// ClientCertificate は provisioner のクライアント証明書を返す。
func (r *Registry) ClientCertificate() *x509.Certificate { return r.cur.Load().clientCert }

// LoadedAt は登録簿を読んだ時刻を返す。
func (r *Registry) LoadedAt() time.Time { return r.cur.Load().loadedAt }

// slugPattern はエッジの ID の形（radsec-ca・エッジの名前と同じ）。
const slugPattern = `^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`

var slugRegexp = regexp.MustCompile(slugPattern)

// ValidEdgeID はエッジの ID の形か。
func ValidEdgeID(id string) bool { return slugRegexp.MatchString(id) }

// readRegistryFile は登録簿のファイルを読む。知らない項目は綴りの誤りとして拒否する。
func readRegistryFile(path string) (*fileFormat, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read downstreams file: %w", err)
	}
	// Unmarshal は 1 つの値の後ろに続くデータも誤りにする。
	var f fileFormat
	if err := json.Unmarshal(data, &f, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("parse downstreams file: %w", err)
	}
	return &f, nil
}

// parseBaseURL は下流の基底 URL を確かめる。https で、ホストがあり、パスが aka-vecgen・aka-radius は /admin/v2、
// radsec-ca とエッジは接頭辞なしであること。
func parseBaseURL(v string, name Name) (*url.URL, error) {
	want := basePath
	if name.radsec() {
		want = ""
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("url %q: want https://<host>:<port>%s", v, want)
	}
	if strings.TrimSuffix(u.Path, "/") != want || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("url %q: path must be %q", v, want)
	}
	u.Path = want
	return u, nil
}

// readCertificate は PEM の最初の証明書を読む。
func readCertificate(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for {
		var b *pem.Block
		b, data = pem.Decode(data)
		if b == nil {
			return nil, fmt.Errorf("no PEM certificate in %s", path)
		}
		if b.Type == "CERTIFICATE" {
			return x509.ParseCertificate(b.Bytes)
		}
	}
}

// resolve は登録簿のファイルからの相対パスを、登録簿のディレクトリを基準に解決する。
func resolve(dir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}
