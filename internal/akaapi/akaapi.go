// Package akaapi は aka-vecgen の管理API の呼び出し。
// API 仕様は aka-vecgen のリポジトリの docs/openapi/admin-api.yaml を参照。接続の仕組みは internal/downstream。
package akaapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/oyaguma3/aka-provisioner/internal/downstream"
)

// Client は aka-vecgen の管理API のクライアント。
type Client struct {
	*downstream.Client
}

// New はクライアントを作る。opts.Name は設定しなくてよい。
func New(opts downstream.Options) (*Client, error) {
	opts.Name = downstream.Aka
	c, err := downstream.New(opts)
	if err != nil {
		return nil, err
	}
	return &Client{c}, nil
}

// Status は aka-vecgen の状態。
type Status struct {
	Version                     string    `json:"version"`
	BootID                      string    `json:"bootId"`
	StartedAt                   time.Time `json:"startedAt"`
	SubscriberCount             int64     `json:"subscriberCount"`
	ClientCount                 int64     `json:"clientCount"`
	AVPlainEnabled              bool      `json:"avPlainEnabled"`
	AVServerCertificateNotAfter time.Time `json:"avServerCertificateNotAfter"`
}

// Status は aka-vecgen の状態を取得する。
func (c *Client) Status(ctx context.Context) (Status, error) {
	return c.Call[Status](ctx, downstream.Request{Method: http.MethodGet, Path: []string{"status"}})
}

// ---- 加入者（鍵を aka-vecgen に置く加入者）----

// SQN 増加タイプ。
const (
	SQNTypeInc1  = "inc1"
	SQNTypeInc32 = "inc32"
	SQNTypeInc33 = "inc33"
)

// ProblemDetails の cause の値（aka-vecgen の管理API の OpenAPI と同じ）。
const (
	CauseUserNotFound     = "USER_NOT_FOUND"
	CauseSubscriberExists = "SUBSCRIBER_ALREADY_EXISTS"
)

// Subscriber は加入者。Ki と OPc は含まない。
type Subscriber struct {
	IMSI       string `json:"imsi"`
	SQN        string `json:"sqn"`
	AMF        string `json:"amf"`
	SQNType    string `json:"sqnType"`
	AllowPlain bool   `json:"allowPlain"`
	// AllowedClientIDs は許可クライアントID（昇順。削除済みクライアントの ID は含まない）。
	AllowedClientIDs []int64   `json:"allowedClientIds"`
	CreatedAt        time.Time `json:"createdAt"`
	// UpdatedAt は最終変更日時。SQN の払い出しでは更新しない。
	UpdatedAt time.Time `json:"updatedAt"`
}

// SubscriberCreate は加入者の登録内容。SQN、AMF、SQNType は空なら管理API の既定値になる。
type SubscriberCreate struct {
	IMSI             string  `json:"imsi"`
	Ki               string  `json:"ki"`
	OPc              string  `json:"opc"`
	SQN              string  `json:"sqn,omitempty"`
	AMF              string  `json:"amf,omitempty"`
	SQNType          string  `json:"sqnType,omitempty"`
	AllowPlain       bool    `json:"allowPlain"`
	AllowedClientIDs []int64 `json:"allowedClientIds"`
}

// SubscriberUpdate は加入者の変更内容（JSON Merge Patch）。nil の項目は変更しない。
// provisioner は許可クライアント（allowedClientIds）を変更しないので、その項目は持たない（設計概要 §5）。
type SubscriberUpdate struct {
	Ki         *string `json:"ki,omitzero"`
	OPc        *string `json:"opc,omitzero"`
	SQN        *string `json:"sqn,omitzero"`
	AMF        *string `json:"amf,omitzero"`
	SQNType    *string `json:"sqnType,omitzero"`
	AllowPlain *bool   `json:"allowPlain,omitzero"`
}

// SubscriberKeys は加入者の Ki と OPc。
type SubscriberKeys struct {
	Ki  string `json:"ki"`
	OPc string `json:"opc"`
}

// SubscriberList は加入者の一覧の 1 ページ（IMSI の辞書順）。
type SubscriberList struct {
	Items []Subscriber `json:"items"`
	// Total は Prefix に一致する加入者の総数。
	Total int64 `json:"total"`
	// NextCursor は次のページがある場合だけ入る。
	NextCursor string `json:"nextCursor"`
}

// ListSubscribers は加入者の一覧を IMSI の辞書順で取得する。
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

// DeleteSubscriber は加入者を削除する。
func (c *Client) DeleteSubscriber(ctx context.Context, imsi string) error {
	_, err := c.Call[struct{}](ctx, downstream.Request{Method: http.MethodDelete, Path: []string{"subscribers", imsi}})
	return err
}

// GetSubscriberKeys は加入者の Ki と OPc を取得する。管理API の監査ログに残る。
func (c *Client) GetSubscriberKeys(ctx context.Context, imsi string) (SubscriberKeys, error) {
	return c.Call[SubscriberKeys](ctx, downstream.Request{Method: http.MethodGet, Path: []string{"subscribers", imsi, "keys"}})
}

// ---- AVクライアント ----

// AVClient は AVクライアント（認証ベクターAPI のクライアント）。
type AVClient struct {
	ID                int64     `json:"id"`
	Name              string    `json:"name"`
	CertPEM           string    `json:"certPem"`
	FingerprintSHA256 string    `json:"fingerprintSha256"`
	Subject           string    `json:"subject"`
	NotBefore         time.Time `json:"notBefore"`
	NotAfter          time.Time `json:"notAfter"`
	Enabled           bool      `json:"enabled"`
	NetworkName       string    `json:"networkName"`
	CreatedAt         time.Time `json:"createdAt"`
}

// CauseClientNotFound は、AVクライアントが存在しないことを表す cause。
const CauseClientNotFound = "CLIENT_NOT_FOUND"

// GetAVClient は AVクライアントを取得する。
func (c *Client) GetAVClient(ctx context.Context, id int64) (AVClient, error) {
	return c.Call[AVClient](ctx, downstream.Request{
		Method: http.MethodGet, Path: []string{"clients", strconv.FormatInt(id, 10)},
	})
}

// hints は aka-vecgen の管理API に接続できなかったときの対処の文。
var hints = downstream.Hints{
	HostnameMismatch: "aka-vecgen の管理API のサーバー証明書に、接続先のホスト名（または IP アドレス）が入っていません。" +
		"aka-vecgen 側の VECGEN_ADMIN_TLS_HOSTS に接続先（同一ホストなら aka-vecgen）を加えて admin-cert reset で作り直し、" +
		"provisioner にも渡し直すか、証明書に入っている名前で接続してください。",
	UnknownServer: "aka-vecgen の管理API のサーバー証明書が、設定した証明書（PROVISIONER_AKA_SERVER_CERT）と一致しません。" +
		"aka-vecgen の admin-cert で取り出した証明書を設定してください。",
	ServerCertInvalid: "aka-vecgen の管理API のサーバー証明書が有効期間外です。aka-vecgen 側で admin-cert reset で作り直し、" +
		"provisioner にも渡し直してください。",
	ClientRejected: "aka-vecgen が provisioner のクライアント証明書を受け付けませんでした。" +
		"aka-vecgen 側の .env の VECGEN_ADMIN_CLIENTS に、このクライアント証明書のフィンガープリントが登録されているか" +
		"（登録後に aka-vecgen を起動し直したか）、証明書が有効期間内かを確認してください。",
	ConnectionReset: "aka-vecgen が接続を切りました。provisioner のクライアント証明書が受け付けられなかった可能性があります。" +
		"aka-vecgen 側の .env の VECGEN_ADMIN_CLIENTS に、このクライアント証明書のフィンガープリントが登録されているか" +
		"（登録後に aka-vecgen を起動し直したか）、証明書が有効期間内かを確認してください" +
		"（aka-vecgen のログに admin client certificate rejected が出ていれば、これが原因です）。",
	NameNotFound: "aka-vecgen のホスト名（%s）を解決できません。aka-vecgen が起動しているか、" +
		"同一ホストの場合は provisioner が共有ネットワーク（aka-av）に参加しているか確認してください。",
	DialFailed: "aka-vecgen の管理API に接続できません。aka-vecgen が起動しているか（管理API は VECGEN_ADMIN_CLIENTS を" +
		"設定したときだけ有効）、接続先の URL とポートが正しいか確認してください。",
	Timeout: "aka-vecgen の管理API から時間内に応答がありません。接続先のアドレス、VPN、aka-vecgen 側の公開先（VECGEN_ADMIN_PUBLISH）を確認してください。",
}

// Diagnose は、aka-vecgen の管理API に接続できなかったときの原因の見当を返す。見当がつかなければ空文字列。
func Diagnose(err error) string { return downstream.Diagnose(err, hints) }
