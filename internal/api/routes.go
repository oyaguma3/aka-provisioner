package api

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/oyaguma3/aka-provisioner/internal/downstream"
)

// route は中継を許す 1 つの口（設計概要 §6.2。段階 3-1 の U2）。
type route struct {
	// method は HTTP のメソッド。
	method string
	// path は下流の基底 URL（/admin/v2）より後ろのパス。{imsi}・{clientId} はパスの値。
	path string
	// action は下流を書き換える口の監査ログの action の後半（例 subscriber.create）。空なら読み出し
	// （Idempotency-Key を求めず、監査ログにも残さない）。
	action string
}

// write は下流を書き換える口か。
func (rt route) write() bool { return rt.action != "" }

// allowlist は下流ごとの、中継を許す口の一覧。下流の OpenAPI と、ここと denylist を合わせたものが同じであることを
// 契約の試験で確かめる（U3）。下流に口が増えたら、どちらかに足す。
var allowlist = map[downstream.Name][]route{
	downstream.Vecgen: {
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
	downstream.Radius: {
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
}

// denylist は下流にあるが中継しない口。鍵の経路を限るため（設計概要 §6.2、概要 §12.2）。
var denylist = map[downstream.Name][]route{
	downstream.Vecgen: {
		{http.MethodGet, "/subscribers/{imsi}/keys", ""},
	},
}

// パスの値の形。下流が詳しく確かめるので、ここでは 1 つのセグメントとして安全な形だけを確かめる。
var (
	// imsiPattern は IMSI（aka-vecgen は 5〜15 桁、aka-radius は 15 桁の数字）。
	imsiPattern = regexp.MustCompile(`^[0-9]{5,15}$`)
)

// pathValue はパスの値を確かめて返す。形が違えば 400 を書いて false。
func pathValue(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	v := r.PathValue(name)
	switch name {
	case "imsi":
		if !imsiPattern.MatchString(v) {
			badParams(causeMandatoryIEIncorrect, invalidParam{"imsi", "must be 5 to 15 digits"}).write(w)
			return "", false
		}
	case "clientId":
		if id, err := strconv.ParseInt(v, 10, 64); err != nil || id < 1 || strconv.FormatInt(id, 10) != v {
			badParams(causeMandatoryIEIncorrect, invalidParam{"clientId", "must be a positive integer"}).write(w)
			return "", false
		}
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
