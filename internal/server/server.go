// Package server は provisioner の API の HTTPS リスナー（mTLS）を起動する。
//
// 管理クライアント（BFF など）は、クライアント証明書の SHA-256 フィンガープリントを設定（PROVISIONER_ADMIN_CLIENTS）と
// 照合して認める（CA は使わない。下流の 2 つと同じ）。証明書なし・未登録・有効期間外の接続は TLS ハンドシェイクで拒否する。
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/certs"
)

const shutdownTimeout = 10 * time.Second

// Options はリスナーの設定。
type Options struct {
	// Addr は待ち受けアドレス。
	Addr string
	// GetCertificate はサーバー証明書を返す。
	GetCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	// AdminClients は、管理クライアントの証明書のフィンガープリント（SHA-256、16進小文字）から識別名を引く。
	AdminClients map[string]string
	Handler      http.Handler
	Log          *slog.Logger
}

// Run は HTTPS リスナーを起動し、ctx が終了するかリスナーが失敗するまで待つ。
func Run(ctx context.Context, opts Options) error {
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", opts.Addr, err)
	}
	return serve(ctx, ln, opts)
}

func serve(ctx context.Context, ln net.Listener, opts Options) error {
	srv := &http.Server{
		Handler:           opts.Handler,
		TLSConfig:         tlsConfig(opts),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// 加入者の作成などは下流を何度か呼ぶので、書き込みの時間は長めにとる。
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
		ErrorLog:     newHTTPErrorLog(opts.Log.Handler()),
	}

	errc := make(chan error, 1)
	go func() {
		// 証明書は TLSConfig.GetCertificate から取るので、ファイル名は渡さない。
		errc <- srv.ServeTLS(ln, "", "")
	}()
	opts.Log.Info("listening", "addr", ln.Addr().String())

	select {
	case <-ctx.Done():
		opts.Log.Info("shutting down")
	case err := <-errc:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		opts.Log.Warn("shutdown", "error", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// tlsConfig は、管理クライアントの証明書をフィンガープリントで照合する TLS 設定を返す。
func tlsConfig(opts Options) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		ClientAuth:     tls.RequireAnyClientCert,
		GetCertificate: opts.GetCertificate,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("client certificate required")
			}
			cert := cs.PeerCertificates[0]
			fp := certs.Fingerprint(cert)
			if _, ok := opts.AdminClients[fp]; !ok {
				opts.Log.Warn("admin client certificate rejected", "reason", "not configured", "fingerprint", fp)
				return errors.New("client certificate is not a configured admin client")
			}
			if now := time.Now(); now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
				opts.Log.Warn("admin client certificate rejected", "reason", "outside validity period", "fingerprint", fp)
				return errors.New("client certificate is outside its validity period")
			}
			return nil
		},
	}
}

// AdminClientName は、リクエストを送ってきた管理クライアントの識別名を返す。mTLS でなければ空文字列。
func AdminClientName(r *http.Request, clients map[string]string) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	return clients[certs.Fingerprint(r.TLS.PeerCertificates[0])]
}
