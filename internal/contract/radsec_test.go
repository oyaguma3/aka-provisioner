package contract

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// radsec-ca・エッジとの契約の試験（段階 4-2 の P12）。scripts/contract.sh が radsec-ca とエッジを固定のコミットから
// compose で起動したときだけ動く（次の環境変数がなければ飛ばす）:
//
//	PROVISIONER_TEST_EDGE_ID         起動したエッジの ID
//	PROVISIONER_TEST_EDGE_DOWN_ID    登録簿にあるが届かないエッジの ID
//	PROVISIONER_TEST_WORK            contract.sh の作業ディレクトリ（aka-provisioner のログと監査ログのファイル）
//	PROVISIONER_TEST_CA_AUDIT_DIR    radsec-ca の監査ログのファイルのディレクトリ
//	PROVISIONER_TEST_EDGE_AUDIT_DIR  エッジの監査ログのファイルのディレクトリ

const (
	// テナントの名前。contract.sh の片付けのコンテナの名前（radsecproxy-<slug>）と、.env の名前の規則に合わせる。
	radsecTenantSlug = "acme"
	radsecTenantID   = "42"
	radsecTenantPort = 20993
	radsecURNPrefix  = "example"
	radsecDNSDomain  = "radsec.example.net"
	// merakiKey は登録の要求の本文に入れる試しの値。ログと監査ログに残らないことを確かめる。
	merakiKey = "contract-meraki-api-key-do-not-log"
)

// testCA は持ち込みの CA の代わり（業者の CA。自己署名）。
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	k := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: k, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

// crl は番号 number の CRL（PEM）を返す。
func (ca *testCA) crl(t *testing.T, number int64) string {
	t.Helper()
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number: big.NewInt(number), ThisUpdate: time.Now().Add(-time.Minute), NextUpdate: time.Now().Add(7 * 24 * time.Hour),
	}, ca.cert, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der}))
}

// leaf は業者の AP の証明書を作る。
func (ca *testCA) leaf(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	k := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, k.Public(), ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}

// newCSR は鍵と CSR（PEM）を作る。
func newCSR(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	k := newKey(t)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "contract"}}, k)
	if err != nil {
		t.Fatal(err)
	}
	return k, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

// keyPair は発行の応答の証明書と鍵から tls.Certificate を作る。
func keyPair(t *testing.T, certPEM string, key *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()
	b, _ := pem.Decode([]byte(certPEM))
	if b == nil {
		t.Fatalf("no certificate in %q", certPEM)
	}
	return tls.Certificate{Certificate: [][]byte{b.Bytes}, PrivateKey: key}
}

// apDial はテナントの口に TLS でつなぎ、受け入れられたかを返す。TLS 1.3 ではハンドシェイクの後に断られることがあるので、
// 少し読んで確かめる（受け入れられたら radsecproxy は何も送らないので、読むのは時間切れになる）。
func apDial(cert tls.Certificate, roots *x509.CertPool) error {
	d := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(radsecTenantPort)), &tls.Config{
		MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, RootCAs: roots,
		ServerName: "primary." + radsecTenantSlug + "." + radsecDNSDomain,
	})
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = conn.Read(make([]byte, 1))
	if nerr, ok := errors.AsType[net.Error](err); ok && nerr.Timeout() {
		return nil
	}
	if err == nil {
		return errors.New("unexpected data")
	}
	return err
}

// wantAccepted は接続が受け入れられるまで少し待つ。
func wantAccepted(t *testing.T, what string, cert tls.Certificate, roots *x509.CertPool) {
	t.Helper()
	var err error
	for range 15 {
		if err = apDial(cert, roots); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
	t.Errorf("%s: want accepted: %v", what, err)
}

// wantRejected は接続が断られるまで少し待つ（CRL の読み直しの間）。
func wantRejected(t *testing.T, what string, cert tls.Certificate, roots *x509.CertPool) {
	t.Helper()
	for range 15 {
		if apDial(cert, roots) != nil {
			return
		}
		time.Sleep(time.Second)
	}
	t.Errorf("%s: want rejected", what)
}

// auditFiles は UID の違うコンテナが書いた監査ログのファイルを、コンテナの中から読む。
func auditFiles(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("docker", "run", "--rm", "-v", dir+":/a:ro", "busybox:1.37.0", "sh", "-c", "cat /a/audit-*.jsonl").CombinedOutput()
	if err != nil {
		t.Fatalf("read audit files in %s: %v: %s", dir, err, out)
	}
	return string(out)
}

// TestContractRadsec は radsec-ca の 13 の口とエッジの 14 の口を aka-provisioner を通して呼び、
// 宣言 → CSR → 署名 → 反映 → AP の TLS 接続 → 失効 → CRL の取り直し → 拒否までを通す。
func TestContractRadsec(t *testing.T) {
	edgeID := os.Getenv("PROVISIONER_TEST_EDGE_ID")
	if edgeID == "" {
		t.Skip("PROVISIONER_TEST_EDGE_ID is not set")
	}
	downID := os.Getenv("PROVISIONER_TEST_EDGE_DOWN_ID")
	c := newClient(t)
	c.waitReachable()
	run := time.Now().UTC().Format("150405")
	key := func(op string) string { return "contract-" + run + "-" + op }
	edge := "/edges/" + edgeID
	tenant := edge + "/tenants/" + radsecTenantID
	acmeCA := "tenant." + radsecTenantSlug
	broughtCA := "brought." + radsecTenantSlug + ".vendor"
	vendor := newTestCA(t, "contract vendor CA")

	var acmeCAPEM string
	serverRoots := x509.NewCertPool()

	t.Run("radsec-ca の登録と発行", func(t *testing.T) {
		c.t = t
		if st := c.want(c.do(http.MethodGet, "/ca/status", "", "t-ca-status", nil), 200, "status").json(t); st["service"] != "radsec-ca" {
			t.Errorf("status = %v", st)
		}
		create := map[string]any{"tenant": radsecTenantSlug}
		// 書き込みは Idempotency-Key が必須。
		if p := c.want(c.do(http.MethodPost, "/ca/cas", "", "t-ca-nokey", create), 400, "without key").json(t); p["cause"] != "MANDATORY_IE_MISSING" {
			t.Errorf("without key = %v", p)
		}
		r := c.want(c.do(http.MethodPost, "/ca/cas", key("ca-create"), "t-ca-create", create), 201, "create CA")
		if loc := r.header.Get("Location"); loc != "/admin/v2/ca/cas/"+acmeCA {
			t.Errorf("Location = %q", loc)
		}
		// 同じキーの再送は最初の応答を返し、内容が違えば 422。
		again := c.want(c.do(http.MethodPost, "/ca/cas", key("ca-create"), "t-ca-create2", create), 201, "replay")
		if again.header.Get("Idempotent-Replayed") != "true" || again.header.Get("Location") != "/admin/v2/ca/cas/"+acmeCA {
			t.Errorf("replay = %v", again.header)
		}
		c.want(c.do(http.MethodPost, "/ca/cas", key("ca-create"), "t-ca-create3", map[string]any{"tenant": "other"}), 422, "mismatch")
		// 下流の誤りは downstream を付けてそのまま返す。覚えた 4xx は再送でも同じ。
		if p := c.want(c.do(http.MethodPost, "/ca/cas", key("ca-create-dup"), "t-ca-dup", create), 409, "duplicate").json(t); p["cause"] != "CA_EXISTS" || p["downstream"] != "ca" {
			t.Errorf("duplicate = %v", p)
		}

		acme := c.want(c.do(http.MethodGet, "/ca/cas/"+acmeCA, "", "t-ca-get", nil), 200, "get CA").json(t)
		acmeCAPEM, _ = acme["certificate"].(string)
		list := c.want(c.do(http.MethodGet, "/ca/cas?tenant="+radsecTenantSlug, "", "t-ca-list", nil), 200, "list CAs").json(t)
		if len(list["items"].([]any)) != 1 {
			t.Errorf("list = %v", list)
		}
		server := c.want(c.do(http.MethodGet, "/ca/cas/server", "", "t-ca-server", nil), 200, "server CA").json(t)
		if !serverRoots.AppendCertsFromPEM([]byte(server["certificate"].(string))) {
			t.Fatal("no certificate in the server CA")
		}
		if crl := c.want(c.do(http.MethodGet, "/ca/cas/"+acmeCA+"/crl", "", "t-ca-crl", nil), 200, "CRL").json(t); !strings.Contains(crl["crl"].(string), "X509 CRL") {
			t.Errorf("crl = %v", crl)
		}
		// パスの値の形は下流に送る前に確かめる。
		if p := c.want(c.do(http.MethodGet, "/ca/cas/acme", "", "t-ca-bad", nil), 400, "bad caId").json(t); p["cause"] != "MANDATORY_IE_INCORRECT" {
			t.Errorf("bad caId = %v", p)
		}
		// 許していない口は通さない。
		if p := c.want(c.do(http.MethodDelete, "/ca/cas/"+acmeCA, key("ca-del"), "t-ca-del", nil), 404, "not allowed").json(t); p["cause"] != "ROUTE_NOT_ALLOWED" {
			t.Errorf("not allowed = %v", p)
		}
	})

	t.Run("持ち込みの CA", func(t *testing.T) {
		c.t = t
		// Meraki の API キーは本文に入れて通す（aka-provisioner はログにも監査ログにも残さない）。届かない口なので下流は 422 を返す。
		p := c.want(c.do(http.MethodPost, "/ca/cas", key("ca-meraki"), "t-ca-meraki", map[string]any{
			"tenant": radsecTenantSlug, "kind": "brought", "name": "meraki", "certPem": newTestCA(t, "contract meraki CA").pem, "tenantOnlyConfirmed": true,
			"crl": map[string]any{"source": "meraki", "merakiOrgId": "1", "merakiApiKey": merakiKey},
		}), 422, "meraki").json(t)
		if p["cause"] != "CRL_INVALID" || p["downstream"] != "ca" || strings.Contains(p["detail"].(string), merakiKey) {
			t.Errorf("meraki = %v", p)
		}

		r := c.want(c.do(http.MethodPost, "/ca/cas", key("ca-brought"), "t-ca-brought", map[string]any{
			"tenant": radsecTenantSlug, "kind": "brought", "name": "vendor", "certPem": vendor.pem, "tenantOnlyConfirmed": true,
			"crl": map[string]any{"source": "upload", "pem": vendor.crl(t, 1)},
		}), 201, "brought CA")
		if loc := r.header.Get("Location"); loc != "/admin/v2/ca/cas/"+broughtCA {
			t.Errorf("Location = %q", loc)
		}
		if got := r.json(t); got["selfSigned"] != true || got["hasCrl"] != true {
			t.Errorf("brought CA = %v", got)
		}
		c.want(c.do(http.MethodPut, "/ca/cas/"+broughtCA+"/crl", key("ca-crl2"), "t-ca-crl-put", map[string]any{"pem": vendor.crl(t, 2)}), 200, "upload CRL")
		c.want(c.do(http.MethodPut, "/ca/cas/"+broughtCA+"/crl-source", key("ca-source"), "t-ca-source",
			map[string]any{"crl": map[string]any{"source": "upload", "pem": vendor.crl(t, 3)}}), 200, "CRL source")
		// upload の CA は取りに行けない（下流まで届いて断られる）。
		if p := c.want(c.do(http.MethodPost, "/ca/cas/"+broughtCA+"/crl:refresh", key("ca-refresh"), "t-ca-refresh", nil), 400, "CRL refresh").json(t); p["cause"] != "VALIDATION_ERROR" || p["downstream"] != "ca" {
			t.Errorf("refresh = %v", p)
		}
	})

	var apSerial string
	var ap, group tls.Certificate

	t.Run("宣言から反映まで", func(t *testing.T) {
		c.t = t
		if acmeCAPEM == "" {
			t.Fatal("the tenant CA was not created")
		}
		if st := c.want(c.do(http.MethodGet, edge+"/status", "", "t-edge-status", nil), 200, "status").json(t); st["mode"] != "managed" {
			t.Errorf("status = %v", st)
		}
		decl := map[string]any{
			"revision": 1, "slug": radsecTenantSlug, "port": radsecTenantPort,
			"authUpstream": "10.255.0.1:1812", "acctUpstream": "10.255.0.1:1813",
			"clientCas": []any{
				map[string]any{"id": acmeCA, "kind": "issued", "certificate": acmeCAPEM, "hasCrl": true},
				map[string]any{"id": broughtCA, "kind": "brought", "certificate": vendor.pem, "hasCrl": true},
			},
			"aps": []any{map[string]any{"name": "ap-001", "kind": "ap"}, map[string]any{"name": "hq", "kind": "group", "purpose": "unifi"}},
		}
		if got := c.want(c.do(http.MethodPut, tenant, key("edge-put"), "t-edge-put", decl), 201, "declare").json(t); got["pending"] != true {
			t.Errorf("declare = %v", got)
		}
		list := c.want(c.do(http.MethodGet, edge+"/tenants", "", "t-edge-list", nil), 200, "tenants").json(t)
		if items := list["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != radsecTenantID {
			t.Errorf("tenants = %v", list)
		}
		// 証明書が足りなければ反映できない（同期で返す）。
		if p := c.want(c.do(http.MethodPost, tenant+"/apply", key("edge-apply0"), "t-edge-apply0", nil), 422, "not ready").json(t); p["cause"] != "NOT_READY" || p["downstream"] != "edge:"+edgeID {
			t.Errorf("not ready = %v", p)
		}

		// CSR に radsec-ca が署名して置く（鍵はエッジの外に出ない）。
		sign := func(kind, profile string) {
			t.Helper()
			csr := c.want(c.do(http.MethodPost, tenant+"/"+kind+"-csr", key("edge-"+kind+"-csr"), "t-edge-"+kind+"-csr", nil), 200, kind+" CSR").json(t)["csr"]
			cert := c.want(c.do(http.MethodPost, "/ca/certs", key("ca-issue-"+kind), "t-ca-issue-"+kind,
				map[string]any{"profile": profile, "tenant": radsecTenantSlug, "edge": edgeID, "csr": csr}), 201, "issue "+profile).json(t)
			c.want(c.do(http.MethodPut, tenant+"/"+kind+"-cert", key("edge-"+kind+"-cert"), "t-edge-"+kind+"-cert",
				map[string]any{"certificate": cert["certificate"]}), 200, kind+" certificate")
		}
		sign("server", "tenant-server")
		sign("probe", "probe-client")

		// AP とグループの証明書。
		apKey, apCSR := newCSR(t)
		r := c.want(c.do(http.MethodPost, "/ca/certs", key("ca-issue-ap"), "t-ca-issue-ap",
			map[string]any{"profile": "ap-client", "tenant": radsecTenantSlug, "ap": "ap-001", "csr": apCSR}), 201, "issue AP")
		cert := r.json(t)
		apSerial, _ = cert["serial"].(string)
		if loc := r.header.Get("Location"); loc != "/admin/v2/ca/certs/"+apSerial {
			t.Errorf("Location = %q", loc)
		}
		ap = keyPair(t, cert["certificate"].(string), apKey)
		groupKey, groupCSR := newCSR(t)
		groupCert := c.want(c.do(http.MethodPost, "/ca/certs", key("ca-issue-group"), "t-ca-issue-group",
			map[string]any{"profile": "group-client", "tenant": radsecTenantSlug, "group": "hq", "csr": groupCSR}), 201, "issue group").json(t)
		if uri, _ := groupCert["sanUri"].(string); uri != "urn:"+radsecURNPrefix+":grp:"+radsecTenantSlug+":hq" {
			t.Errorf("group SAN = %q", uri)
		}
		group = keyPair(t, groupCert["certificate"].(string), groupKey)
		if got := c.want(c.do(http.MethodGet, "/ca/certs/"+apSerial, "", "t-ca-cert", nil), 200, "certificate").json(t); got["status"] != "valid" {
			t.Errorf("certificate = %v", got)
		}
		certs := c.want(c.do(http.MethodGet, "/ca/certs?tenant="+radsecTenantSlug+"&limit=10", "", "t-ca-certs", nil), 200, "certificates").json(t)
		if n := len(certs["items"].([]any)); n != 4 {
			t.Errorf("certificates = %d", n)
		}

		// 反映は裏で動くので、結果を待つ（足りない CRL は radsec-ca の配布の口から取る）。
		c.want(c.do(http.MethodPost, tenant+"/apply", key("edge-apply1"), "t-edge-apply1", nil), 202, "apply")
		var state map[string]any
		deadline := time.Now().Add(150 * time.Second)
		for {
			got := c.want(c.do(http.MethodGet, tenant, "", "t-edge-get", nil), 200, "tenant").json(t)
			state = got["apply"].(map[string]any)
			if state["state"] == "idle" && state["lastResult"] != nil {
				if state["lastResult"] != "ok" || got["appliedRevision"] != float64(1) || got["pending"] != false {
					logs := c.do(http.MethodGet, tenant+"/logs?lines=100", "", "t-edge-logs0", nil)
					t.Fatalf("apply = %v (tenant %v)\n%s", state, got, logs.body)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("apply did not finish: %v", state)
			}
			time.Sleep(2 * time.Second)
		}
	})

	t.Run("AP の接続と失効", func(t *testing.T) {
		c.t = t
		if apSerial == "" {
			t.Fatal("the AP certificate was not issued")
		}
		wantAccepted(t, "AP", ap, serverRoots)
		wantAccepted(t, "group", group, serverRoots)
		vendorAP := vendor.leaf(t, "aabbccddeeff")
		wantAccepted(t, "vendor AP", vendorAP, serverRoots)
		// 登録していない CA の証明書は断る。
		wantRejected(t, "unknown CA", newTestCA(t, "contract unknown CA").leaf(t, "aabbccddee00"), serverRoots)
		// radsecproxy が AP の名前で受け入れたことはログで分かる（docker logs は少し遅れる）。
		var logs string
		for range 20 {
			logs = string(c.want(c.do(http.MethodGet, tenant+"/logs?lines=200", "", "t-edge-logs", nil), 200, "logs").body)
			if strings.Contains(logs, "client "+radsecTenantSlug+"-ap-001,") {
				break
			}
			time.Sleep(time.Second)
		}
		if !strings.Contains(logs, "client "+radsecTenantSlug+"-ap-001,") {
			t.Errorf("the AP is not in the logs: %s", logs)
		}
		c.want(c.do(http.MethodGet, tenant+"/last-seen", "", "t-edge-seen", nil), 200, "last-seen")

		// 失効 → CRL の取り直し → 拒否。ほかの証明書は巻き込まれない。
		revoked := c.want(c.do(http.MethodPost, "/ca/certs/"+apSerial+"/revoke", key("ca-revoke"), "t-ca-revoke",
			map[string]any{"reason": "keyCompromise"}), 200, "revoke").json(t)
		if revoked["status"] != "revoked" {
			t.Errorf("revoke = %v", revoked)
		}
		refreshed := c.want(c.do(http.MethodPost, tenant+"/crl:refresh", key("edge-refresh"), "t-edge-refresh", nil), 200, "CRL refresh").json(t)
		if refreshed["ok"] != true {
			t.Errorf("CRL refresh = %v", refreshed)
		}
		wantRejected(t, "revoked AP", ap, serverRoots)
		wantAccepted(t, "group after revocation", group, serverRoots)
		wantAccepted(t, "vendor AP after revocation", vendorAP, serverRoots)
	})

	t.Run("届かないエッジと登録簿にないエッジ", func(t *testing.T) {
		c.t = t
		if p := c.want(c.do(http.MethodGet, "/edges/none/status", "", "t-edge-none", nil), 404, "unknown edge").json(t); p["cause"] != "EDGE_NOT_FOUND" || p["downstream"] != "edge:none" {
			t.Errorf("unknown edge = %v", p)
		}
		// パスの値の形は下流に送る前に確かめる。
		if p := c.want(c.do(http.MethodGet, edge+"/tenants/acme", "", "t-edge-bad", nil), 400, "bad tenantId").json(t); p["cause"] != "MANDATORY_IE_INCORRECT" {
			t.Errorf("bad tenantId = %v", p)
		}
		if p := c.want(c.do(http.MethodPost, tenant+"/apply/x", key("edge-denied"), "t-edge-denied", nil), 404, "not allowed").json(t); p["cause"] != "ROUTE_NOT_ALLOWED" || p["downstream"] != "edge:"+edgeID {
			t.Errorf("not allowed = %v", p)
		}
		if downID == "" {
			return
		}
		if p := c.want(c.do(http.MethodGet, "/edges/"+downID+"/status", "", "t-edge-down", nil), 503, "unreachable edge").json(t); p["cause"] != "DOWNSTREAM_UNAVAILABLE" || p["downstream"] != "edge:"+downID {
			t.Errorf("unreachable edge = %v", p)
		}
		st := c.want(c.do(http.MethodGet, "/status", "", "t-status2", nil), 200, "status").json(t)
		seen := map[string]map[string]any{}
		for _, d := range st["downstreams"].([]any) {
			m := d.(map[string]any)
			seen[m["name"].(string)] = m
		}
		if d := seen["edge:"+downID]; d == nil || d["reachable"] != false || d["error"] == nil {
			t.Errorf("unreachable edge in /status = %v", d)
		}
		d := seen["edge:"+edgeID]
		if d == nil || d["reachable"] != true || d["status"].(map[string]any)["mode"] != "managed" {
			t.Fatalf("edge in /status = %v", d)
		}
		// フィンガープリントで照合するエッジは、接続のときに受け取った証明書を出す。
		if cert, _ := d["serverCertificate"].(map[string]any); cert == nil || cert["subject"] == "" || cert["notAfter"] == nil {
			t.Errorf("edge certificate in /status = %v", d["serverCertificate"])
		}
		if ca := seen["ca"]; ca == nil || ca["reachable"] != true || ca["status"].(map[string]any)["service"] != "radsec-ca" {
			t.Errorf("ca in /status = %v", ca)
		}
	})

	t.Run("片付けの口", func(t *testing.T) {
		c.t = t
		c.want(c.do(http.MethodPost, tenant+"/restart", key("edge-restart"), "t-edge-restart", nil), 204, "restart")
		c.want(c.do(http.MethodDelete, tenant, key("edge-delete"), "t-edge-delete", nil), 204, "delete tenant")
		if p := c.want(c.do(http.MethodGet, tenant, "", "t-edge-gone", nil), 404, "gone").json(t); p["cause"] != "TENANT_NOT_FOUND" || p["downstream"] != "edge:"+edgeID {
			t.Errorf("gone = %v", p)
		}
		if got := c.want(c.do(http.MethodPost, "/ca/cas/"+broughtCA+"/disable", key("ca-disable"), "t-ca-disable", nil), 200, "disable").json(t); got["status"] != "disabled" {
			t.Errorf("disable = %v", got)
		}
	})

	t.Run("監査ログ", func(t *testing.T) {
		c.t = t
		// aka-provisioner の監査ログ。
		for _, a := range []struct{ action, traceID, target string }{
			{"ca.ca.create", "t-ca-create", acmeCA},
			{"ca.ca.create", "t-ca-brought", broughtCA},
			{"ca.ca.disable", "t-ca-disable", broughtCA},
			{"ca.crl.upload", "t-ca-crl-put", broughtCA},
			{"ca.crl_source.put", "t-ca-source", broughtCA},
			{"ca.cert.issue", "t-ca-issue-ap", apSerial},
			{"ca.cert.revoke", "t-ca-revoke", apSerial},
			{"edge.tenant.put", "t-edge-put", radsecTenantID},
			{"edge.tenant.delete", "t-edge-delete", radsecTenantID},
			{"edge.server_csr.create", "t-edge-server-csr", radsecTenantID},
			{"edge.server_cert.put", "t-edge-server-cert", radsecTenantID},
			{"edge.probe_csr.create", "t-edge-probe-csr", radsecTenantID},
			{"edge.probe_cert.put", "t-edge-probe-cert", radsecTenantID},
			{"edge.tenant.apply", "t-edge-apply1", radsecTenantID},
			{"edge.crl.refresh", "t-edge-refresh", radsecTenantID},
			{"edge.tenant.restart", "t-edge-restart", radsecTenantID},
		} {
			if !c.auditEntry(a.action, a.traceID, a.target) {
				t.Errorf("aka-provisioner audit log has no %s %s with %s", a.action, a.target, a.traceID)
			}
		}
		// 操作者とトレースID が下流の監査ログに残る。
		ca := auditFiles(t, os.Getenv("PROVISIONER_TEST_CA_AUDIT_DIR"))
		for _, traceID := range []string{"t-ca-create", "t-ca-brought", "t-ca-issue-ap", "t-ca-revoke"} {
			if !strings.Contains(ca, traceID) {
				t.Errorf("radsec-ca audit file has no %s", traceID)
			}
		}
		edgeAudit := auditFiles(t, os.Getenv("PROVISIONER_TEST_EDGE_AUDIT_DIR"))
		for _, traceID := range []string{"t-edge-put", "t-edge-apply1", "t-edge-delete"} {
			if !strings.Contains(edgeAudit, traceID) {
				t.Errorf("edge audit file has no %s", traceID)
			}
		}
		if !strings.Contains(ca, "contract-op") || !strings.Contains(edgeAudit, "contract-op") {
			t.Error("the operator is not in the downstream audit files")
		}

		// Meraki の API キーの値は aka-provisioner のログにも監査ログにも残らない。
		work := os.Getenv("PROVISIONER_TEST_WORK")
		logs, err := os.ReadFile(filepath.Join(work, "aka-provisioner.log"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(logs), merakiKey) {
			t.Error("the Meraki API key is in the log of aka-provisioner")
		}
		files, err := filepath.Glob(filepath.Join(work, "prov-audit", "*"))
		if err != nil || len(files) == 0 {
			t.Fatalf("audit files of aka-provisioner: %v %v", files, err)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), merakiKey) {
				t.Errorf("the Meraki API key is in %s", f)
			}
		}
		if strings.Contains(ca, merakiKey) {
			t.Error("the Meraki API key is in the audit file of radsec-ca")
		}
		out := c.want(c.do(http.MethodGet, "/audit-logs?limit=500", "", "t-audit", nil), 200, "audit logs").body
		if strings.Contains(string(out), merakiKey) {
			t.Error("the Meraki API key is in the audit log of aka-provisioner")
		}
	})
}

// auditEntry は aka-provisioner の監査ログに action・traceId・target の組があるかを返す。
func (c *client) auditEntry(action, traceID, target string) bool {
	c.t.Helper()
	out := c.want(c.do(http.MethodGet, "/audit-logs?limit=500", "", "contract-audit", nil), 200, "audit logs").json(c.t)
	for _, it := range out["items"].([]any) {
		e := it.(map[string]any)
		if e["action"] == action && e["traceId"] == traceID && e["target"] == target && e["operator"] == "contract-op" {
			return true
		}
	}
	return false
}
