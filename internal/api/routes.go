package api

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/oyaguma3/aka-provisioner/internal/downstream"
)

// kind は下流の種類。allowlist のまとまりで、監査ログの action の前半。エッジは全台で同じ口の一覧を使う。
type kind string

const (
	kindVecgen kind = "vecgen"
	kindRadius kind = "radius"
	kindCA     kind = "ca"
	kindEdge   kind = "edge"
)

// kinds は中継する下流の種類。
var kinds = []kind{kindVecgen, kindRadius, kindCA, kindEdge}

// pathPrefix は aka-provisioner のパスの、基底（/admin/v2）の後ろの部分。
func (k kind) pathPrefix() string {
	if k == kindEdge {
		return "/edges/{edgeId}"
	}
	return "/" + string(k)
}

// radsec は radsec-ca かエッジか。この 2 つはパスに接頭辞がなく、Idempotency-Key を受け取る（P2・P7）。
func (k kind) radsec() bool { return k == kindCA || k == kindEdge }

// name は要求の下流の名前を返す。
func (k kind) name(r *http.Request) downstream.Name {
	if k == kindEdge {
		return downstream.EdgeName(r.PathValue("edgeId"))
	}
	return downstream.Name(k)
}

// route は中継を許す 1 つの口（設計概要 §6.2。段階 3-1 の U2、段階 4-2 の P5）。
type route struct {
	// method は HTTP のメソッド。
	method string
	// path は下流の基底 URL より後ろのパス。{imsi}・{clientId}・{caId}・{serial}・{tenantId} はパスの値。
	path string
	// action は下流を書き換える口の監査ログの action の後半（例 subscriber.create）。空なら読み出し
	// （Idempotency-Key を求めず、監査ログにも残さない）。
	action string
}

// write は下流を書き換える口か。
func (rt route) write() bool { return rt.action != "" }

// targetParam は監査ログの target にするパスの値の名前を返す（口のパスの最後の {名前}。P10）。なければ空文字で、Location から取る。
func (rt route) targetParam() string {
	name := ""
	for seg := range strings.SplitSeq(rt.path, "/") {
		if v, ok := strings.CutPrefix(seg, "{"); ok {
			name = strings.TrimSuffix(v, "}")
		}
	}
	return name
}

// allowlist は下流の種類ごとの、中継を許す口の一覧。下流の OpenAPI と、ここと denylist を合わせたものが同じであることを
// 契約の試験で確かめる（U3）。下流に口が増えたら、どちらかに足す。
var allowlist = map[kind][]route{
	kindVecgen: {
		{http.MethodGet, "/status", ""},
		{http.MethodGet, "/subscribers", ""},
		{http.MethodPost, "/subscribers", "subscriber.create"},
		// 一括取得は POST だが何も変えないので、読み出しとして扱う（U2）。
		{http.MethodPost, "/subscribers/batch-get", ""},
		{http.MethodGet, "/subscribers/{imsi}", ""},
		{http.MethodPatch, "/subscribers/{imsi}", "subscriber.update"},
		{http.MethodDelete, "/subscribers/{imsi}", "subscriber.delete"},
		{http.MethodPut, "/subscribers/{imsi}/state", "subscriber.state"},
		{http.MethodPost, "/subscribers/{imsi}/reinstate", "subscriber.reinstate"},
		{http.MethodGet, "/clients", ""},
		{http.MethodGet, "/clients/{clientId}", ""},
		{http.MethodGet, "/av-server-certificate", ""},
		{http.MethodGet, "/audit-logs", ""},
	},
	kindRadius: {
		{http.MethodGet, "/status", ""},
		{http.MethodGet, "/subscribers", ""},
		{http.MethodPost, "/subscribers", "subscriber.create"},
		{http.MethodPost, "/subscribers/batch-get", ""},
		{http.MethodGet, "/subscribers/{imsi}", ""},
		{http.MethodPatch, "/subscribers/{imsi}", "subscriber.update"},
		{http.MethodDelete, "/subscribers/{imsi}", "subscriber.delete"},
		{http.MethodPut, "/subscribers/{imsi}/state", "subscriber.state"},
		{http.MethodPost, "/subscribers/{imsi}/reinstate", "subscriber.reinstate"},
		{http.MethodGet, "/sessions", ""},
		{http.MethodGet, "/usage", ""},
		{http.MethodGet, "/clients", ""},
		{http.MethodGet, "/sources", ""},
		{http.MethodGet, "/audit-logs", ""},
	},
	kindCA: {
		{http.MethodGet, "/status", ""},
		{http.MethodGet, "/cas", ""},
		{http.MethodPost, "/cas", "ca.create"},
		{http.MethodGet, "/cas/{caId}", ""},
		{http.MethodPost, "/cas/{caId}/disable", "ca.disable"},
		{http.MethodGet, "/cas/{caId}/crl", ""},
		{http.MethodPut, "/cas/{caId}/crl", "crl.upload"},
		{http.MethodPost, "/cas/{caId}/crl:refresh", "crl.refresh"},
		{http.MethodPut, "/cas/{caId}/crl-source", "crl_source.put"},
		{http.MethodGet, "/certs", ""},
		{http.MethodPost, "/certs", "cert.issue"},
		{http.MethodGet, "/certs/{serial}", ""},
		{http.MethodPost, "/certs/{serial}/revoke", "cert.revoke"},
	},
	kindEdge: {
		{http.MethodGet, "/status", ""},
		{http.MethodGet, "/tenants", ""},
		{http.MethodGet, "/tenants/{tenantId}", ""},
		{http.MethodPut, "/tenants/{tenantId}", "tenant.put"},
		{http.MethodDelete, "/tenants/{tenantId}", "tenant.delete"},
		{http.MethodPost, "/tenants/{tenantId}/server-csr", "server_csr.create"},
		{http.MethodPut, "/tenants/{tenantId}/server-cert", "server_cert.put"},
		{http.MethodPost, "/tenants/{tenantId}/probe-csr", "probe_csr.create"},
		{http.MethodPut, "/tenants/{tenantId}/probe-cert", "probe_cert.put"},
		{http.MethodPost, "/tenants/{tenantId}/apply", "tenant.apply"},
		{http.MethodPost, "/tenants/{tenantId}/crl:refresh", "crl.refresh"},
		{http.MethodPost, "/tenants/{tenantId}/restart", "tenant.restart"},
		{http.MethodGet, "/tenants/{tenantId}/logs", ""},
		{http.MethodGet, "/tenants/{tenantId}/last-seen", ""},
	},
}

// denylist は下流にあるが中継しない口。鍵の経路を限るため（設計概要 §6.2、概要 §12.2）。
var denylist = map[kind][]route{
	kindVecgen: {
		{http.MethodGet, "/subscribers/{imsi}/keys", ""},
	},
}

// パスの値の形。下流が詳しく確かめるので、ここでは 1 つのセグメントとして安全な形だけを確かめる（P6）。
var (
	// imsiPattern は IMSI（aka-vecgen は 5〜15 桁、aka-radius は 15 桁の数字）。
	imsiPattern = regexp.MustCompile(`^[0-9]{5,15}$`)
	// tenantIDPattern はエッジのテナントの ID（OSS/BSS のテナントの番号）。
	tenantIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	// caIDPattern は radsec-ca の CA の ID（server、tenant.<slug>、brought.<slug>.<名前>）。
	caIDPattern = regexp.MustCompile(`^(server|tenant\.` + slug + `|brought\.` + slug + `\.` + slug + `)$`)
	// serialPattern は証明書のシリアル番号（16 進の小文字）。
	serialPattern = regexp.MustCompile(`^[0-9a-f]{1,64}$`)
)

// slug は radsec-ca・エッジの名前の形。
const slug = `[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?`

// pathValue はパスの値を確かめて返す。形が違えば 400 を書いて false。
func pathValue(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	v := r.PathValue(name)
	reason := ""
	switch name {
	case "imsi":
		if !imsiPattern.MatchString(v) {
			reason = "must be 5 to 15 digits"
		}
	case "clientId":
		if id, err := strconv.ParseInt(v, 10, 64); err != nil || id < 1 || strconv.FormatInt(id, 10) != v {
			reason = "must be a positive integer"
		}
	case "edgeId":
		if !downstream.ValidEdgeID(v) {
			reason = "must be a slug"
		}
	case "tenantId":
		if !tenantIDPattern.MatchString(v) {
			reason = "must be a positive integer of up to 19 digits"
		}
	case "caId":
		if !caIDPattern.MatchString(v) {
			reason = "must be server, tenant.<slug> or brought.<slug>.<name>"
		}
	case "serial":
		if !serialPattern.MatchString(v) {
			reason = "must be lowercase hexadecimal"
		}
	}
	if reason != "" {
		badParams(causeMandatoryIEIncorrect, invalidParam{name, reason}).write(w)
		return "", false
	}
	return v, true
}

// downstreamPath は口のパスの {名前} をパスの値に置き換えた、下流のパスの要素を返す。形が違えば 400 を書いて false。
func downstreamPath(w http.ResponseWriter, r *http.Request, path string) ([]string, bool) {
	var out []string
	for seg := range strings.SplitSeq(strings.TrimPrefix(path, "/"), "/") {
		if name, ok := strings.CutPrefix(seg, "{"); ok {
			v, ok := pathValue(w, r, strings.TrimSuffix(name, "}"))
			if !ok {
				return nil, false
			}
			seg = v
		}
		out = append(out, seg)
	}
	return out, true
}
