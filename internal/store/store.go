// Package store は provisioner 専用の Valkey を扱う。
// 用途は操作の記録・IMSI ごとのロック・Idempotency-Key・監査ログに限る（加入者のデータは持たない）。
// キー設計は docs/design-overview.md §8 を参照。
package store

import (
	"context"
	"fmt"

	"github.com/valkey-io/valkey-go"
)

// Store は Valkey への接続を持つ。
type Store struct {
	c valkey.Client
}

// Options は Valkey への接続設定。
type Options struct {
	Addr     string // host:port
	Password string
	// DB は使用する論理データベースの番号。通常は 0。
	DB int
}

// Open は Valkey に接続し、疎通を確認する。
func Open(ctx context.Context, o Options) (*Store, error) {
	c, err := valkey.NewClient(valkey.ClientOption{
		InitAddress: []string{o.Addr},
		Password:    o.Password,
		SelectDB:    o.DB,
		ClientName:  "aka-provisioner",
		// 単一ノード前提で、クライアント側キャッシュは使わない。
		DisableCache: true,
	})
	if err != nil {
		return nil, fmt.Errorf("connect valkey %s: %w", o.Addr, err)
	}
	if err := c.Do(ctx, c.B().Ping().Build()).Error(); err != nil {
		c.Close()
		return nil, fmt.Errorf("ping valkey %s: %w", o.Addr, err)
	}
	return &Store{c: c}, nil
}

// Close は接続を閉じる。
func (s *Store) Close() { s.c.Close() }

// Ping は Valkey に接続できるかを確かめる。
func (s *Store) Ping(ctx context.Context) error {
	return s.c.Do(ctx, s.c.B().Ping().Build()).Error()
}
