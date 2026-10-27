package api

import (
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/oyaguma3/aka-provisioner/internal/provapi"
)

// 入力の検証（設計概要 §6.2）。下流と同じ規則で先に検証し、下流への書き込みを始めてから入力の誤りで断られて
// 補償する、という流れを通常は起こさないようにする。規則は本PoCの pkg/validation と aka-vecgen の管理API に揃える。

var (
	hex32Pattern = regexp.MustCompile(`^[0-9A-Fa-f]{32}$`)
	hex12Pattern = regexp.MustCompile(`^[0-9A-Fa-f]{12}$`)
	hex4Pattern  = regexp.MustCompile(`^[0-9A-Fa-f]{4}$`)
	// nasIDPattern は NAS-Identifier（印字可能 ASCII 1〜253 文字。"*" 単独は任意の NAS）。
	nasIDPattern = regexp.MustCompile(`^[\x21-\x7E]{1,253}$`)
	vlanPattern  = regexp.MustCompile(`^[0-9]{1,4}$`)
	// listIMSIPattern は一覧の prefix と cursor（IMSI の前方 1〜15 桁）。
	listIMSIPattern = regexp.MustCompile(`^[0-9]{1,15}$`)
)

var sqnTypes = []string{"inc1", "inc32", "inc33"}

// validation は入力検証の結果を集める。
type validation struct {
	missing   []invalidParam // 必須項目がない
	mandatory []invalidParam // 必須項目の値が不正
	optional  []invalidParam // 任意項目の値が不正
}

// problem は検証エラーがあれば 400 を返す。なければ nil。
// cause は、必須項目の欠落、必須項目の不正、任意項目の不正の順に優先し、invalidParams には全てを同じ順で並べる（下流と同じ）。
func (v *validation) problem() *problem {
	all := append(append(append([]invalidParam(nil), v.missing...), v.mandatory...), v.optional...)
	switch {
	case len(v.missing) > 0:
		return badParams(causeMandatoryIEMissing, all...)
	case len(v.mandatory) > 0:
		return badParams(causeMandatoryIEIncorrect, all...)
	case len(v.optional) > 0:
		return badParams(causeOptionalIEIncorrect, all...)
	}
	return nil
}

// required は必須の文字列項目を確かめる。
func (v *validation) required(name string, p *string, ok func(string) bool, reason string) {
	switch {
	case p == nil:
		v.missing = append(v.missing, invalidParam{name, "is required"})
	case !ok(*p):
		v.mandatory = append(v.mandatory, invalidParam{name, reason})
	}
}

// optionalField は任意の文字列項目を確かめる。
func (v *validation) optionalField(name string, p *string, ok func(string) bool, reason string) {
	if p != nil && !ok(*p) {
		v.optional = append(v.optional, invalidParam{name, reason})
	}
}

// policyRequest は認可ポリシーの入力。項目の有無を区別するためにポインターで受ける。
type policyRequest struct {
	Default *string        `json:"default"`
	Rules   *[]ruleRequest `json:"rules"`
}

type ruleRequest struct {
	NASID          *string   `json:"nasId"`
	AllowedSSIDs   *[]string `json:"allowedSsids"`
	VLANID         *string   `json:"vlanId"`
	SessionTimeout *int      `json:"sessionTimeout"`
}

// policy は認可ポリシーを検証し、正規化したもの（本PoCと同じく、default は小文字、nasId と SSID は前後の空白を除く）を返す。
func (v *validation) policy(prefix string, p *policyRequest) provapi.PolicyPut {
	out := provapi.PolicyPut{Rules: []provapi.PolicyRule{}}
	if p.Default != nil {
		d := strings.ToLower(strings.TrimSpace(*p.Default))
		p.Default, out.Default = &d, d
	}
	v.required(prefix+"default", p.Default, func(s string) bool { return s == "allow" || s == "deny" }, "must be 'allow' or 'deny'")
	if p.Rules == nil {
		v.missing = append(v.missing, invalidParam{prefix + "rules", "is required"})
		return out
	}
	for i, r := range *p.Rules {
		name := fmt.Sprintf("%srules[%d].", prefix, i)
		rule := provapi.PolicyRule{}
		if r.NASID != nil {
			n := strings.TrimSpace(*r.NASID)
			r.NASID, rule.NASID = &n, n
		}
		v.required(name+"nasId", r.NASID, nasIDPattern.MatchString, "must be 1-253 printable ASCII characters")
		switch {
		case r.AllowedSSIDs == nil:
			v.missing = append(v.missing, invalidParam{name + "allowedSsids", "is required"})
		case len(*r.AllowedSSIDs) == 0:
			v.mandatory = append(v.mandatory, invalidParam{name + "allowedSsids", "at least one SSID required"})
		}
		if r.AllowedSSIDs != nil {
			for j, ssid := range *r.AllowedSSIDs {
				ssid = strings.TrimSpace(ssid)
				if ssid == "" || len(ssid) > 32 {
					v.mandatory = append(v.mandatory, invalidParam{fmt.Sprintf("%sallowedSsids[%d]", name, j), "must be 1-32 bytes"})
				}
				rule.AllowedSSIDs = append(rule.AllowedSSIDs, ssid)
			}
		}
		if r.VLANID != nil {
			if *r.VLANID != "" {
				n, err := strconv.Atoi(*r.VLANID)
				if !vlanPattern.MatchString(*r.VLANID) || err != nil || n > 4094 {
					v.optional = append(v.optional, invalidParam{name + "vlanId", "must be digits between 0 and 4094"})
				}
			}
			rule.VLANID = *r.VLANID
		}
		if r.SessionTimeout != nil {
			if *r.SessionTimeout < 0 || *r.SessionTimeout > 86400 {
				v.optional = append(v.optional, invalidParam{name + "sessionTimeout", "must be between 0 and 86400"})
			}
			rule.SessionTimeout = *r.SessionTimeout
		}
		out.Rules = append(out.Rules, rule)
	}
	return out
}

// checkContentType は要求の Content-Type を確かめる。allowed のどれかでなければ 400 を返す（パラメーターは無視する）。
func checkContentType(r *http.Request, allowed ...string) *problem {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	for _, a := range allowed {
		if mt == a {
			return nil
		}
	}
	return newProblem(http.StatusBadRequest, causeInvalidMsgFormat, "Content-Type must be "+strings.Join(allowed, " or "))
}
