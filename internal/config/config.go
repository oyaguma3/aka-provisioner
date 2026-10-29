// Package config は環境変数から設定を読む（接頭辞 PROVISIONER_。設計概要 §11）。
// 下流の登録簿と管理クライアントのピンはファイルで与え、SIGHUP で読み直す（設計概要 §8）。
package config

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config は aka-provisioner の設定。
type Config struct {
	// Addr は API の待ち受けアドレス。
	Addr string
	// TLSCertFile・TLSKeyFile は API のサーバー証明書と秘密鍵。なければ自己署名を作る。
	TLSCertFile string
	TLSKeyFile  string
	// TLSHosts は自己署名のサーバー証明書の SAN。
	TLSHosts []string
	// AdminClientsFile は管理クライアントのピンのファイル（1 行に「識別名=フィンガープリント」）。
	AdminClientsFile string
	// DownstreamsFile は下流の登録簿のファイル（JSON）。
	DownstreamsFile string
	// DownstreamTimeout は下流の 1 回の呼び出しのタイムアウト。
	DownstreamTimeout time.Duration

	// ValkeyAddr・ValkeyPassword は専用の Valkey（Idempotency-Key の記録と監査ログ）。
	ValkeyAddr     string
	ValkeyPassword string

	// AuditMaxLen は監査ログの Stream の件数の上限（おおよそ）。
	AuditMaxLen int64
	// AuditDir は監査ログの日ごとのファイルの置き場所。
	AuditDir string
	// AuditRetentionDays は監査ログの日ごとのファイルを持つ日数。
	AuditRetentionDays int

	// HealthAddr は healthcheck の口（ループバック）。
	HealthAddr string

	LogLevel slog.Level
}

// Load は環境変数から設定を読む。誤りはまとめて返す。
func Load() (Config, error) {
	c := Config{
		Addr:             cmp.Or(os.Getenv("PROVISIONER_ADDR"), ":9446"),
		TLSCertFile:      cmp.Or(os.Getenv("PROVISIONER_TLS_CERT"), "/data/tls/cert.pem"),
		TLSKeyFile:       cmp.Or(os.Getenv("PROVISIONER_TLS_KEY"), "/data/tls/key.pem"),
		TLSHosts:         splitList(cmp.Or(os.Getenv("PROVISIONER_TLS_HOSTS"), "localhost,127.0.0.1,aka-provisioner")),
		AdminClientsFile: cmp.Or(os.Getenv("PROVISIONER_ADMIN_CLIENTS_FILE"), "/run/secrets/admin-clients"),
		DownstreamsFile:  cmp.Or(os.Getenv("PROVISIONER_DOWNSTREAMS_FILE"), "/etc/aka-provisioner/downstreams/downstreams.json"),

		ValkeyAddr:     cmp.Or(os.Getenv("PROVISIONER_VALKEY_ADDR"), "valkey:6379"),
		ValkeyPassword: os.Getenv("PROVISIONER_VALKEY_PASSWORD"),

		AuditDir:   cmp.Or(os.Getenv("PROVISIONER_AUDIT_DIR"), "/data/audit"),
		HealthAddr: cmp.Or(os.Getenv("PROVISIONER_HEALTH_ADDR"), "127.0.0.1:9090"),
	}

	var errs []error
	if v := os.Getenv("PROVISIONER_LOG_LEVEL"); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("PROVISIONER_LOG_LEVEL: %w", err))
		}
	}
	var err error
	if c.DownstreamTimeout, err = positiveDuration("PROVISIONER_DOWNSTREAM_TIMEOUT", 5*time.Second); err != nil {
		errs = append(errs, err)
	} else if c.DownstreamTimeout > MaxDownstreamTimeout {
		errs = append(errs, fmt.Errorf("PROVISIONER_DOWNSTREAM_TIMEOUT: must be %s or less", MaxDownstreamTimeout))
	}
	if c.AuditMaxLen, err = positiveInt("PROVISIONER_AUDIT_MAX", 100000); err != nil {
		errs = append(errs, err)
	}
	days, err := positiveInt("PROVISIONER_AUDIT_RETENTION_DAYS", 14)
	if err != nil {
		errs = append(errs, err)
	}
	c.AuditRetentionDays = int(days)
	if len(c.TLSHosts) == 0 {
		errs = append(errs, errors.New("PROVISIONER_TLS_HOSTS: at least one host is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return c, nil
}

// MaxDownstreamTimeout は PROVISIONER_DOWNSTREAM_TIMEOUT の上限。API のサーバーの書き込みの期限（60 秒）より十分に短くする。
const MaxDownstreamTimeout = 30 * time.Second

func positiveInt(name string, def int64) (int64, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s: must be a positive integer", name)
	}
	return n, nil
}

// positiveDuration は 5s や 1m のような時間を読む。
func positiveDuration(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: must be a positive duration such as 5s or 1m", name)
	}
	return d, nil
}

func splitList(v string) []string {
	var out []string
	for part := range strings.SplitSeq(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
