// Package trace はトレースID を扱う。
//
// provisioner は、管理クライアント（BFF など）が X-Trace-ID ヘッダーで渡したトレースID を使い、なければ採番する。
// 同じ値を下流（provisioning-api と aka-vecgen の管理API）にも X-Trace-ID で渡すので、
// 管理クライアント・provisioner・下流のログと監査ログをトレースID で突き合わせられる。
package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
)

// Header はトレースID を渡す HTTP ヘッダー。
const Header = "X-Trace-ID"

// pattern は受け付けるトレースID（印字可能 ASCII 1〜64 文字）。下流の検証と同じ。
var pattern = regexp.MustCompile(`^[\x21-\x7E]{1,64}$`)

type key struct{}

// New はトレースID（16進32桁。下流が自分で採番する形式と同じ）を作る。
func New() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Valid は id がトレースID として受け付けられる形式かを返す。
func Valid(id string) bool { return pattern.MatchString(id) }

// With はトレースID をコンテキストに入れる。
func With(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, key{}, id)
}

// From はコンテキストに入れたトレースID を返す。入っていなければ空文字列。
func From(ctx context.Context) string {
	id, _ := ctx.Value(key{}).(string)
	return id
}
