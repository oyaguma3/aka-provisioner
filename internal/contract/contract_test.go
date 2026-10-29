// Package contract は、動いている aka-provisioner と本物の下流（aka-vecgen・aka-radius）を通した契約の試験（設計概要 §13。段階 3-1 の U4）。
// 下流は CI の契約の試験のジョブで、aka-mvpset の versions.env の固定のコミットからビルドして起動する（scripts/contract-up.sh）。
// 次の環境変数を与えたときだけ動く（なければ飛ばす）:
//
//	PROVISIONER_TEST_URL           aka-provisioner の API の基底 URL（例 https://127.0.0.1:19446/admin/v2）
//	PROVISIONER_TEST_SERVER_CERT   aka-provisioner のサーバー証明書（PEM）
//	PROVISIONER_TEST_CLIENT_CERT   管理クライアント（aka-ossbss の代わり）の証明書と秘密鍵（PEM。1 つのファイル）
//	PROVISIONER_TEST_AV_CLIENT_ID  aka-vecgen に登録した AV クライアントの ID（既定 1）
//	PROVISIONER_TEST_SOURCE_ID     aka-radius の取得先の登録簿の ID（既定 vecgen）
package contract

import (
	"bytes"
	"cmp"
	"crypto/tls"
	"crypto/x509"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

type client struct {
	t    *testing.T
	base string
	hc   *http.Client
}

func newClient(t *testing.T) *client {
	t.Helper()
	base := os.Getenv("PROVISIONER_TEST_URL")
	if base == "" {
		t.Skip("PROVISIONER_TEST_URL is not set")
	}
	serverPEM, err := os.ReadFile(os.Getenv("PROVISIONER_TEST_SERVER_CERT"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(serverPEM) {
		t.Fatal("no certificate in PROVISIONER_TEST_SERVER_CERT")
	}
	clientFile := os.Getenv("PROVISIONER_TEST_CLIENT_CERT")
	cert, err := tls.LoadX509KeyPair(clientFile, clientFile)
	if err != nil {
		t.Fatal(err)
	}
	return &client{t: t, base: strings.TrimSuffix(base, "/"), hc: &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{cert}}},
	}}
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return v
}

// do は要求を送る。key が空でなければ Idempotency-Key を付ける。操作者とトレースID はいつも付ける。
func (c *client) do(method, path, key, traceID string, body any) response {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	req.Header.Set("X-Operator-Id", "contract-op")
	req.Header.Set("X-Trace-ID", traceID)
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: b}
}

// want は状態コードを確かめる。
func (c *client) want(r response, status int, what string) response {
	c.t.Helper()
	if r.status != status {
		c.t.Fatalf("%s: status %d, want %d: %s", what, r.status, status, r.body)
	}
	return r
}

// waitReachable は、aka-provisioner が全下流につながったと /status に出るまで待つ。
func (c *client) waitReachable() {
	c.t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for {
		st := c.want(c.do(http.MethodGet, "/status", "", "contract-status", nil), 200, "status").json(c.t)
		all := true
		for _, d := range st["downstreams"].([]any) {
			if d.(map[string]any)["reachable"] != true {
				all = false
			}
		}
		if all {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("downstreams are not reachable: %v", st["downstreams"])
		}
		time.Sleep(time.Second)
	}
}

// auditHas は、監査ログの口 path の新しい 500 件に、action と traceId の組があるかを返す。
func (c *client) auditHas(path, action, traceID string) bool {
	c.t.Helper()
	out := c.want(c.do(http.MethodGet, path+"?limit=500", "", "contract-audit", nil), 200, path).json(c.t)
	for _, it := range out["items"].([]any) {
		e := it.(map[string]any)
		if e["action"] == action && e["traceId"] == traceID && e["operator"] == "contract-op" {
			return true
		}
	}
	return false
}

func TestContract(t *testing.T) {
	c := newClient(t)
	c.waitReachable()
	run := time.Now().UTC().Format("150405")
	imsi := "44010" + "9" + strings.Repeat("0", 3) + run // 15 桁（実行ごとに違う値）
	avClientID := cmp.Or(os.Getenv("PROVISIONER_TEST_AV_CLIENT_ID"), "1")
	sourceID := cmp.Or(os.Getenv("PROVISIONER_TEST_SOURCE_ID"), "vecgen")
	key := func(op string) string { return "contract-" + run + "-" + op }

	t.Run("vecgen", func(t *testing.T) {
		c.t = t
		create := map[string]any{"imsi": imsi, "ki": "465b5ce8b199b49faa5f0a2ee238a6bc", "opc": "cd63cb71954a9f4e48a5994e37a02baf",
			"state": "active", "allowedClientIds": []any{jsontext.Value(avClientID)}}
		// 書き込みは Idempotency-Key が必須。
		p := c.want(c.do(http.MethodPost, "/vecgen/subscribers", "", "t-vg-nokey", create), 400, "create without key").json(t)
		if p["cause"] != "MANDATORY_IE_MISSING" {
			t.Errorf("without key = %v", p)
		}
		r := c.want(c.do(http.MethodPost, "/vecgen/subscribers", key("vg-create"), "t-vg-create", create), 201, "create")
		if loc := r.header.Get("Location"); loc != "/admin/v2/vecgen/subscribers/"+imsi {
			t.Errorf("Location = %q", loc)
		}
		if strings.Contains(string(r.body), "465b5ce8") {
			t.Error("the response contains the key")
		}
		// 同じキーの再送は最初の応答を返し、内容が違えば 422。
		again := c.want(c.do(http.MethodPost, "/vecgen/subscribers", key("vg-create"), "t-vg-create2", create), 201, "replay")
		if again.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(again.body, r.body) {
			t.Errorf("replay = %v %s", again.header, again.body)
		}
		create["state"] = "inactive"
		c.want(c.do(http.MethodPost, "/vecgen/subscribers", key("vg-create"), "t-vg-create3", create), 422, "mismatch")

		if s := c.want(c.do(http.MethodGet, "/vecgen/subscribers/"+imsi, "", "t-vg-get", nil), 200, "get").json(t); s["state"] != "active" {
			t.Errorf("get = %v", s)
		}
		c.want(c.do(http.MethodGet, "/vecgen/subscribers?prefix="+imsi[:10]+"&limit=10", "", "t-vg-list", nil), 200, "list")
		bg := c.want(c.do(http.MethodPost, "/vecgen/subscribers/batch-get", "", "t-vg-batch",
			map[string]any{"imsis": []string{imsi, "001019999999999"}}), 200, "batch-get").json(t)
		if len(bg["items"].([]any)) != 1 || !slices.Contains(bg["notFound"].([]any), any("001019999999999")) {
			t.Errorf("batch-get = %v", bg)
		}
		c.want(c.do(http.MethodPatch, "/vecgen/subscribers/"+imsi, key("vg-patch"), "t-vg-patch", map[string]any{"amf": "8001"}), 200, "patch")
		c.want(c.do(http.MethodPut, "/vecgen/subscribers/"+imsi+"/state", key("vg-state1"), "t-vg-state1", map[string]any{"state": "inactive"}), 200, "inactive")
		c.want(c.do(http.MethodPut, "/vecgen/subscribers/"+imsi+"/state", key("vg-state2"), "t-vg-state2",
			map[string]any{"state": "terminated", "reason": "contract"}), 200, "terminated")
		c.want(c.do(http.MethodPost, "/vecgen/subscribers/"+imsi+"/reinstate", key("vg-reinstate"), "t-vg-reinstate",
			map[string]any{"reason": "contract"}), 200, "reinstate")

		// 鍵の読み出しは通さない。
		if p := c.want(c.do(http.MethodGet, "/vecgen/subscribers/"+imsi+"/keys", "", "t-vg-keys", nil), 404, "keys").json(t); p["cause"] != "ROUTE_NOT_ALLOWED" {
			t.Errorf("keys = %v", p)
		}
		c.want(c.do(http.MethodGet, "/vecgen/status", "", "t-vg-status", nil), 200, "status")
		c.want(c.do(http.MethodGet, "/vecgen/clients", "", "t-vg-clients", nil), 200, "clients")
		c.want(c.do(http.MethodGet, "/vecgen/clients/"+avClientID, "", "t-vg-client", nil), 200, "client")
		c.want(c.do(http.MethodGet, "/vecgen/av-server-certificate", "", "t-vg-avcert", nil), 200, "av-server-certificate")
		c.want(c.do(http.MethodDelete, "/vecgen/subscribers/"+imsi, key("vg-delete"), "t-vg-delete", nil), 204, "delete")
		// 下流の誤りは downstream を付けてそのまま返す。
		if p := c.want(c.do(http.MethodGet, "/vecgen/subscribers/"+imsi, "", "t-vg-gone", nil), 404, "gone").json(t); p["downstream"] != "vecgen" {
			t.Errorf("gone = %v", p)
		}
		// 操作者とトレースID が下流の監査ログに残る。
		for action, traceID := range map[string]string{"subscriber.create": "t-vg-create", "subscriber.delete": "t-vg-delete"} {
			if !c.auditHas("/vecgen/audit-logs", action, traceID) {
				t.Errorf("aka-vecgen audit log has no %s with %s", action, traceID)
			}
		}
	})

	t.Run("radius", func(t *testing.T) {
		c.t = t
		rules := []any{map[string]any{"tenantId": 42, "nasId": "*", "ssids": []string{"*"}}}
		create := map[string]any{"imsi": imsi, "state": "active", "sourceId": sourceID, "rules": rules}
		r := c.want(c.do(http.MethodPost, "/radius/subscribers", key("rd-create"), "t-rd-create", create), 201, "create")
		if loc := r.header.Get("Location"); loc != "/admin/v2/radius/subscribers/"+imsi {
			t.Errorf("Location = %q", loc)
		}
		if s := c.want(c.do(http.MethodGet, "/radius/subscribers/"+imsi, "", "t-rd-get", nil), 200, "get").json(t); s["sourceId"] != sourceID {
			t.Errorf("get = %v", s)
		}
		c.want(c.do(http.MethodGet, "/radius/subscribers?prefix="+imsi[:10], "", "t-rd-list", nil), 200, "list")
		c.want(c.do(http.MethodPost, "/radius/subscribers/batch-get", "", "t-rd-batch", map[string]any{"imsis": []string{imsi}}), 200, "batch-get")
		rules = append(rules, map[string]any{"tenantId": 42, "nasId": "ap-01", "ssids": []string{"corp"}, "vlanId": 100})
		c.want(c.do(http.MethodPatch, "/radius/subscribers/"+imsi, key("rd-patch"), "t-rd-patch", map[string]any{"rules": rules}), 200, "patch")
		c.want(c.do(http.MethodPut, "/radius/subscribers/"+imsi+"/state", key("rd-state1"), "t-rd-state1",
			map[string]any{"state": "terminated", "reason": "contract"}), 200, "terminated")
		c.want(c.do(http.MethodPost, "/radius/subscribers/"+imsi+"/reinstate", key("rd-reinstate"), "t-rd-reinstate",
			map[string]any{"reason": "contract"}), 200, "reinstate")
		c.want(c.do(http.MethodGet, "/radius/status", "", "t-rd-status", nil), 200, "status")
		c.want(c.do(http.MethodGet, "/radius/sessions?tenantId=42", "", "t-rd-sessions", nil), 200, "sessions")
		day := time.Now().In(time.FixedZone("JST", 9*60*60)).Format(time.DateOnly)
		c.want(c.do(http.MethodGet, "/radius/usage?date="+day, "", "t-rd-usage", nil), 200, "usage")
		c.want(c.do(http.MethodGet, "/radius/clients", "", "t-rd-clients", nil), 200, "clients")
		if s := c.want(c.do(http.MethodGet, "/radius/sources", "", "t-rd-sources", nil), 200, "sources").body; !strings.Contains(string(s), `"`+sourceID+`"`) {
			t.Errorf("sources = %s", s)
		}
		// RADIUS クライアントの書き込みは通さない。
		if p := c.want(c.do(http.MethodPost, "/radius/clients", key("rd-client"), "t-rd-client", map[string]any{}), 404, "clients write").json(t); p["cause"] != "ROUTE_NOT_ALLOWED" {
			t.Errorf("clients write = %v", p)
		}
		c.want(c.do(http.MethodDelete, "/radius/subscribers/"+imsi, key("rd-delete"), "t-rd-delete", nil), 204, "delete")
		for action, traceID := range map[string]string{"subscriber.create": "t-rd-create", "subscriber.delete": "t-rd-delete"} {
			if !c.auditHas("/radius/audit-logs", action, traceID) {
				t.Errorf("aka-radius audit log has no %s with %s", action, traceID)
			}
		}
	})

	t.Run("provisioner", func(t *testing.T) {
		c.t = t
		for _, a := range []struct{ action, traceID string }{
			{"vecgen.subscriber.create", "t-vg-create"}, {"vecgen.subscriber.reinstate", "t-vg-reinstate"},
			{"radius.subscriber.update", "t-rd-patch"}, {"radius.subscriber.delete", "t-rd-delete"},
		} {
			if !c.auditHas("/audit-logs", a.action, a.traceID) {
				t.Errorf("aka-provisioner audit log has no %s with %s", a.action, a.traceID)
			}
		}
		st := c.want(c.do(http.MethodGet, "/status", "", "t-status", nil), 200, "status").json(t)
		for _, d := range st["downstreams"].([]any) {
			if m := d.(map[string]any); m["status"] == nil {
				t.Errorf("status of %v is missing", m["name"])
			}
		}
		fmt.Println("aka-provisioner status:", st["version"], st["adminClients"])
	})
}
