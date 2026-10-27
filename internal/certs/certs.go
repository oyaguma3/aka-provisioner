// Package certs は provisioner の API のサーバー証明書と、下流に提示するクライアント証明書を扱う。
// 自己署名証明書の生成と、ファイルからの読み込み（更新されたら読み直す）を行う。
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// selfSignedValidFor は自動生成する自己署名サーバー証明書の有効期間。
	selfSignedValidFor = 10 * 365 * 24 * time.Hour
	// checkInterval は証明書ファイルの更新を確かめる間隔。
	checkInterval = time.Minute
)

// Fingerprint は証明書（DER）の SHA-256 を 16進小文字で返す。
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// SelfSigned はサーバー認証用の ECDSA P-256 の鍵と自己署名証明書を生成し、PEM で返す。
// hosts は SAN で、IP アドレスとして解釈できるものは IP、それ以外は DNS 名として入れる。
func SelfSigned(commonName string, hosts []string, validFor time.Duration) (certPEM, keyPEM []byte, err error) {
	return selfSigned(commonName, hosts, validFor, x509.ExtKeyUsageServerAuth)
}

// SelfSignedClient はクライアント認証用の ECDSA P-256 の鍵と自己署名証明書を生成し、PEM で返す。
// 下流（provisioning-api と aka-vecgen の管理API）に提示するクライアント証明書に使う（下流はフィンガープリントで照合する）。
func SelfSignedClient(commonName string, validFor time.Duration) (certPEM, keyPEM []byte, err error) {
	return selfSigned(commonName, nil, validFor, x509.ExtKeyUsageClientAuth)
}

func selfSigned(commonName string, hosts []string, validFor time.Duration, usage x509.ExtKeyUsage) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}

	notBefore := time.Now().Add(-5 * time.Minute)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(validFor),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{usage},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// EnsureFiles は、証明書と秘密鍵のファイルがどちらも存在しなければ自己署名を生成して保存する。
// 生成したら true を返す。片方だけ存在する場合は、持ち込みの途中である可能性があるのでエラーにする。
func EnsureFiles(certPath, keyPath string, hosts []string) (bool, error) {
	certExists, err := exists(certPath)
	if err != nil {
		return false, err
	}
	keyExists, err := exists(keyPath)
	if err != nil {
		return false, err
	}
	switch {
	case certExists && keyExists:
		return false, nil
	case certExists:
		return false, fmt.Errorf("%s exists but %s does not", certPath, keyPath)
	case keyExists:
		return false, fmt.Errorf("%s exists but %s does not", keyPath, certPath)
	}

	certPEM, keyPEM, err := SelfSigned("aka-provisioner", hosts, selfSignedValidFor)
	if err != nil {
		return false, fmt.Errorf("generate self-signed server certificate: %w", err)
	}
	if err := writeNew(keyPath, keyPEM, 0o600); err != nil {
		return false, err
	}
	if err := writeNew(certPath, certPEM, 0o644); err != nil {
		os.Remove(keyPath)
		return false, err
	}
	return true, nil
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// writeNew はファイルを新規作成して書き込む。親ディレクトリがなければ作る。既存のファイルは上書きしない。
func writeNew(path string, data []byte, perm fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// File はファイルから読み込んだサーバー証明書。
// 持ち込みの証明書（tailscale cert などで定期的に更新されるもの）に対応するため、
// ファイルの内容が変わっていたら読み直す。読み直しに失敗したら、それまでの証明書を使い続ける。
type File struct {
	certPath, keyPath string
	log               *slog.Logger

	cert atomic.Pointer[tls.Certificate]

	mu        sync.Mutex
	checkedAt time.Time
	// sum は読み込んだ証明書と秘密鍵の内容のハッシュ。更新時刻は粒度が粗く、
	// 短時間の書き換えを見逃すことがあるので、内容で比べる。
	sum [sha256.Size]byte
}

// LoadFile は証明書と秘密鍵のファイルを読み込む。
func LoadFile(certPath, keyPath string, log *slog.Logger) (*File, error) {
	f := &File{certPath: certPath, keyPath: keyPath, log: log}
	cert, sum, err := f.load()
	if err != nil {
		return nil, err
	}
	f.cert.Store(cert)
	f.sum = sum
	f.checkedAt = time.Now()
	return f, nil
}

// Leaf は現在の証明書を返す。
func (f *File) Leaf() *x509.Certificate { return f.cert.Load().Leaf }

// GetCertificate は tls.Config.GetCertificate に渡す。
func (f *File) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	f.mu.Lock()
	if time.Since(f.checkedAt) >= checkInterval {
		f.checkedAt = time.Now()
		f.reloadIfChanged()
	}
	f.mu.Unlock()
	return f.cert.Load(), nil
}

// reloadIfChanged はファイルの内容が変わっていたら読み直す。f.mu を保持して呼ぶ。
func (f *File) reloadIfChanged() {
	cert, sum, err := f.load()
	if err != nil {
		// 証明書と秘密鍵の片方だけが書き換わった途中の可能性がある。次の確認で再試行する。
		f.log.Warn("server certificate reload failed; keeping the current one", "error", err)
		return
	}
	if sum == f.sum {
		return
	}
	f.cert.Store(cert)
	f.sum = sum
	f.log.Info("reloaded server certificate",
		"fingerprint", Fingerprint(cert.Leaf), "not_after", cert.Leaf.NotAfter)
}

func (f *File) load() (*tls.Certificate, [sha256.Size]byte, error) {
	certPEM, err := os.ReadFile(f.certPath)
	if err != nil {
		return nil, [sha256.Size]byte{}, err
	}
	keyPEM, err := os.ReadFile(f.keyPath)
	if err != nil {
		return nil, [sha256.Size]byte{}, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf("load server certificate: %w", err)
	}
	if time.Now().After(cert.Leaf.NotAfter) {
		f.log.Warn("server certificate has expired", "not_after", cert.Leaf.NotAfter)
	}
	h := sha256.New()
	h.Write(certPEM)
	h.Write(keyPEM)
	return &cert, [sha256.Size]byte(h.Sum(nil)), nil
}
