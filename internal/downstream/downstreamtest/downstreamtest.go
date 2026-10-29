// Package downstreamtest は、下流の管理API の代わりになる mTLS のテスト用サーバーと、下流の登録簿のファイルを作る（テスト専用）。
package downstreamtest

import (
	"crypto/tls"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/certs"
)

// Discard はログを捨てる slog.Logger。
var Discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// Server は下流の代わりのサーバー。クライアント証明書は求めるが、どれでも受け付ける。
type Server struct {
	*httptest.Server
	// BaseURL は下流の管理API の基底 URL（/admin/v2）。
	BaseURL string
	// ServerCertPEM はサーバー証明書（登録簿の serverCert に書くもの）。
	ServerCertPEM []byte
}

// NewServer は h で応える下流の代わりのサーバーを起動する。
func NewServer(t *testing.T, h http.HandlerFunc) *Server {
	t.Helper()
	serverCertPEM, serverKeyPEM, err := certs.SelfSigned("downstream", []string{"127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = slog.NewLogLogger(Discard.Handler(), slog.LevelWarn)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequestClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("client certificate required")
			}
			return nil
		},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &Server{Server: srv, BaseURL: srv.URL + "/admin/v2", ServerCertPEM: serverCertPEM}
}

// Registry は WriteRegistry で書いた登録簿。
type Registry struct {
	// Path は登録簿のファイルのパス。
	Path string
	// Dir は登録簿と証明書を置いたディレクトリ。
	Dir string
	// ClientFP は provisioner のクライアント証明書のフィンガープリント。
	ClientFP string
}

// WriteRegistry は、vecgen と radius をそれぞれの基底 URL とサーバー証明書で指す登録簿のファイルを、
// provisioner のクライアント証明書とともに一時のディレクトリに書く。
func WriteRegistry(t *testing.T, vecgenURL string, vecgenCert []byte, radiusURL string, radiusCert []byte) Registry {
	t.Helper()
	dir := t.TempDir()
	clientCertPEM, clientKeyPEM, err := certs.SelfSignedClient("provisioner", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	clientCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	WriteFile(t, filepath.Join(dir, "client.pem"), clientCertPEM)
	WriteFile(t, filepath.Join(dir, "client.key"), clientKeyPEM)
	WriteFile(t, filepath.Join(dir, "vecgen-admin.pem"), vecgenCert)
	WriteFile(t, filepath.Join(dir, "radius-admin.pem"), radiusCert)
	reg := map[string]any{
		"clientCert": "client.pem",
		"clientKey":  "client.key",
		"vecgen":     map[string]string{"url": vecgenURL, "serverCert": "vecgen-admin.pem"},
		"radius":     map[string]string{"url": radiusURL, "serverCert": "radius-admin.pem"},
	}
	b, err := json.Marshal(reg, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "downstreams.json")
	WriteFile(t, path, b)
	return Registry{Path: path, Dir: dir, ClientFP: certs.Fingerprint(clientCert.Leaf)}
}

// WriteFile はテスト用のファイルを書く。
func WriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ClosedAddr は、接続を受け付けないループバックのアドレスを返す。
func ClosedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// WriteJSON は JSON の応答を書く。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.MarshalWrite(w, v)
}

// WriteProblem は ProblemDetails の応答を書く。
func WriteProblem(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	io.WriteString(w, body)
}
