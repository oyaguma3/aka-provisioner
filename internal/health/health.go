// Package health はループバックの healthcheck の口を提供する（設計概要 §9。aka-radius の決定事項 13 と同じ作り）。
//
// プロセスがループバックで HTTP の GET /healthz を待ち受け、専用の Valkey に届くか（PING）を返す。
// コマンド aka-provisioner healthcheck がこの口を叩き、compose の healthcheck はそのコマンドを使う
// （API は mTLS なので、管理クライアントの証明書がないと確かめられない）。
package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	// pingTimeout は Valkey の PING の上限。
	pingTimeout = time.Second
	// checkTimeout はコマンド healthcheck が口に問い合わせる上限（compose の healthcheck の timeout より短くする）。
	checkTimeout = 3 * time.Second
	path         = "/healthz"
)

// Server は healthcheck の口。
type Server struct {
	srv *http.Server
	ln  net.Listener
}

// Start は addr で healthcheck の口を待ち受ける。ping は Valkey に届くかを確かめる。
func Start(addr string, ping func(context.Context) error, log *slog.Logger) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen healthcheck %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), pingTimeout)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		if err := ping(ctx); err != nil {
			log.Warn("healthcheck failed", "event_id", "HEALTH_VALKEY_ERR", "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"status":"unavailable","valkey":"unreachable"}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok","valkey":"ok"}`)
	})
	s := &Server{
		srv: &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug)},
		ln:  ln,
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("healthcheck server error", "event_id", "HEALTH_SERVE_ERR", "error", err)
		}
	}()
	return s, nil
}

// Addr は待ち受けているアドレスを返す。
func (s *Server) Addr() string {
	return s.ln.Addr().String()
}

// Close は口を閉じる。
func (s *Server) Close() error {
	return s.srv.Close()
}

// Check は addr の healthcheck の口に問い合わせ、200 でなければ誤りを返す（コマンド healthcheck）。
// 待ち受けのホストが空か 0.0.0.0 などなら、ループバックに問い合わせる。
func Check(ctx context.Context, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("healthcheck address %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+path, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: %s %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
