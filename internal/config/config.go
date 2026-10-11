// Package config は環境変数から設定を読み込む。一覧は docs/design-overview.md §11 と .env.example を参照。
package config

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/oyaguma3/eapaka-node-provisioner/internal/plmn"
)

// Config は provisioner の設定。
type Config struct {
	// Addr は provisioner の API の待ち受けアドレス。
	Addr string
	// TLSCertFile と TLSKeyFile は provisioner の API のサーバー証明書と秘密鍵（PEM）のパス。
	// どちらも存在しなければ、起動時に自己署名を生成してここに保存する。
	TLSCertFile string
	TLSKeyFile  string
	// TLSHosts は、自己署名のサーバー証明書を生成するときに SAN へ入れるホスト名と IP アドレス。
	TLSHosts []string
	// AdminClients は管理クライアント（BFF など）。証明書のフィンガープリント（SHA-256、16進小文字）から識別名を引く。
	AdminClients map[string]string

	// ClientCertFile は下流 2 つに提示するクライアント証明書（PEM）のパス。
	ClientCertFile string
	// ClientKeyFile はクライアント証明書の秘密鍵（PEM）のパス。空なら ClientCertFile から読む。
	ClientKeyFile string

	// ProvURL は本PoCの Provisioning API（provisioning-api）のベース URL。
	ProvURL string
	// ProvServerCertFile は provisioning-api のサーバー証明書（PEM）。これを信頼する証明書として検証する。
	ProvServerCertFile string
	// AkaURL は aka-only-server の管理API のベース URL。空なら aka-only-server を扱わない。
	AkaURL string
	// AkaServerCertFile は aka-only-server の管理API のサーバー証明書（PEM）。
	AkaServerCertFile string
	// DownstreamTimeout は下流の 1 回の呼び出しの上限時間。
	DownstreamTimeout time.Duration

	// PLMNMap は PLMN から鍵の置き場所への対応（本PoCの VECTOR_GATEWAY_PLMN_MAP と同じ値にする）。
	PLMNMap plmn.Map
	// AkaAVClientID は、本PoCの vector-gateway が aka-only-server に登録されている AVクライアントID。
	// aka-only-server を扱わない場合は 0。
	AkaAVClientID int64

	// ValkeyAddr と ValkeyPassword は provisioner 専用の Valkey の接続先とパスワード。
	ValkeyAddr     string
	ValkeyPassword string
	// AuditMaxLen は監査ログの保持件数の上限。
	AuditMaxLen int64

	// LogLevel はログの出力レベル。
	LogLevel slog.Level
}

// AkaEnabled は aka-only-server を扱うかを返す。
func (c Config) AkaEnabled() bool { return c.AkaURL != "" }

var (
	// adminClientNamePattern は管理クライアントの識別名（aka-only-server・provisioning-api と同じ規則）。
	adminClientNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	fingerprintPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Load は環境変数から設定を読み込み、形式と組み合わせを確かめる。
func Load() (Config, error) {
	c := Config{
		Addr:        cmp.Or(os.Getenv("PROVISIONER_ADDR"), ":9446"),
		TLSCertFile: cmp.Or(os.Getenv("PROVISIONER_TLS_CERT"), "/data/tls/cert.pem"),
		TLSKeyFile:  cmp.Or(os.Getenv("PROVISIONER_TLS_KEY"), "/data/tls/key.pem"),
		TLSHosts:    splitList(cmp.Or(os.Getenv("PROVISIONER_TLS_HOSTS"), "localhost,127.0.0.1,eapaka-provisioner")),

		ClientCertFile: cmp.Or(os.Getenv("PROVISIONER_CLIENT_CERT"), "/certs/client.pem"),
		ClientKeyFile:  os.Getenv("PROVISIONER_CLIENT_KEY"),

		ProvURL:            cmp.Or(os.Getenv("PROVISIONER_PROV_URL"), "https://provisioning-api:9444/admin/v1"),
		ProvServerCertFile: cmp.Or(os.Getenv("PROVISIONER_PROV_SERVER_CERT"), "/certs/prov-server.pem"),
		AkaURL:             os.Getenv("PROVISIONER_AKA_URL"),
		AkaServerCertFile:  cmp.Or(os.Getenv("PROVISIONER_AKA_SERVER_CERT"), "/certs/aka-server.pem"),

		ValkeyAddr:     cmp.Or(os.Getenv("PROVISIONER_VALKEY_ADDR"), "valkey:6379"),
		ValkeyPassword: os.Getenv("PROVISIONER_VALKEY_PASSWORD"),
	}

	var errs []error
	if v := os.Getenv("PROVISIONER_LOG_LEVEL"); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("PROVISIONER_LOG_LEVEL: %w", err))
		}
	}
	var err error
	if c.AdminClients, err = ParseAdminClients(os.Getenv("PROVISIONER_ADMIN_CLIENTS")); err != nil {
		errs = append(errs, fmt.Errorf("PROVISIONER_ADMIN_CLIENTS: %w", err))
	}
	if c.PLMNMap, err = plmn.Parse(os.Getenv("PROVISIONER_PLMN_MAP")); err != nil {
		errs = append(errs, fmt.Errorf("PROVISIONER_PLMN_MAP: %w", err))
	}
	if c.DownstreamTimeout, err = positiveDuration("PROVISIONER_DOWNSTREAM_TIMEOUT", 5*time.Second); err != nil {
		errs = append(errs, err)
	} else if c.DownstreamTimeout > MaxDownstreamTimeout {
		errs = append(errs, fmt.Errorf("PROVISIONER_DOWNSTREAM_TIMEOUT: must be %s or less (an operation calls the downstreams up to 5 times in sequence within the 60s IMSI lock)", MaxDownstreamTimeout))
	}
	if c.AuditMaxLen, err = positiveInt("PROVISIONER_AUDIT_MAX", 10000); err != nil {
		errs = append(errs, err)
	}
	if v := os.Getenv("PROVISIONER_AKA_AV_CLIENT_ID"); v != "" {
		if c.AkaAVClientID, err = strconv.ParseInt(v, 10, 64); err != nil || c.AkaAVClientID < 1 {
			errs = append(errs, errors.New("PROVISIONER_AKA_AV_CLIENT_ID: must be a positive integer"))
		}
	}

	// 組み合わせ。
	switch {
	case c.AkaEnabled() && os.Getenv("PROVISIONER_AKA_AV_CLIENT_ID") == "":
		errs = append(errs, errors.New("PROVISIONER_AKA_AV_CLIENT_ID is required when PROVISIONER_AKA_URL is set"))
	case !c.AkaEnabled() && c.PLMNMap.Uses(plmn.KeyStoreAKA):
		errs = append(errs, errors.New("PROVISIONER_PLMN_MAP maps a PLMN to 01 (aka-only-server) but PROVISIONER_AKA_URL is not set"))
	case !c.AkaEnabled() && c.AkaAVClientID != 0:
		errs = append(errs, errors.New("PROVISIONER_AKA_AV_CLIENT_ID is set but PROVISIONER_AKA_URL is not set"))
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return c, nil
}

// MaxDownstreamTimeout は PROVISIONER_DOWNSTREAM_TIMEOUT の上限。1 つの操作（加入者の作成など）は下流を順に最大 5 回
// 呼ぶ（存在確認、作成、認可ポリシー、補償 2 回）ので、その合計を IMSI のロックの有効期限（60 秒。api.LockTTL）に収める
// （設計概要 §9.1）。
const MaxDownstreamTimeout = 10 * time.Second

// CheckServe は、サーバーの起動に必須の設定がそろっているかを確かめる。
func (c Config) CheckServe() error {
	if len(c.AdminClients) == 0 {
		return errors.New("PROVISIONER_ADMIN_CLIENTS is not set (no admin client could connect)")
	}
	return nil
}

// ParseAdminClients は「識別名=フィンガープリント」のカンマ区切りを解釈する（aka-only-server の AKA_ADMIN_CLIENTS と同じ形式）。
// フィンガープリントは SHA-256 の 16進表記で、大文字やコロン区切り（openssl の出力形式）も受け付ける。
func ParseAdminClients(v string) (map[string]string, error) {
	clients := map[string]string{}
	for _, entry := range splitList(v) {
		name, fp, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		if !ok || !adminClientNamePattern.MatchString(name) {
			return nil, fmt.Errorf("bad entry %q: want <name>=<sha256 fingerprint>", entry)
		}
		fp = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(fp), ":", ""))
		if !fingerprintPattern.MatchString(fp) {
			return nil, fmt.Errorf("bad fingerprint for %q: want 64 hex digits", name)
		}
		if other, dup := clients[fp]; dup {
			return nil, fmt.Errorf("fingerprint of %q is already used by %q", name, other)
		}
		clients[fp] = name
	}
	return clients, nil
}

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
