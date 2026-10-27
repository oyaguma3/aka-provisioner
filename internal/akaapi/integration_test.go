package akaapi

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

	"github.com/oyaguma3/aka-provisioner/internal/certs"
	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/downstream/downstreamtest"
	"github.com/oyaguma3/aka-provisioner/internal/trace"
)

// 実際の aka-vecgen の管理API を相手にした契約テスト。接続先は downstreamtest.IntegrationOptions を参照。
// テスト用の加入者（IMSI 00101 で始まるテスト用の番号）と AVクライアントを作り、終わったら削除する。

func newIntegrationClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(downstreamtest.IntegrationOptions(t, "AKA"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func integrationCtx(t *testing.T) (context.Context, string) {
	id := trace.New()
	return downstream.WithOperator(trace.With(t.Context(), id), downstreamtest.IntegrationOperator), id
}

func cleanupCtx() context.Context {
	return downstream.WithOperator(context.Background(), "it-cleanup")
}

func wantCause(t *testing.T, err error, status int, cause string) {
	t.Helper()
	apiErr, ok := errors.AsType[*downstream.Error](err)
	if !ok || apiErr.Status != status || apiErr.Problem.Cause != cause {
		t.Fatalf("err = %v, want %d %s", err, status, cause)
	}
}

// addAVClient は、vector-gateway の代わりの AVクライアントを登録し、テストの終わりに削除する。
// provisioner は AVクライアントを登録しないので、Relay で管理API を直接呼ぶ。
func addAVClient(t *testing.T, c *Client) AVClient {
	t.Helper()
	certPEM, _, err := certs.SelfSignedClient("it-vector-gateway", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"name": "it-vector-gateway", "certPem": string(certPEM)})
	ctx, _ := integrationCtx(t)
	resp, err := c.Relay(ctx, downstream.RelayRequest{Method: http.MethodPost, Path: []string{"clients"}, Body: body, ContentType: "application/json"})
	if err != nil || resp.Status != http.StatusCreated {
		t.Fatalf("add av client: %+v (%s), %v", resp, resp.Body, err)
	}
	var av AVClient
	if err := json.Unmarshal(resp.Body, &av); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Relay(cleanupCtx(), downstream.RelayRequest{Method: http.MethodDelete, Path: []string{"clients", fmt.Sprint(av.ID)}})
	})
	return av
}

func TestIntegrationStatus(t *testing.T) {
	c := newIntegrationClient(t)
	if st, err := c.Status(t.Context()); err != nil || st.Version == "" || st.StartedAt.IsZero() {
		t.Errorf("status = %+v, %v", st, err)
	}
}

func TestIntegrationAVClient(t *testing.T) {
	c := newIntegrationClient(t)
	av := addAVClient(t, c)
	got, err := c.GetAVClient(t.Context(), av.ID)
	if err != nil || got.ID != av.ID || got.Name != "it-vector-gateway" || !got.Enabled || got.FingerprintSHA256 == "" {
		t.Errorf("av client = %+v, %v", got, err)
	}
	_, err = c.GetAVClient(t.Context(), 1<<40)
	wantCause(t, err, http.StatusNotFound, CauseClientNotFound)
}

func TestIntegrationSubscribers(t *testing.T) {
	c := newIntegrationClient(t)
	av := addAVClient(t, c)
	imsi := downstreamtest.TestIMSI()
	t.Cleanup(func() {
		if err := c.DeleteSubscriber(cleanupCtx(), imsi); err != nil && downstream.CauseOf(err) != CauseUserNotFound {
			t.Errorf("cleanup subscriber %s: %v", imsi, err)
		}
	})

	ctx, createTrace := integrationCtx(t)
	created, err := c.CreateSubscriber(ctx, SubscriberCreate{
		IMSI: imsi, Ki: "465B5CE8B199B49FAA5F0A2EE238A6BC", OPc: "cd63cb71954a9f4e48a5994e37a02baf",
		SQNType: SQNTypeInc33, AllowedClientIDs: []int64{av.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.IMSI != imsi || created.SQN != "000000000000" || created.AMF != "8000" || created.SQNType != SQNTypeInc33 ||
		created.AllowPlain || !slices.Equal(created.AllowedClientIDs, []int64{av.ID}) || created.CreatedAt.IsZero() {
		t.Errorf("created = %+v", created)
	}
	downstreamtest.CheckAudit(t, c.Client, createTrace, "subscriber.create", imsi)

	ctx, _ = integrationCtx(t)
	_, err = c.CreateSubscriber(ctx, SubscriberCreate{IMSI: imsi, Ki: strings.Repeat("0", 32), OPc: strings.Repeat("0", 32), AllowedClientIDs: []int64{}})
	wantCause(t, err, http.StatusConflict, CauseSubscriberExists)

	// 存在しない許可クライアントは 400 の CLIENT_NOT_FOUND（provisioner は設定の誤りとして 502 にする）。
	_, err = c.CreateSubscriber(ctx, SubscriberCreate{IMSI: downstreamtest.TestIMSI(), Ki: strings.Repeat("0", 32), OPc: strings.Repeat("0", 32),
		AllowedClientIDs: []int64{1 << 40}})
	wantCause(t, err, http.StatusBadRequest, CauseClientNotFound)

	// 指定した項目だけを変える。許可クライアントは送らないので変わらない。
	plain := true
	updated, err := c.UpdateSubscriber(ctx, imsi, SubscriberUpdate{AllowPlain: &plain})
	if err != nil || !updated.AllowPlain || !slices.Equal(updated.AllowedClientIDs, []int64{av.ID}) || updated.SQNType != SQNTypeInc33 {
		t.Errorf("updated = %+v, %v", updated, err)
	}
	ctx, keysTrace := integrationCtx(t)
	if keys, err := c.GetSubscriberKeys(ctx, imsi); err != nil || keys.Ki != "465b5ce8b199b49faa5f0a2ee238a6bc" {
		t.Errorf("keys = %+v, %v", keys, err)
	}
	downstreamtest.CheckAudit(t, c.Client, keysTrace, "subscriber.keys.read", imsi)

	if l, err := c.ListSubscribers(t.Context(), downstream.ListParams{Prefix: imsi}); err != nil || l.Total != 1 || l.Items[0].IMSI != imsi {
		t.Errorf("list = %+v, %v", l, err)
	}

	if err := c.DeleteSubscriber(ctx, imsi); err != nil {
		t.Fatal(err)
	}
	_, err = c.GetSubscriber(t.Context(), imsi)
	wantCause(t, err, http.StatusNotFound, CauseUserNotFound)
	wantCause(t, c.DeleteSubscriber(ctx, imsi), http.StatusNotFound, CauseUserNotFound)
}
