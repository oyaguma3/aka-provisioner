// Package plmn は PLMN マップ（PROVISIONER_PLMN_MAP）と、IMSI から鍵の置き場所を決める規則を扱う。
//
// 本PoCの vector-gateway は、IMSI の PLMN と VECTOR_GATEWAY_PLMN_MAP で認証ベクターの取得先（接続方式）を決める
// （本PoC D-12 §3）。鍵をそれと違う所に置くと認証できないので、provisioner は同じ形式・同じ値のマップを持ち、
// 同じ規則で置き場所を決める。
package plmn

import (
	"fmt"
	"strings"
)

// KeyStore は鍵（Ki / OPc）の置き場所。
type KeyStore string

const (
	// KeyStorePoC は本PoCの Vector API（接続方式 00。provisioning-api の加入者）。
	KeyStorePoC KeyStore = "poc"
	// KeyStoreAKA は aka-vecgen（接続方式 01。aka-vecgen の管理API の加入者）。
	KeyStoreAKA KeyStore = "aka"
)

// Entry はマップの 1 件。
type Entry struct {
	PLMN     string
	KeyStore KeyStore
}

// Map は PLMN から置き場所への対応。ゼロ値は空のマップ（すべて poc）。
type Map struct {
	byPLMN map[string]KeyStore
	// entries は設定に書かれた順（同じ PLMN は後のものだけ）。
	entries []Entry
}

// Parse は "PLMN:ID,PLMN:ID,..." を読む。本PoCの vector-gateway の ParsePLMNMap と同じく、
// 前後の空白と空の要素は無視し、同じ PLMN が複数あれば後のものを使う。
// ID は 00（poc）と 01（aka）だけを受け付ける（provisioner はそれ以外の接続方式を扱えない）。
func Parse(s string) (Map, error) {
	m := Map{byPLMN: map[string]KeyStore{}}
	for entry := range strings.SplitSeq(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		p, id, ok := strings.Cut(entry, ":")
		if !ok {
			return Map{}, fmt.Errorf("invalid PLMN map entry %q: want PLMN:ID", entry)
		}
		p, id = strings.TrimSpace(p), strings.TrimSpace(id)
		if !isDigits(p) || len(p) < 5 || len(p) > 6 {
			return Map{}, fmt.Errorf("invalid PLMN map entry %q: PLMN must be 5-6 digits", entry)
		}
		var ks KeyStore
		switch id {
		case "00":
			ks = KeyStorePoC
		case "01":
			ks = KeyStoreAKA
		default:
			return Map{}, fmt.Errorf("invalid PLMN map entry %q: ID must be 00 or 01 (provisioner handles only these)", entry)
		}
		if _, dup := m.byPLMN[p]; dup {
			for i, e := range m.entries {
				if e.PLMN == p {
					m.entries = append(m.entries[:i], m.entries[i+1:]...)
					break
				}
			}
		}
		m.byPLMN[p] = ks
		m.entries = append(m.entries, Entry{PLMN: p, KeyStore: ks})
	}
	return m, nil
}

// KeyStore は IMSI の置き場所を返す。先頭 6 桁、次に先頭 5 桁の順でマップと照合し（本PoC D-12 §3.4 と同じ）、
// どれにも一致しなければ poc。
func (m Map) KeyStore(imsi string) KeyStore {
	for _, n := range []int{6, 5} {
		if len(imsi) >= n {
			if ks, ok := m.byPLMN[imsi[:n]]; ok {
				return ks
			}
		}
	}
	return KeyStorePoC
}

// Entries はマップの内容を設定に書かれた順で返す。
func (m Map) Entries() []Entry { return append([]Entry(nil), m.entries...) }

// Uses は、マップに置き場所 ks の PLMN があるかを返す。
func (m Map) Uses(ks KeyStore) bool {
	for _, e := range m.entries {
		if e.KeyStore == ks {
			return true
		}
	}
	return false
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
