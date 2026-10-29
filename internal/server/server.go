// Package server は provisioner の API の HTTPS リスナー（mTLS）を起動する。
//
// 管理クライアント（aka-ossbss）は、クライアント証明書の SHA-256 フィンガープリントをピンのファイル
// （PROVISIONER_ADMIN_CLIENTS_FILE）と照合して認める（CA は使わない。下流の 2 つと同じ）。
// 証明書なし・未登録・有効期間外の接続は TLS ハンドシェイクで拒否し、ADMIN_CLIENT_REJECTED を出す。
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
)

const shutdownTimeout = 10 * time.Second

// Options はリスナーの設定。
type Options struct {
	// Addr は待ち受けアドレス。
	Addr string
	// GetCertificate はサーバー証明書を返す（SIGHUP で読み直した証明書を次の接続から出す）。
	GetCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	// Pins は管理クライアントのピン。
	Pins *Pins
	// Handler は API のハンドラー。
	Handler http.Handler
	Log     *slog.Logger
	// Now は現在時刻を返す（試験用）。nil なら time.Now。
	Now func() time.Time
}

// Run は ctx が終わるまで待ち受ける。ctx が終わったら、処理中の要求を待ってから閉じる。
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
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          newHTTPErrorLog(opts.Log.Handler()),
	}

	errc := make(chan error, 1)
	go func() {
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

// tlsConfig はリスナーの TLS 設定を返す（TLS 1.2 以上、mTLS 必須）。
// 証明書のない接続もログに残すため、ClientAuth は RequestClientCert にして VerifyConnection で断る
// （RequireAnyClientCert では VerifyConnection の前に断られる）。提示された証明書の秘密鍵の所持は、ClientAuth によらず検証される。
// 拒否のログに送信元IPを残すため、接続ごとに VerifyConnection を差し替えた設定を使う。
func tlsConfig(opts Options) *tls.Config {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	v := &verifier{pins: opts.Pins, log: opts.Log, now: now}
	base := &tls.Config{
		MinVersion: tls.VersionTLS12,
		// GetConfigForClient が返す設定には http.Server が足す ALPN が入らないので、ここで HTTP/2 を入れておく。
		NextProtos:       []string{"h2", "http/1.1"},
		ClientAuth:       tls.RequestClientCert,
		GetCertificate:   opts.GetCertificate,
		VerifyConnection: func(cs tls.ConnectionState) error { return v.verify(cs, "") },
	}
	cfg := base.Clone()
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		srcIP := ""
		if hello.Conn != nil {
			srcIP = hostOf(hello.Conn.RemoteAddr())
		}
		c := base.Clone()
		c.VerifyConnection = func(cs tls.ConnectionState) error { return v.verify(cs, srcIP) }
		return c, nil
	}
	return cfg
}
