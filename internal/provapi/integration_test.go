package provapi

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/downstream/downstreamtest"
	"github.com/oyaguma3/aka-provisioner/internal/trace"
)

// 実際の provisioning-api を相手にした契約テスト。接続先は downstreamtest.IntegrationOptions を参照。
// テスト用の加入者・認可ポリシー（IMSI 00101 で始まるテスト用の番号）と、RADIUSクライアント
// （文書用のアドレス 198.51.100.0/24）を作り、終わったら削除する。

func newIntegrationClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(downstreamtest.IntegrationOptions(t, "PROV"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// integrationCtx は、操作者と新しいトレースID を入れたコンテキストと、そのトレースID を返す。
func integrationCtx(t *testing.T) (context.Context, string) {
	id := trace.New()
	return downstream.WithOperator(trace.With(t.Context(), id), downstreamtest.IntegrationOperator), id
}

// cleanup は、テストの終わりに加入者と認可ポリシーを削除する（削除済みなら何もしない）。
// t.Context() は後片付けの前にキャンセルされるので、ここでは使わない。
func cleanup(t *testing.T, c *Client, imsi string) {
	t.Cleanup(func() {
		ctx := downstream.WithOperator(context.Background(), "it-cleanup")
		if err := c.DeleteSubscriber(ctx, imsi); err != nil && downstream.CauseOf(err) != CauseUserNotFound {
			t.Errorf("cleanup subscriber %s: %v", imsi, err)
		}
		if err := c.DeletePolicy(ctx, imsi); err != nil && downstream.CauseOf(err) != CausePolicyNotFound {
			t.Errorf("cleanup policy %s: %v", imsi, err)
		}
	})
}

func wantCause(t *testing.T, err error, status int, cause string) *downstream.Error {
	t.Helper()
	apiErr, ok := errors.AsType[*downstream.Error](err)
	if !ok || apiErr.Status != status || apiErr.Problem.Cause != cause {
		t.Fatalf("err = %v, want %d %s", err, status, cause)
	}
	return apiErr
}

func TestIntegrationStatus(t *testing.T) {
	c := newIntegrationClient(t)
	st, err := c.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// 0.3.0 以降の provisioning-api（provisioner が前提にする版）は sessionCount を返す。
	if st.Version == "" || st.StartedAt.IsZero() || st.SessionCount == nil {
		t.Errorf("status = %+v", st)
	}
}

func TestIntegrationSubscribers(t *testing.T) {
	c := newIntegrationClient(t)
	imsi := downstreamtest.TestIMSI()
	cleanup(t, c, imsi)

	ctx, createTrace := integrationCtx(t)
	created, err := c.CreateSubscriber(ctx, SubscriberCreate{IMSI: imsi, Ki: "465B5CE8B199B49FAA5F0A2EE238A6BC", OPc: "cd63cb71954a9f4e48a5994e37a02baf"})
	if err != nil {
		t.Fatal(err)
	}
	if created.IMSI != imsi || created.AMF != "8000" || created.SQN != "000000000000" || created.CreatedAt.IsZero() {
		t.Errorf("created = %+v", created)
	}
	downstreamtest.CheckAudit(t, c.Client, createTrace, "subscriber.create", imsi)

	// 同じ IMSI はもう作れない（provisioner は存在確認の後の競合をこれで知る）。
	ctx, _ = integrationCtx(t)
	_, err = c.CreateSubscriber(ctx, SubscriberCreate{IMSI: imsi, Ki: strings.Repeat("0", 32), OPc: strings.Repeat("0", 32)})
	wantCause(t, err, http.StatusConflict, CauseSubscriberExists)

	// 入力の誤りは項目つきで返る（provisioner は param を付け替えて返す）。
	_, err = c.CreateSubscriber(ctx, SubscriberCreate{IMSI: downstreamtest.TestIMSI(), Ki: "xyz", OPc: strings.Repeat("0", 32)})
	if apiErr := wantCause(t, err, http.StatusBadRequest, "MANDATORY_IE_INCORRECT"); len(apiErr.Problem.InvalidParams) != 1 || apiErr.Problem.InvalidParams[0].Param != "ki" {
		t.Errorf("invalidParams = %+v", apiErr.Problem.InvalidParams)
	}

	// 指定した項目だけを変える。SQN は指定しなければ変わらない。16 進は小文字で返る。
	amf := "B9B9"
	updated, err := c.UpdateSubscriber(ctx, imsi, SubscriberUpdate{AMF: &amf})
	if err != nil || updated.AMF != "b9b9" || updated.SQN != "000000000000" {
		t.Errorf("updated = %+v, %v", updated, err)
	}
	ctx, keysTrace := integrationCtx(t)
	keys, err := c.GetSubscriberKeys(ctx, imsi)
	if err != nil || keys.Ki != "465b5ce8b199b49faa5f0a2ee238a6bc" || keys.OPc != "cd63cb71954a9f4e48a5994e37a02baf" {
		t.Errorf("keys = %+v, %v", keys, err)
	}
	downstreamtest.CheckAudit(t, c.Client, keysTrace, "subscriber.keys.read", imsi)

	l, err := c.ListSubscribers(t.Context(), downstream.ListParams{Prefix: imsi})
	if err != nil || l.Total != 1 || len(l.Items) != 1 || l.Items[0].IMSI != imsi || l.NextCursor != "" {
		t.Errorf("list = %+v, %v", l, err)
	}

	if err := c.DeleteSubscriber(ctx, imsi); err != nil {
		t.Fatal(err)
	}
	_, err = c.GetSubscriber(t.Context(), imsi)
	wantCause(t, err, http.StatusNotFound, CauseUserNotFound)
	// 既にないものの削除は 404（provisioner は削除済みとして扱う）。
	wantCause(t, c.DeleteSubscriber(ctx, imsi), http.StatusNotFound, CauseUserNotFound)
}

func TestIntegrationPolicies(t *testing.T) {
	c := newIntegrationClient(t)
	imsi := downstreamtest.TestIMSI()
	cleanup(t, c, imsi)

	ctx, putTrace := integrationCtx(t)
	put := PolicyPut{Default: "deny", Rules: []PolicyRule{{NASID: "AP-IT-01", AllowedSSIDs: []string{"CORP"}, VLANID: "100", SessionTimeout: 3600}}}
	p, created, err := c.PutPolicy(ctx, imsi, put)
	// 新規は active（0.4.0 以降の provisioning-api）。
	if err != nil || !created || p.IMSI != imsi || p.Default != "deny" || len(p.Rules) != 1 || p.Rules[0].VLANID != "100" || p.Status != PolicyActive {
		t.Fatalf("put (create) = %+v, %v, %v", p, created, err)
	}
	downstreamtest.CheckAudit(t, c.Client, putTrace, "policy.create", imsi)

	// 停止（provisioner は PUT /policies/{imsi}/status を中継する）。応答は変更後の認可ポリシー全体。
	statusCtx, suspendTrace := integrationCtx(t)
	setStatus := func(ctx context.Context, body string) downstream.RelayResponse {
		t.Helper()
		resp, err := c.Relay(ctx, downstream.RelayRequest{Method: http.MethodPut, Path: []string{"policies", imsi, "status"},
			Body: []byte(body), ContentType: "application/json"})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := setStatus(statusCtx, `{"status":"suspended"}`)
	var sp Policy
	if resp.Status != http.StatusOK || json.Unmarshal(resp.Body, &sp) != nil || sp.IMSI != imsi || sp.Status != PolicySuspended || sp.Default != "deny" {
		t.Fatalf("suspend: %+v (%s)", resp, resp.Body)
	}
	downstreamtest.CheckAudit(t, c.Client, suspendTrace, "policy.suspend", imsi)
	// 不正な値は 400（provisioner は検証せずに中継する）。
	if resp := setStatus(statusCtx, `{"status":"paused"}`); resp.Status != http.StatusBadRequest || !strings.Contains(string(resp.Body), `"cause"`) {
		t.Errorf("bad status: %+v (%s)", resp, resp.Body)
	}

	// 置き換え（全体）。作成でないことが分かる。状態は変わらない（provisioner の補償はこれを前提にする）。
	put = PolicyPut{Default: "allow", Rules: []PolicyRule{}}
	if p, created, err = c.PutPolicy(ctx, imsi, put); err != nil || created || p.Default != "allow" || len(p.Rules) != 0 || p.Status != PolicySuspended {
		t.Errorf("put (replace) = %+v, %v, %v", p, created, err)
	}
	if got, err := c.GetPolicy(t.Context(), imsi); err != nil || got.Default != "allow" || got.Status != PolicySuspended {
		t.Errorf("get = %+v, %v", got, err)
	}
	if l, err := c.ListPolicies(t.Context(), downstream.ListParams{Prefix: imsi}); err != nil || l.Total != 1 || l.Items[0].IMSI != imsi ||
		l.Items[0].Status != PolicySuspended {
		t.Errorf("list = %+v, %v", l, err)
	}

	// 再開。値は前後の空白を除き小文字にして受け付ける。
	statusCtx, resumeTrace := integrationCtx(t)
	if resp := setStatus(statusCtx, `{"status":" Active "}`); resp.Status != http.StatusOK || !strings.Contains(string(resp.Body), `"status":"active"`) {
		t.Errorf("resume: %+v (%s)", resp, resp.Body)
	}
	downstreamtest.CheckAudit(t, c.Client, resumeTrace, "policy.resume", imsi)
	// 同じ状態への変更も 200。
	if resp := setStatus(statusCtx, `{"status":"active"}`); resp.Status != http.StatusOK {
		t.Errorf("same status: %+v (%s)", resp, resp.Body)
	}

	// ルールの誤りは位置つきで返る。
	_, _, err = c.PutPolicy(ctx, imsi, PolicyPut{Default: "deny", Rules: []PolicyRule{{NASID: "*", AllowedSSIDs: []string{}}}})
	if apiErr := wantCause(t, err, http.StatusBadRequest, "MANDATORY_IE_INCORRECT"); !slices.ContainsFunc(apiErr.Problem.InvalidParams,
		func(p downstream.InvalidParam) bool { return strings.HasPrefix(p.Param, "rules[0].") }) {
		t.Errorf("invalidParams = %+v", apiErr.Problem.InvalidParams)
	}

	if err := c.DeletePolicy(ctx, imsi); err != nil {
		t.Fatal(err)
	}
	_, err = c.GetPolicy(t.Context(), imsi)
	wantCause(t, err, http.StatusNotFound, CausePolicyNotFound)
	wantCause(t, c.DeletePolicy(ctx, imsi), http.StatusNotFound, CausePolicyNotFound)
	// 認可ポリシーがなければ停止できない。
	if resp := setStatus(statusCtx, `{"status":"suspended"}`); resp.Status != http.StatusNotFound || !strings.Contains(string(resp.Body), CausePolicyNotFound) {
		t.Errorf("suspend without policy: %+v (%s)", resp, resp.Body)
	}
}

// TestIntegrationRelay は、provisioner が中継する操作（RADIUSクライアント、セッション、監査ログ）を Relay で確かめる。
func TestIntegrationRelay(t *testing.T) {
	c := newIntegrationClient(t)
	ctx, createTrace := integrationCtx(t)
	ip := fmt.Sprintf("198.51.100.%d", 1+time.Now().UnixNano()%250)
	relay := func(method string, path []string, query, body string) downstream.RelayResponse {
		t.Helper()
		req := downstream.RelayRequest{Method: method, Path: path, RawQuery: query}
		if body != "" {
			req.Body, req.ContentType = []byte(body), "application/json"
		}
		resp, err := c.Relay(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := relay(http.MethodPost, []string{"clients"}, "", `{"ip":"`+ip+`","secret":"it-secret-1","name":"it-ap"}`)
	if resp.Status != http.StatusCreated || !strings.HasPrefix(resp.Location, "/admin/v1/clients/") {
		t.Fatalf("create client: %+v (%s)", resp, resp.Body)
	}
	var client struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(resp.Body, &client); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprint(client.ID)
	t.Cleanup(func() {
		c.Relay(downstream.WithOperator(context.Background(), "it-cleanup"), downstream.RelayRequest{Method: http.MethodDelete, Path: []string{"clients", id}})
	})
	downstreamtest.CheckAudit(t, c.Client, createTrace, "client.create", id)

	// 下流のエラーもそのまま返る。
	if resp := relay(http.MethodPost, []string{"clients"}, "", `{"ip":"`+ip+`","secret":"x","name":"dup"}`); resp.Status != http.StatusConflict ||
		resp.ContentType != "application/problem+json" || !strings.Contains(string(resp.Body), "CLIENT_ALREADY_EXISTS") {
		t.Errorf("duplicate: %+v (%s)", resp, resp.Body)
	}
	if resp := relay(http.MethodGet, []string{"clients"}, "ip="+ip, ""); resp.Status != http.StatusOK || !strings.Contains(string(resp.Body), `"id":`+id) {
		t.Errorf("find by ip: %+v (%s)", resp, resp.Body)
	}
	if resp := relay(http.MethodGet, []string{"clients", id, "secret"}, "", ""); resp.Status != http.StatusOK || !strings.Contains(string(resp.Body), "it-secret-1") {
		t.Errorf("secret: %+v", resp.Status)
	}
	if resp := relay(http.MethodPatch, []string{"clients", id}, "", `{"name":"it-ap-2"}`); resp.Status != http.StatusOK || !strings.Contains(string(resp.Body), "it-ap-2") {
		t.Errorf("update: %+v (%s)", resp, resp.Body)
	}
	if resp := relay(http.MethodDelete, []string{"clients", id}, "", ""); resp.Status != http.StatusNoContent {
		t.Errorf("delete: %+v (%s)", resp, resp.Body)
	}
	if resp := relay(http.MethodGet, []string{"clients", id}, "", ""); resp.Status != http.StatusNotFound || !strings.Contains(string(resp.Body), "CLIENT_NOT_FOUND") {
		t.Errorf("deleted: %+v (%s)", resp, resp.Body)
	}

	if resp := relay(http.MethodGet, []string{"sessions"}, "limit=1", ""); resp.Status != http.StatusOK || !strings.Contains(string(resp.Body), `"total"`) {
		t.Errorf("sessions: %+v (%s)", resp, resp.Body)
	}
	if resp := relay(http.MethodGet, []string{"sessions"}, "limit=0", ""); resp.Status != http.StatusBadRequest || !strings.Contains(string(resp.Body), "INVALID_QUERY_PARAM") {
		t.Errorf("sessions bad query: %+v (%s)", resp, resp.Body)
	}
}
