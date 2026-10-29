package config

import (
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":9446" || c.AdminClientsFile != "/run/secrets/admin-clients" ||
		c.DownstreamsFile != "/etc/aka-provisioner/downstreams/downstreams.json" ||
		c.DownstreamTimeout != 5*time.Second || c.AuditMaxLen != 100000 || c.AuditDir != "/data/audit" ||
		c.AuditRetentionDays != 14 || c.HealthAddr != "127.0.0.1:9090" || c.ValkeyAddr != "valkey:6379" ||
		strings.Join(c.TLSHosts, ",") != "localhost,127.0.0.1,aka-provisioner" {
		t.Errorf("defaults = %+v", c)
	}
}

func TestLoad(t *testing.T) {
	t.Setenv("PROVISIONER_ADMIN_CLIENTS_FILE", "/x/pins")
	t.Setenv("PROVISIONER_DOWNSTREAMS_FILE", "/x/downstreams.json")
	t.Setenv("PROVISIONER_DOWNSTREAM_TIMEOUT", "3s")
	t.Setenv("PROVISIONER_AUDIT_MAX", "50")
	t.Setenv("PROVISIONER_AUDIT_RETENTION_DAYS", "7")
	t.Setenv("PROVISIONER_LOG_LEVEL", "debug")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminClientsFile != "/x/pins" || c.DownstreamsFile != "/x/downstreams.json" || c.DownstreamTimeout != 3*time.Second ||
		c.AuditMaxLen != 50 || c.AuditRetentionDays != 7 || c.LogLevel.String() != "DEBUG" {
		t.Errorf("config = %+v", c)
	}
}

func TestLoadErrors(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"タイムアウトの形":     {"PROVISIONER_DOWNSTREAM_TIMEOUT": "5"},
		"タイムアウトの上限":    {"PROVISIONER_DOWNSTREAM_TIMEOUT": "31s"},
		"監査ログの上限":      {"PROVISIONER_AUDIT_MAX": "0"},
		"監査ログの日数":      {"PROVISIONER_AUDIT_RETENTION_DAYS": "x"},
		"ログの段階":        {"PROVISIONER_LOG_LEVEL": "loud"},
		"サーバー証明書の SAN": {"PROVISIONER_TLS_HOSTS": " , "},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Error("want error")
			}
		})
	}
}
