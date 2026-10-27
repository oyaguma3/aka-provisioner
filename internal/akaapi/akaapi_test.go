package akaapi

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/downstream/downstreamtest"
)

func TestStatusAndAVClient(t *testing.T) {
	srv := downstreamtest.NewServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/v1/status":
			downstreamtest.WriteJSON(w, 200, map[string]any{
				"version": "1.0.0", "bootId": "b", "startedAt": "2026-10-09T00:00:00Z",
				"subscriberCount": 7, "clientCount": 2, "avPlainEnabled": false,
				"avServerCertificateNotAfter": "2036-10-01T00:00:00Z",
			})
		case "/admin/v1/clients/1":
			downstreamtest.WriteJSON(w, 200, map[string]any{
				"id": 1, "name": "vector-gateway", "enabled": true, "networkName": "",
				"notAfter": "2028-10-01T00:00:00Z",
			})
		default:
			downstreamtest.WriteProblem(w, 404, `{"title":"Not Found","status":404,"cause":"CLIENT_NOT_FOUND"}`)
		}
	})
	c, err := New(srv.Options)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() != downstream.Aka {
		t.Errorf("name = %s", c.Name())
	}
	st, err := c.Status(t.Context())
	if err != nil || st.Version != "1.0.0" || st.SubscriberCount != 7 {
		t.Errorf("status = %+v, %v", st, err)
	}
	av, err := c.GetAVClient(t.Context(), 1)
	if err != nil || av.ID != 1 || av.Name != "vector-gateway" || !av.Enabled {
		t.Errorf("av client = %+v, %v", av, err)
	}
	_, err = c.GetAVClient(t.Context(), 9)
	if apiErr, ok := errors.AsType[*downstream.Error](err); !ok || apiErr.Status != 404 || downstream.CauseOf(err) != CauseClientNotFound {
		t.Errorf("missing av client: %v", err)
	}
}

func TestDiagnose(t *testing.T) {
	// 文面が aka-vecgen の設定と手順を指していること（判定そのものは internal/downstream で確かめる）。
	dnsErr := fmt.Errorf("wrap: %w", &net.DNSError{Name: "aka-vecgen", IsNotFound: true})
	if got := Diagnose(dnsErr); !strings.Contains(got, "（aka-vecgen）") || !strings.Contains(got, "aka-av") {
		t.Errorf("dns: %q", got)
	}
	if !strings.Contains(hints.UnknownServer, "PROVISIONER_AKA_SERVER_CERT") || !strings.Contains(hints.ClientRejected, "VECGEN_ADMIN_CLIENTS") {
		t.Errorf("hints = %+v", hints)
	}
}

func TestSubscriberRequests(t *testing.T) {
	type req struct{ method, path, contentType, body string }
	var got []req
	srv := downstreamtest.NewServer(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		got = append(got, req{r.Method, r.URL.Path, r.Header.Get("Content-Type"), string(b)})
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/keys"):
			downstreamtest.WriteJSON(w, 200, map[string]any{"ki": "00", "opc": "11"})
		case r.URL.Path == "/admin/v1/subscribers" && r.Method == http.MethodGet:
			downstreamtest.WriteJSON(w, 200, map[string]any{"items": []any{}, "total": 0})
		default:
			downstreamtest.WriteJSON(w, 200, map[string]any{
				"imsi": "001020000000001", "sqn": "000000000000", "amf": "8000", "sqnType": "inc32",
				"allowPlain": false, "allowedClientIds": []int{1}, "createdAt": "2026-10-10T00:00:00Z", "updatedAt": "2026-10-10T00:00:00Z",
			})
		}
	})
	c, err := New(srv.Options)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := c.ListSubscribers(ctx, downstream.ListParams{Prefix: "00102"}); err != nil {
		t.Fatal(err)
	}
	s, err := c.CreateSubscriber(ctx, SubscriberCreate{IMSI: "001020000000001", Ki: "aa", OPc: "bb", AllowedClientIDs: []int64{1}})
	if err != nil || s.SQNType != SQNTypeInc32 || !slices.Equal(s.AllowedClientIDs, []int64{1}) {
		t.Errorf("create = %+v, %v", s, err)
	}
	plain := true
	if _, err := c.UpdateSubscriber(ctx, "001020000000001", SubscriberUpdate{AllowPlain: &plain}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetSubscriber(ctx, "001020000000001"); err != nil {
		t.Fatal(err)
	}
	if k, err := c.GetSubscriberKeys(ctx, "001020000000001"); err != nil || k.Ki != "00" {
		t.Errorf("keys = %+v, %v", k, err)
	}
	if err := c.DeleteSubscriber(ctx, "001020000000001"); err != nil {
		t.Fatal(err)
	}
	want := []req{
		{"GET", "/admin/v1/subscribers", "", ""},
		// 既定値に任せる項目（SQN、AMF、SQN 増加タイプ）は送らない。許可フラグと許可クライアントは送る。
		{"POST", "/admin/v1/subscribers", "application/json", `{"imsi":"001020000000001","ki":"aa","opc":"bb","allowPlain":false,"allowedClientIds":[1]}`},
		{"PATCH", "/admin/v1/subscribers/001020000000001", "application/merge-patch+json", `{"allowPlain":true}`},
		{"GET", "/admin/v1/subscribers/001020000000001", "", ""},
		{"GET", "/admin/v1/subscribers/001020000000001/keys", "", ""},
		{"DELETE", "/admin/v1/subscribers/001020000000001", "", ""},
	}
	if !slices.Equal(got, want) {
		t.Errorf("requests =\n%v\nwant\n%v", got, want)
	}
}
