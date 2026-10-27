package provapi

import (
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/oyaguma3/aka-provisioner/internal/downstream"
	"github.com/oyaguma3/aka-provisioner/internal/downstream/downstreamtest"
)

func TestStatus(t *testing.T) {
	srv := downstreamtest.NewServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/v1/status" {
			http.NotFound(w, r)
			return
		}
		downstreamtest.WriteJSON(w, 200, map[string]any{
			"version": "0.3.0", "nodeName": "poc-01", "startedAt": "2026-10-09T00:00:00Z",
			"subscriberCount": 3, "clientCount": 2, "policyCount": 4, "sessionCount": 1,
		})
	})
	c, err := New(srv.Options)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() != downstream.Prov {
		t.Errorf("name = %s", c.Name())
	}
	st, err := c.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != "0.3.0" || st.NodeName != "poc-01" || st.SubscriberCount != 3 || st.PolicyCount != 4 ||
		st.SessionCount == nil || *st.SessionCount != 1 {
		t.Errorf("status = %+v", st)
	}
}

func TestDiagnose(t *testing.T) {
	// 文面が provisioning-api の設定と手順を指していること（判定そのものは internal/downstream で確かめる）。
	dnsErr := fmt.Errorf("wrap: %w", &net.DNSError{Name: "provisioning-api", IsNotFound: true})
	if got := Diagnose(dnsErr); !strings.Contains(got, "（provisioning-api）") || !strings.Contains(got, "eapaka-prov") {
		t.Errorf("dns: %q", got)
	}
	for name, h := range map[string]string{
		"unknown server":  hints.UnknownServer,
		"client rejected": hints.ClientRejected,
		"reset":           hints.ConnectionReset,
	} {
		if !strings.Contains(h, "PROVISIONER_PROV_SERVER_CERT") && !strings.Contains(h, "PROVISIONING_API_ADMIN_CLIENTS") {
			t.Errorf("%s: %q", name, h)
		}
	}
}

func TestSubscriberAndPolicyRequests(t *testing.T) {
	type req struct{ method, path, query, contentType, body string }
	var got []req
	srv := downstreamtest.NewServer(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		got = append(got, req{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type"), string(b)})
		switch {
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/001010000000001"):
			downstreamtest.WriteJSON(w, 201, map[string]any{"imsi": "001010000000001", "default": "deny", "rules": []any{}})
		case r.Method == http.MethodPut:
			downstreamtest.WriteJSON(w, 200, map[string]any{"imsi": "001010000000002", "default": "allow", "rules": []any{}})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/keys"):
			downstreamtest.WriteJSON(w, 200, map[string]any{"ki": "00", "opc": "11"})
		case strings.HasSuffix(r.URL.Path, "/subscribers") && r.Method == http.MethodGet:
			downstreamtest.WriteJSON(w, 200, map[string]any{"items": []any{map[string]any{"imsi": "001010000000001", "amf": "8000", "sqn": "000000000000"}}, "total": 1})
		case strings.HasSuffix(r.URL.Path, "/policies") && r.Method == http.MethodGet:
			downstreamtest.WriteJSON(w, 200, map[string]any{"items": []any{}, "total": 0})
		default:
			downstreamtest.WriteJSON(w, 200, map[string]any{"imsi": "001010000000001", "amf": "b9b9", "sqn": "000000000020"})
		}
	})
	c, err := New(srv.Options)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if l, err := c.ListSubscribers(ctx, downstream.ListParams{Prefix: "00101", Limit: 10}); err != nil || l.Total != 1 || l.Items[0].AMF != "8000" {
		t.Errorf("list = %+v, %v", l, err)
	}
	if _, err := c.CreateSubscriber(ctx, SubscriberCreate{IMSI: "001010000000001", Ki: "aa", OPc: "bb"}); err != nil {
		t.Fatal(err)
	}
	amf := "b9b9"
	if s, err := c.UpdateSubscriber(ctx, "001010000000001", SubscriberUpdate{AMF: &amf}); err != nil || s.AMF != "b9b9" {
		t.Errorf("update = %+v, %v", s, err)
	}
	if k, err := c.GetSubscriberKeys(ctx, "001010000000001"); err != nil || k.Ki != "00" || k.OPc != "11" {
		t.Errorf("keys = %+v, %v", k, err)
	}
	if err := c.DeleteSubscriber(ctx, "001010000000001"); err != nil {
		t.Fatal(err)
	}
	put := PolicyPut{Default: "deny", Rules: []PolicyRule{{NASID: "*", AllowedSSIDs: []string{"CORP"}}}}
	if p, created, err := c.PutPolicy(ctx, "001010000000001", put); err != nil || !created || p.Default != "deny" {
		t.Errorf("put (create) = %+v, %v, %v", p, created, err)
	}
	if p, created, err := c.PutPolicy(ctx, "001010000000002", put); err != nil || created || p.Default != "allow" {
		t.Errorf("put (replace) = %+v, %v, %v", p, created, err)
	}
	if _, err := c.ListPolicies(ctx, downstream.ListParams{}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeletePolicy(ctx, "001010000000001"); err != nil {
		t.Fatal(err)
	}

	want := []req{
		{"GET", "/admin/v1/subscribers", "limit=10&prefix=00101", "", ""},
		{"POST", "/admin/v1/subscribers", "", "application/json", `{"imsi":"001010000000001","ki":"aa","opc":"bb"}`},
		// JSON Merge Patch で、指定した項目だけを送る。
		{"PATCH", "/admin/v1/subscribers/001010000000001", "", "application/merge-patch+json", `{"amf":"b9b9"}`},
		{"GET", "/admin/v1/subscribers/001010000000001/keys", "", "", ""},
		{"DELETE", "/admin/v1/subscribers/001010000000001", "", "", ""},
		{"PUT", "/admin/v1/policies/001010000000001", "", "application/json", `{"default":"deny","rules":[{"nasId":"*","allowedSsids":["CORP"]}]}`},
		{"PUT", "/admin/v1/policies/001010000000002", "", "application/json", `{"default":"deny","rules":[{"nasId":"*","allowedSsids":["CORP"]}]}`},
		{"GET", "/admin/v1/policies", "", "", ""},
		{"DELETE", "/admin/v1/policies/001010000000001", "", "", ""},
	}
	if !slices.Equal(got, want) {
		t.Errorf("requests =\n%v\nwant\n%v", got, want)
	}
}
