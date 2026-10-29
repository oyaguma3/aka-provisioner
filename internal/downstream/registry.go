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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// basePath は下流の管理API の基底のパス（aka-vecgen・aka-radius の 2.0.0）。登録簿の url はこれで終わる。
const basePath = "/admin/v2"

// fileFormat は下流の登録簿のファイル（PROVISIONER_DOWNSTREAMS_FILE。設計概要 §8.1）。
// ファイルのパスは登録簿のファイルからの相対で書ける（ディレクトリごとマウントする）。
// 段階 3-1 では vecgen と radius だけを受け付ける（ca・edges は段階 4-2 で足す）。
type fileFormat struct {
	// ClientCert・ClientKey は、全下流に提示する provisioner のクライアント証明書と秘密鍵。
	ClientCert string       `json:"clientCert"`
	ClientKey  string       `json:"clientKey"`
	Vecgen     *entryFormat `json:"vecgen"`
	Radius     *entryFormat `json:"radius"`
}

type entryFormat struct {
	// URL は下流の管理API の基底 URL（https://<ホスト>:<ポート>/admin/v2）。
	URL string `json:"url"`
	// ServerCert は下流のサーバー証明書（自己署名。信頼の起点にする）。
	ServerCert string `json:"serverCert"`
}

// Registry は下流の登録簿。ファイルから読み、SIGHUP で読み直す（Reload）。
type Registry struct {
	path    string
	timeout time.Duration
	log     *slog.Logger

	mu  sync.Mutex // Reload を 1 つずつにする
	cur atomic.Pointer[registrySet]
}

type registrySet struct {
	clients    map[Name]*Client
	clientCert *x509.Certificate
	loadedAt   time.Time
}

// LoadRegistry は登録簿のファイルを読む。読めない・形が違う・証明書が読めないときはエラーを返す。
func LoadRegistry(path string, timeout time.Duration, log *slog.Logger) (*Registry, error) {
	r := &Registry{path: path, timeout: timeout, log: log}
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
	entries := map[Name]*entryFormat{Vecgen: f.Vecgen, Radius: f.Radius}
	for _, name := range Names {
		e := entries[name]
		if e == nil {
			return nil, fmt.Errorf("%s: required", name)
		}
		base, err := parseBaseURL(e.URL)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if e.ServerCert == "" {
			return nil, fmt.Errorf("%s: serverCert is required", name)
		}
		serverCert, err := readCertificate(resolve(dir, e.ServerCert))
		if err != nil {
			return nil, fmt.Errorf("%s: server certificate: %w", name, err)
		}
		s.clients[name] = newClient(clientOptions{
			name: name, baseURL: base, clientCert: cert, serverCert: serverCert, timeout: r.timeout, log: r.log,
		})
	}
	return s, nil
}

// Client は下流 name のクライアントを返す。
func (r *Registry) Client(name Name) *Client { return r.cur.Load().clients[name] }

// ClientCertificate は provisioner のクライアント証明書を返す。
func (r *Registry) ClientCertificate() *x509.Certificate { return r.cur.Load().clientCert }

// LoadedAt は登録簿を読んだ時刻を返す。
func (r *Registry) LoadedAt() time.Time { return r.cur.Load().loadedAt }

// readRegistryFile は登録簿のファイルを読む。知らない項目は綴りの誤り（段階 4-2 の ca・edges を含む）として拒否する。
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

// parseBaseURL は下流の基底 URL を確かめる。https で、ホストがあり、パスが /admin/v2 であること。
func parseBaseURL(v string) (*url.URL, error) {
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("url %q: want https://<host>:<port>%s", v, basePath)
	}
	if strings.TrimSuffix(u.Path, "/") != basePath || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("url %q: path must be %s", v, basePath)
	}
	u.Path = basePath
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
