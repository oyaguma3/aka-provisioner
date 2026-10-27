package downstreamtest

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/downstream"
)

// 契約テスト（実際の下流を相手にしたテスト）の接続先は、次の環境変数で指定する。指定しなければテストはスキップする。
//
//	PROVISIONER_TEST_CLIENT_CERT       provisioner のクライアント証明書（と秘密鍵）の PEM。下流 2 つに登録しておく
//	PROVISIONER_TEST_CLIENT_KEY        秘密鍵の PEM（証明書のファイルに含めた場合は不要）
//	PROVISIONER_TEST_PROV_URL          provisioning-api のベース URL（例: https://127.0.0.1:19444/admin/v1）
//	PROVISIONER_TEST_PROV_SERVER_CERT  provisioning-api のサーバー証明書の PEM
//	PROVISIONER_TEST_AKA_URL           aka-vecgen の管理API のベース URL（例: https://127.0.0.1:19443/admin/v1）
//	PROVISIONER_TEST_AKA_SERVER_CERT   aka-vecgen の管理API のサーバー証明書の PEM

// IntegrationOperator は契約テストで下流に渡す操作者。
const IntegrationOperator = "it-provisioner"

// IntegrationOptions は、契約テストの接続先の設定を返す。name は PROV または AKA。
// 接続先が指定されていなければテストをスキップする。
func IntegrationOptions(t *testing.T, name string) downstream.Options {
	t.Helper()
	url := os.Getenv("PROVISIONER_TEST_" + name + "_URL")
	if url == "" {
		t.Skip("PROVISIONER_TEST_" + name + "_URL is not set")
	}
	return downstream.Options{
		BaseURL:        url,
		ClientCertFile: os.Getenv("PROVISIONER_TEST_CLIENT_CERT"),
		ClientKeyFile:  os.Getenv("PROVISIONER_TEST_CLIENT_KEY"),
		ServerCertFile: os.Getenv("PROVISIONER_TEST_" + name + "_SERVER_CERT"),
		Timeout:        10 * time.Second,
		Log:            Discard,
	}
}

// TestIMSI は、契約テスト用の IMSI（PLMN 00101 で始まるテスト用の番号）を返す。
func TestIMSI() string {
	return fmt.Sprintf("00101%010d", time.Now().UnixNano()%1e10)
}

// AuditEntry は、下流の監査ログの 1 件のうち、契約テストで確かめる項目（prov と aka で共通の項目）。
type AuditEntry struct {
	Operator   string `json:"operator"`
	MgmtClient string `json:"mgmtClient"`
	Action     string `json:"action"`
	Target     string `json:"target"`
	TraceID    string `json:"traceId"`
}

// FindAudit は、下流の監査ログ（GET /audit-logs）の新しい方から、トレースID と操作が一致するものを探す。
func FindAudit(t *testing.T, c *downstream.Client, traceID, action string) (AuditEntry, bool) {
	t.Helper()
	resp, err := c.Relay(t.Context(), downstream.RelayRequest{Method: http.MethodGet, Path: []string{"audit-logs"}, RawQuery: "limit=100"})
	if err != nil || resp.Status != http.StatusOK {
		t.Fatalf("audit-logs: %+v, %v", resp, err)
	}
	var l struct {
		Items []AuditEntry `json:"items"`
	}
	if err := json.Unmarshal(resp.Body, &l); err != nil {
		t.Fatal(err)
	}
	for _, e := range l.Items {
		if e.TraceID == traceID && e.Action == action {
			return e, true
		}
	}
	return AuditEntry{}, false
}

// CheckAudit は、トレースID traceID の操作 action が、下流の監査ログに操作者と管理クライアントつきで記録されたことを確かめる。
func CheckAudit(t *testing.T, c *downstream.Client, traceID, action, target string) {
	t.Helper()
	e, ok := FindAudit(t, c, traceID, action)
	if !ok {
		t.Errorf("audit %s (trace %s) not found", action, traceID)
		return
	}
	if e.Operator != IntegrationOperator || e.MgmtClient == "" || e.Target != target {
		t.Errorf("audit %s = %+v", action, e)
	}
}
