package main

import (
	"context"

	"github.com/oyaguma3/aka-provisioner/internal/config"
	"github.com/oyaguma3/aka-provisioner/internal/health"
)

// healthcheck は、同じコンテナで動いている provisioner のループバックの healthcheck の口に問い合わせる
// （専用の Valkey に届くか。設計概要 §9）。distroless のイメージには curl がないので、compose の healthcheck はこれを使う。
func healthcheck(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return health.Check(ctx, cfg.HealthAddr)
}
