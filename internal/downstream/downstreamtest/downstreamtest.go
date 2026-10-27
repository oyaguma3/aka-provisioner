// Package downstreamtest は、下流の API の代わりになる mTLS のテスト用サーバーを提供する（テスト専用）。
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
	"github.com/oyaguma3/aka-provisioner/internal/downstream"
)

// Discard はテストで使う、何も出力しないロガー。
var Discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// Server は mTLS の下流の代わりのサーバーと、それにつなぐクライアントの設定。
type Server struct {
	*httptest.Server
	// Options はこのサーバーにつなぐクライアントの設定（Name は呼び出し側で設定する）。
	Options downstream.Options
	// ClientFP はクライアントが提示するはずの証明書のフィンガープリント。
	ClientFP string
	// Dir は証明書のファイルを置いたディレクトリ。
	Dir string
}

// NewServer はハンドラー h を mTLS で提供するサーバーを立てる。
// 下流と同じく、クライアント証明書は RequestClientCert で求めて VerifyConnection で確かめる（Server.TLS で差し替えられる）。
// クライアント証明書は証明書と秘密鍵を 1 つの PEM にまとめた形（gen-client-cert の標準出力と同じ）で渡す。
func NewServer(t *testing.T, h http.HandlerFunc) *Server {
	t.Helper()
	dir := t.TempDir()

	serverCertPEM, serverKeyPEM, err := certs.SelfSigned("downstream", []string{"127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	clientCertPEM, clientKeyPEM, err := certs.SelfSignedClient("provisioner", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	clientCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
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

	WriteFile(t, filepath.Join(dir, "client.pem"), append(clientCertPEM, clientKeyPEM...))
	WriteFile(t, filepath.Join(dir, "server.pem"), serverCertPEM)
	return &Server{
		Server: srv,
		Options: downstream.Options{
			BaseURL:        srv.URL + "/admin/v1",
			ClientCertFile: filepath.Join(dir, "client.pem"),
			ServerCertFile: filepath.Join(dir, "server.pem"),
			Timeout:        2 * time.Second,
			Log:            Discard,
		},
		ClientFP: certs.Fingerprint(clientCert.Leaf),
		Dir:      dir,
	}
}

// WriteFile はファイルを書く。
func WriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ClosedAddr は、待ち受けていないことが確かなアドレスを返す。
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
