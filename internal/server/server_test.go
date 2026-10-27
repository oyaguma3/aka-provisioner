package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/certs"
)

// lockedBuffer は、サーバーのゴルーチンが書くログを、テストから安全に読むためのバッファ。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func clientCert(t *testing.T, name string, validFor time.Duration) tls.Certificate {
	t.Helper()
	certPEM, keyPEM, err := certs.SelfSignedClient(name, validFor)
	if err != nil {
		t.Fatal(err)
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestServeMTLS(t *testing.T) {
	serverPEM, serverKey, err := certs.SelfSigned("aka-provisioner", []string{"127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := tls.X509KeyPair(serverPEM, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	bff := clientCert(t, "bff", time.Hour)
	stranger := clientCert(t, "stranger", time.Hour)
	expired := clientCert(t, "expired", -time.Hour) // NotAfter が過去になる
	clients := map[string]string{
		certs.Fingerprint(bff.Leaf):     "bff-01",
		certs.Fingerprint(expired.Leaf): "expired",
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, Options{
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &serverCert, nil },
			AdminClients:   clients,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, r.Proto+" "+AdminClientName(r, clients))
			}),
			Log: slog.New(slog.NewJSONHandler(&logs, nil)),
		})
	}()

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(serverPEM)
	get := func(certs ...tls.Certificate) (string, error) {
		client := &http.Client{Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: pool, Certificates: certs},
			ForceAttemptHTTP2: true,
		}}
		resp, err := client.Get("https://" + ln.Addr().String() + "/")
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return string(body), err
	}

	// 登録済みの証明書は通り、識別名が分かる。HTTP/2 で通信できる。
	if body, err := get(bff); err != nil || body != "HTTP/2.0 bff-01" {
		t.Errorf("configured client: %q, %v", body, err)
	}
	// 未登録、有効期間外、証明書なしは拒否する。
	for name, c := range map[string][]tls.Certificate{"stranger": {stranger}, "expired": {expired}, "none": nil} {
		if body, err := get(c...); err == nil {
			t.Errorf("%s: accepted (%q)", name, body)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after cancel")
	}
	if !strings.Contains(logs.String(), `"reason":"not configured"`) || !strings.Contains(logs.String(), `"reason":"outside validity period"`) {
		t.Errorf("rejection logs: %s", logs.String())
	}
}

func TestAdminClientNameWithoutTLS(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "http://example/", nil)
	if got := AdminClientName(r, map[string]string{"x": "y"}); got != "" {
		t.Errorf("got %q", got)
	}
}
