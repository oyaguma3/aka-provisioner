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
	"os"
	"path/filepath"
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
	pins := NewPins(Clients{
		certs.Fingerprint(bff.Leaf):     "bff-01",
		certs.Fingerprint(expired.Leaf): "expired",
	})

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
			Pins:           pins,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, r.Proto+" "+pins.NameFromRequest(r))
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
	if !strings.Contains(logs.String(), `"reason":"not configured"`) || !strings.Contains(logs.String(), `"reason":"outside validity period"`) ||
		!strings.Contains(logs.String(), `"reason":"no client certificate"`) || !strings.Contains(logs.String(), `"event_id":"ADMIN_CLIENT_REJECTED"`) ||
		!strings.Contains(logs.String(), `"fingerprint":"`+certs.Fingerprint(stranger.Leaf)+`"`) || !strings.Contains(logs.String(), `"src_ip":"127.0.0.1"`) {
		t.Errorf("rejection logs: %s", logs.String())
	}
}

func TestNameFromRequestWithoutTLS(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "http://example/", nil)
	if got := NewPins(Clients{"x": "y"}).NameFromRequest(r); got != "" {
		t.Errorf("got %q", got)
	}
}

const fp1 = "7a7a91920ccfb51760f70d6dba4ceb8ba4386eec2728955983ad04c42e273043"
const fp2 = "0b7a91920ccfb51760f70d6dba4ceb8ba4386eec2728955983ad04c42e273043"

func TestParseClients(t *testing.T) {
	c, err := ParseClients("# aka-ossbss\nossbss=" + strings.ToUpper(fp1[:2]) + ":" + fp1[2:] + "\n\nossbss=" + fp2 + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if c[fp1] != "ossbss" || c[fp2] != "ossbss" || len(c) != 2 || strings.Join(c.Names(), ",") != "ossbss" {
		t.Errorf("clients = %v", c)
	}
	for name, v := range map[string]string{
		"区切りがない":      "ossbss " + fp1,
		"名前の形":        "a b=" + fp1,
		"フィンガープリントの形": "ossbss=xyz",
		"同じフィンガープリント": "a=" + fp1 + "\nb=" + fp1,
	} {
		if _, err := ParseClients(v); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestPinsReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin-clients")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	// 1 つもないファイルでは起動しない。
	if _, err := LoadPins(path); err == nil {
		t.Fatal("empty file: want error")
	}
	if err := os.WriteFile(path, []byte("ossbss="+fp1+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPins(path)
	if err != nil {
		t.Fatal(err)
	}
	_, first := p.Names()
	// 形の違うファイルでは前のピンのまま。
	if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Reload(); err == nil {
		t.Error("broken file: want error")
	}
	if names, at := p.Names(); strings.Join(names, ",") != "ossbss" || !at.Equal(first) {
		t.Errorf("after failed reload: %v %v", names, at)
	}
	// 新旧 2 つを書いて読み直せる。
	if err := os.WriteFile(path, []byte("ossbss="+fp1+"\nossbss="+fp2+"\nother="+strings.Repeat("c", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Reload(); err != nil {
		t.Fatal(err)
	}
	if names, _ := p.Names(); strings.Join(names, ",") != "ossbss,other" || p.Fingerprints() != 3 {
		t.Errorf("after reload: %v %d", names, p.Fingerprints())
	}
}
