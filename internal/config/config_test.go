package config

import (
	"strings"
	"testing"
	"time"

	"github.com/oyaguma3/eapaka-node-provisioner/internal/plmn"
)

const fp = "7a7a91920ccfb51760f70d6dba4ceb8ba4386eec2728955983ad04c42e273043"

func TestDefaults(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":9446" || c.ProvURL != "https://provisioning-api:9444/admin/v1" || c.AkaEnabled() ||
		c.DownstreamTimeout != 5*time.Second || c.AuditMaxLen != 10000 || c.ValkeyAddr != "valkey:6379" ||
		strings.Join(c.TLSHosts, ",") != "localhost,127.0.0.1,eapaka-provisioner" || c.ClientCertFile != "/certs/client.pem" {
		t.Errorf("defaults = %+v", c)
	}
	// 管理クライアントがなければ起動しない。
	if err := c.CheckServe(); err == nil {
		t.Error("CheckServe without admin clients: want error")
	}
}

func TestLoad(t *testing.T) {
	t.Setenv("PROVISIONER_ADMIN_CLIENTS", "bff="+strings.ToUpper(fp[:2])+":"+fp[2:])
	t.Setenv("PROVISIONER_AKA_URL", "https://aka-only-server:9443/admin/v1")
	t.Setenv("PROVISIONER_AKA_AV_CLIENT_ID", "3")
	t.Setenv("PROVISIONER_PLMN_MAP", "00102:01")
	t.Setenv("PROVISIONER_DOWNSTREAM_TIMEOUT", "3s")
	t.Setenv("PROVISIONER_LOG_LEVEL", "debug")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminClients[fp] != "bff" || !c.AkaEnabled() || c.AkaAVClientID != 3 ||
		c.PLMNMap.KeyStore("001020000000001") != plmn.KeyStoreAKA || c.DownstreamTimeout != 3*time.Second {
		t.Errorf("config = %+v", c)
	}
	if err := c.CheckServe(); err != nil {
		t.Error(err)
	}
}

func TestLoadErrors(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"bad admin client":       {"PROVISIONER_ADMIN_CLIENTS": "bff=abc"},
		"bad admin client name":  {"PROVISIONER_ADMIN_CLIENTS": "b f=" + fp},
		"duplicate fingerprint":  {"PROVISIONER_ADMIN_CLIENTS": "a=" + fp + ",b=" + fp},
		"bad plmn map":           {"PROVISIONER_PLMN_MAP": "00102:02"},
		"aka plmn without url":   {"PROVISIONER_PLMN_MAP": "00102:01"},
		"aka url without client": {"PROVISIONER_AKA_URL": "https://aka-only-server:9443/admin/v1"},
		"client without aka url": {"PROVISIONER_AKA_AV_CLIENT_ID": "1"},
		"bad av client id": {
			"PROVISIONER_AKA_URL": "https://aka-only-server:9443/admin/v1", "PROVISIONER_AKA_AV_CLIENT_ID": "0",
		},
		"bad timeout":   {"PROVISIONER_DOWNSTREAM_TIMEOUT": "5"},
		"long timeout":  {"PROVISIONER_DOWNSTREAM_TIMEOUT": "11s"},
		"bad audit max": {"PROVISIONER_AUDIT_MAX": "-1"},
		"bad log level": {"PROVISIONER_LOG_LEVEL": "verbose"},
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

func TestDownstreamTimeoutLimit(t *testing.T) {
	t.Setenv("PROVISIONER_DOWNSTREAM_TIMEOUT", "10s")
	c, err := Load()
	if err != nil || c.DownstreamTimeout != MaxDownstreamTimeout {
		t.Errorf("10s: %v %v", c.DownstreamTimeout, err)
	}
}
