package server

import (
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHTTPErrorLog(t *testing.T) {
	for _, tc := range []struct {
		line, level string
	}{
		{"http: TLS handshake error from 127.0.0.1:51234: EOF", "DEBUG"},
		{"http: TLS handshake error from [::1]:51234: EOF", "DEBUG"},
		// 外からの接続、EOF 以外のエラーは WARN のまま。
		{"http: TLS handshake error from 172.18.0.1:51234: EOF", "WARN"},
		{"http: TLS handshake error from 127.0.0.1:51234: remote error: tls: bad certificate", "WARN"},
		{"http: TLS handshake error from 127.0.0.1:51234: EOF extra", "WARN"},
		{"http: panic serving 10.0.0.1:1234: boom", "WARN"},
	} {
		var buf bytes.Buffer
		l := newHTTPErrorLog(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		l.Print(tc.line)
		out := buf.String()
		if !strings.Contains(out, `"level":"`+tc.level+`"`) || !strings.Contains(out, `"msg":"`+tc.line+`"`) {
			t.Errorf("%q: got %s, want level %s", tc.line, out, tc.level)
		}
	}
}

func TestHTTPErrorLog_HealthcheckConnection(t *testing.T) {
	// 実際の TLS サーバーに healthcheck と同じ接続（TCP で接続してすぐ切る）をしても、WARN は出ない。
	var buf syncBuffer
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Config.ErrorLog = newHTTPErrorLog(slog.NewJSONHandler(&buf, nil)) // INFO 以上だけ出す（既定の設定と同じ）
	srv.StartTLS()
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	time.Sleep(200 * time.Millisecond) // サーバーがハンドシェイクの失敗を記録するのを待つ
	if out := buf.String(); out != "" {
		t.Errorf("logged at INFO or above: %s", out)
	}
}

// syncBuffer は、サーバーのゴルーチンから書き込まれるログを集める。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
