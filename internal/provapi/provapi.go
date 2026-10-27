// Package provapi は本PoC（aka-radius）の Provisioning API（provisioning-api）の呼び出し。
// API 仕様は本PoCのリポジトリの docs/openapi/provisioning-api.yaml を参照。接続の仕組みは internal/downstream。
package provapi

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/downstream"
)

// Client は Provisioning API のクライアント。
type Client struct {
	*downstream.Client
}

// New はクライアントを作る。opts.Name は設定しなくてよい。
func New(opts downstream.Options) (*Client, error) {
	opts.Name = downstream.Prov
	c, err := downstream.New(opts)
	if err != nil {
		return nil, err
	}
	return &Client{c}, nil
}

// Status は provisioning-api の状態。
type Status struct {
	Version string `json:"version"`
	// NodeName はノードの識別名（本PoC側の PROVISIONING_API_NODE_NAME）。
	NodeName        string    `json:"nodeName"`
	StartedAt       time.Time `json:"startedAt"`
	SubscriberCount int64     `json:"subscriberCount"`
	ClientCount     int64     `json:"clientCount"`
	PolicyCount     int64     `json:"policyCount"`
	// SessionCount はアクティブセッションの数。0.3.0 より前の provisioning-api は返さない（nil）。
	SessionCount *int64 `json:"sessionCount"`
}

// Status は provisioning-api の状態を取得する。
func (c *Client) Status(ctx context.Context) (Status, error) {
	return c.Call[Status](ctx, downstream.Request{Method: http.MethodGet, Path: []string{"status"}})
}

// hints は provisioning-api に接続できなかったときの対処の文。
var hints = downstream.Hints{
	HostnameMismatch: "provisioning-api のサーバー証明書に、接続先のホスト名（または IP アドレス）が入っていません。" +
		"本PoC側でサーバー証明書の SAN に接続先（同一ホストなら DNS:provisioning-api）を入れて作り直し" +
		"（本PoC B-02 §15.2）、provisioner にも渡し直してください。",
	UnknownServer: "provisioning-api のサーバー証明書が、設定した証明書（PROVISIONER_PROV_SERVER_CERT）と一致しません。" +
		"本PoCの deployments/certs/provisioning/server.pem を設定してください。",
	ServerCertInvalid: "provisioning-api のサーバー証明書が有効期間外です。本PoC側で作り直し（本PoC B-02 §15.2）、provisioner にも渡し直してください。",
	ClientRejected: "provisioning-api が provisioner のクライアント証明書を受け付けませんでした。" +
		"本PoC側の .env の PROVISIONING_API_ADMIN_CLIENTS に、このクライアント証明書のフィンガープリントが登録されているか" +
		"（登録後に provisioning-api を作り直したか）、証明書が有効期間内かを確認してください。",
	ConnectionReset: "provisioning-api が接続を切りました。provisioner のクライアント証明書が受け付けられなかった可能性があります。" +
		"本PoC側の .env の PROVISIONING_API_ADMIN_CLIENTS に、このクライアント証明書のフィンガープリントが登録されているか" +
		"（登録後に provisioning-api を作り直したか）、証明書が有効期間内かを確認してください" +
		"（provisioning-api のログに PROV_CLIENT_REJECTED が出ていれば、これが原因です）。",
	NameNotFound: "provisioning-api のホスト名（%s）を解決できません。本PoCを docker compose --profile provisioning up -d で" +
		"起動しているか、同一ホストの場合は provisioner が共有ネットワーク（eapaka-prov）に参加しているか確認してください。",
	DialFailed: "provisioning-api に接続できません。本PoCの provisioning-api が起動しているか（--profile provisioning）、" +
		"接続先の URL とポートが正しいか確認してください。",
	Timeout: "provisioning-api から時間内に応答がありません。接続先のアドレス、VPN、本PoC側の公開先（PROVISIONING_API_BIND）を確認してください。",
}

// Diagnose は、provisioning-api に接続できなかったときの原因の見当を返す。見当がつかなければ空文字列。
func Diagnose(err error) string { return downstream.Diagnose(err, hints) }

// ---- 加入者（鍵を本PoCに置く加入者。sub:{IMSI}）----

// Subscriber は加入者。Ki と OPc は含まない。
type Subscriber struct {
	IMSI string `json:"imsi"`
	AMF  string `json:"amf"`
	SQN  string `json:"sqn"`
	// CreatedAt は登録日時。値を持たない加入者（古いデータなど）ではゼロ値。
	CreatedAt time.Time `json:"createdAt,omitzero"`
}

// SubscriberCreate は加入者の登録内容。AMF と SQN は空なら provisioning-api の既定値（8000、000000000000）になる。
type SubscriberCreate struct {
	IMSI string `json:"imsi"`
	Ki   string `json:"ki"`
	OPc  string `json:"opc"`
	AMF  string `json:"amf,omitempty"`
	SQN  string `json:"sqn,omitempty"`
}

// SubscriberUpdate は加入者の変更内容（JSON Merge Patch）。nil の項目は変更しない。
// SQN を指定しなければ、認証で進んだ SQN には触れない。
type SubscriberUpdate struct {
	Ki  *string `json:"ki,omitzero"`
	OPc *string `json:"opc,omitzero"`
	AMF *string `json:"amf,omitzero"`
	SQN *string `json:"sqn,omitzero"`
}

// SubscriberKeys は加入者の Ki と OPc。
type SubscriberKeys struct {
	Ki  string `json:"ki"`
	OPc string `json:"opc"`
}

// SubscriberList は加入者の一覧の 1 ページ（IMSI の昇順）。
type SubscriberList struct {
	Items []Subscriber `json:"items"`
	// Total は Prefix に一致する加入者の総数。
	Total int64 `json:"total"`
	// NextCursor は次のページがある場合だけ入る。
	NextCursor string `json:"nextCursor"`
}

// ProblemDetails の cause の値（provisioning-api の OpenAPI と同じ）。
const (
	CauseUserNotFound     = "USER_NOT_FOUND"
	CausePolicyNotFound   = "POLICY_NOT_FOUND"
	CauseSubscriberExists = "SUBSCRIBER_ALREADY_EXISTS"
)

// ListSubscribers は加入者の一覧を IMSI の昇順で取得する。
func (c *Client) ListSubscribers(ctx context.Context, p downstream.ListParams) (SubscriberList, error) {
	return c.Call[SubscriberList](ctx, downstream.Request{Method: http.MethodGet, Path: []string{"subscribers"}, Query: p.Query()})
}

// CreateSubscriber は加入者を登録する。
func (c *Client) CreateSubscriber(ctx context.Context, s SubscriberCreate) (Subscriber, error) {
	return c.Call[Subscriber](ctx, downstream.Request{Method: http.MethodPost, Path: []string{"subscribers"}, Body: s})
}

// GetSubscriber は加入者を取得する。
func (c *Client) GetSubscriber(ctx context.Context, imsi string) (Subscriber, error) {
	return c.Call[Subscriber](ctx, downstream.Request{Method: http.MethodGet, Path: []string{"subscribers", imsi}})
}

// UpdateSubscriber は加入者を変更する。
func (c *Client) UpdateSubscriber(ctx context.Context, imsi string, u SubscriberUpdate) (Subscriber, error) {
	return c.Call[Subscriber](ctx, downstream.Request{
		Method: http.MethodPatch, Path: []string{"subscribers", imsi}, Body: u, ContentType: "application/merge-patch+json",
	})
}

// DeleteSubscriber は加入者を削除する。同じ IMSI の認可ポリシーは削除しない。
func (c *Client) DeleteSubscriber(ctx context.Context, imsi string) error {
	_, err := c.Call[struct{}](ctx, downstream.Request{Method: http.MethodDelete, Path: []string{"subscribers", imsi}})
	return err
}

// GetSubscriberKeys は加入者の Ki と OPc を取得する。provisioning-api の監査ログに残る。
func (c *Client) GetSubscriberKeys(ctx context.Context, imsi string) (SubscriberKeys, error) {
	return c.Call[SubscriberKeys](ctx, downstream.Request{Method: http.MethodGet, Path: []string{"subscribers", imsi, "keys"}})
}

// ---- 認可ポリシー（policy:{IMSI}）----

// PolicyRule は認可ポリシーのルール。先頭から順に評価し、最初に一致したものを採用する。
type PolicyRule struct {
	// NASID は NAS-Identifier。"*" は任意の NAS に一致する。
	NASID string `json:"nasId"`
	// AllowedSSIDs は許可する SSID。"*" は任意の SSID に一致する。
	AllowedSSIDs []string `json:"allowedSsids"`
	// VLANID は割り当てる VLAN ID（0〜4094 の数字）。空なら未設定。
	VLANID string `json:"vlanId,omitempty"`
	// SessionTimeout は Session-Timeout（秒）。0 なら未設定。
	SessionTimeout int `json:"sessionTimeout,omitzero"`
}

// Policy は認可ポリシー。
type Policy struct {
	IMSI    string       `json:"imsi"`
	Default string       `json:"default"`
	Rules   []PolicyRule `json:"rules"`
	// Status は加入者の状態（provisioning-api 0.4.0 から）。active（利用中）か suspended（停止中）。
	// 認可ポリシーの PUT では変わらない。変更は PUT /policies/{imsi}/status（provisioner は中継するだけ）。
	Status string `json:"status,omitempty"`
}

// 加入者の状態（Policy.Status）。
const (
	PolicyActive    = "active"
	PolicySuspended = "suspended"
)

// PolicyPut は認可ポリシーの内容（全体を置き換える）。
type PolicyPut struct {
	Default string       `json:"default"`
	Rules   []PolicyRule `json:"rules"`
}

// PolicyList は認可ポリシーの一覧の 1 ページ（IMSI の昇順）。
type PolicyList struct {
	Items []Policy `json:"items"`
	// Total は Prefix に一致する認可ポリシーの総数。
	Total int64 `json:"total"`
	// NextCursor は次のページがある場合だけ入る。
	NextCursor string `json:"nextCursor"`
}

// ListPolicies は認可ポリシーの一覧を IMSI の昇順で取得する。
func (c *Client) ListPolicies(ctx context.Context, p downstream.ListParams) (PolicyList, error) {
	return c.Call[PolicyList](ctx, downstream.Request{Method: http.MethodGet, Path: []string{"policies"}, Query: p.Query()})
}

// GetPolicy は認可ポリシーを取得する。
func (c *Client) GetPolicy(ctx context.Context, imsi string) (Policy, error) {
	return c.Call[Policy](ctx, downstream.Request{Method: http.MethodGet, Path: []string{"policies", imsi}})
}

// PutPolicy は認可ポリシーを作成する、または全体を置き換える。作成した場合は created が true。加入者の有無は問わない。
func (c *Client) PutPolicy(ctx context.Context, imsi string, p PolicyPut) (policy Policy, created bool, err error) {
	resp, err := c.Send(ctx, downstream.Request{Method: http.MethodPut, Path: []string{"policies", imsi}, Body: p})
	if err != nil {
		return Policy{}, false, err
	}
	defer resp.Body.Close()
	if err := json.UnmarshalRead(io.LimitReader(resp.Body, maxResponseBytes), &policy); err != nil {
		return Policy{}, false, fmt.Errorf("prov PUT %s: decode response: %w", resp.Request.URL.Path, err)
	}
	return policy, resp.StatusCode == http.StatusCreated, nil
}

// DeletePolicy は認可ポリシーを削除する。
func (c *Client) DeletePolicy(ctx context.Context, imsi string) error {
	_, err := c.Call[struct{}](ctx, downstream.Request{Method: http.MethodDelete, Path: []string{"policies", imsi}})
	return err
}

// maxResponseBytes は、PutPolicy で応答を読む上限（認可ポリシー 1 件なので小さくてよい）。
const maxResponseBytes = 1 << 20
