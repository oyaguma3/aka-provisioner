// Package certs は provisioner の API のサーバー証明書と、下流に提示するクライアント証明書を扱う。
// 自己署名証明書の生成と、ファイルからの読み込み（SIGHUP で読み直す）を行う。
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

// File はファイルから読み込んだサーバー証明書。SIGHUP で読み直す（Reload。設計概要 §8.1）。
// 読み直しに失敗したら、それまでの証明書を使い続ける。
type File struct {
	certPath, keyPath string
	log               *slog.Logger

	mu   sync.Mutex // Reload を 1 つずつにする
	cert atomic.Pointer[tls.Certificate]
}

// LoadFile は証明書と秘密鍵のファイルを読み込む。
func LoadFile(certPath, keyPath string, log *slog.Logger) (*File, error) {
	f := &File{certPath: certPath, keyPath: keyPath, log: log}
	cert, err := f.load()
	if err != nil {
		return nil, err
	}
	f.cert.Store(cert)
	return f, nil
}

// Leaf は今の証明書を返す。
func (f *File) Leaf() *x509.Certificate { return f.cert.Load().Leaf }

// GetCertificate は tls.Config.GetCertificate に使う。読み直した証明書を次の接続から出す。
func (f *File) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return f.cert.Load(), nil
}

// Reload は証明書と秘密鍵のファイルを読み直す。読めない・組が合わないときは前の証明書のまま続け、エラーを返す。
func (f *File) Reload() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cert, err := f.load()
	if err != nil {
		return err
	}
	f.cert.Store(cert)
	return nil
}

func (f *File) load() (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(f.certPath, f.keyPath)
	if err != nil {
		return nil, fmt.Errorf("load server certificate: %w", err)
	}
	if time.Now().After(cert.Leaf.NotAfter) {
		f.log.Warn("server certificate has expired", "not_after", cert.Leaf.NotAfter)
	}
	return &cert, nil
}
