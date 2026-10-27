package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/config"
)

// healthcheckTimeout は、healthcheck で待ち受けに接続するときの上限。
const healthcheckTimeout = 3 * time.Second

// healthcheck は、同じコンテナで動いている provisioner の API が接続を受け付けているかを確かめる。
// distroless のイメージには curl がないので、compose の healthcheck はこれを使う。
// API は mTLS で、管理クライアントの証明書がないと要求を送れないので、TCP で接続できるかだけを見る
// （下流や Valkey の状態は、ログと /status で確かめる）。
func healthcheck(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return checkListener(ctx, cfg.Addr)
}

// checkListener は、待ち受けアドレスのポートに、ループバックで TCP 接続できるかを確かめる。
func checkListener(ctx context.Context, addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("PROVISIONER_ADDR %q: %w", addr, err)
	}
	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return fmt.Errorf("listener %s: %w", addr, err)
	}
	return conn.Close()
}
