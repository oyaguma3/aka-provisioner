# eapaka-node-provisioner

[![CI](https://github.com/oyaguma3/eapaka-node-provisioner/actions/workflows/ci.yml/badge.svg)](https://github.com/oyaguma3/eapaka-node-provisioner/actions/workflows/ci.yml)
[![govulncheck](https://github.com/oyaguma3/eapaka-node-provisioner/actions/workflows/govulncheck.yml/badge.svg)](https://github.com/oyaguma3/eapaka-node-provisioner/actions/workflows/govulncheck.yml)

> このリポジトリは、4 つのソフトウェア（[本PoC](https://github.com/oyaguma3/eapaka-radius-server-poc)、[aka-only-server](https://github.com/oyaguma3/aka-only-server)、[eapaka-node-provisioner](https://github.com/oyaguma3/eapaka-node-provisioner)、[eapaka-ossbss-sample](https://github.com/oyaguma3/eapaka-ossbss-sample)）を組み合わせを確かめて提供する **PoC セット**（[eapaka-pocset](https://github.com/oyaguma3/eapaka-pocset)）の一部です。組み合わせ（各リポジトリのコミット）、変更の記録（CHANGELOG）、1 台への導入の案内、4 つを通した試験は eapaka-pocset にあります。

[EAP-AKA RADIUS PoC](https://github.com/oyaguma3/eapaka-radius-server-poc)（以下「本PoC」）の Provisioning API と、[aka-only-server](https://github.com/oyaguma3/aka-only-server) の管理API を組み合わせて操作する統合API（コマンド名 `eapaka-provisioner`。以下「provisioner」）です。加入者を「IMSI＋鍵の置き場所＋認可ポリシー」として扱い、2 つのノードにまたがる操作の順序・排他・失敗時の補償を受け持ちます。

```
[BFF など] ──mTLS（X-Operator-Id / X-Trace-ID / Idempotency-Key）──> [provisioner] ──> Valkey（provisioner 専用）
                                                                       ├─mTLS─> provisioning-api（本PoC）
                                                                       └─mTLS─> aka-only-server の管理API
```

個人利用・検証用途の API です。画面は持たず、管理 GUI（BFF）から呼ばれることを想定しています。インターネットに直接公開せず、同じホストの Docker ネットワークか VPN（Tailscale、WireGuard など）越しに使います。

## 特徴

- 加入者の鍵（Ki / OPc）の置き場所を、本PoCの vector-gateway と同じ PLMN マップで決めます。接続方式 `00` の PLMN は本PoC（`poc`）、`01` の PLMN は aka-only-server（`aka`）に鍵を置き、認可ポリシーはどちらも本PoCに置きます。
- 加入者の作成・変更・削除は、手順ごとの状態を「操作の記録」に残しながら行います。途中で失敗したら、作成・変更は補償で元に戻し、削除は残りを続けます。戻せなかったものは、中のワーカーが後で自動でやり直し、`/operations` で確かめて手で対応できます。
- 同じ IMSI の操作は IMSI ごとのロックで排他し、`Idempotency-Key` で再送を判定します。未完了の操作が残っている IMSI への新しい操作は、片付けるまで断ります（古い操作の補償が新しい操作の結果を消さないように）。
- 取得では、2 つのノードの食い違い（鍵がない、ポリシーがない、置き場所でない方にも鍵がある等）を `issues` で示します。
- RADIUSクライアント、認可ポリシー、セッション、下流の監査ログは、本PoCの Provisioning API と同じ形で中継します（BFF が呼び先を付け替えやすいように）。
- 加入者を登録したまま停止・再開できます（本PoCの認可ポリシーの状態 `status`。鍵の置き場所によらず本PoCが認証を拒否します）。
- 操作者（`X-Operator-Id`）とトレースID（`X-Trace-ID`）を下流 2 つに渡すので、provisioner と下流の監査ログを突き合わせられます。
- 加入者のデータは保存しません（正本は下流）。専用の Valkey に持つのは、操作の記録、ロック、`Idempotency-Key`、監査ログだけです。Ki / OPc と共有シークレットは保存せず、ログにも出しません。

| API（`/admin/v1`） | 内容 |
|---|---|
| `/status` | 下流 2 つへの接続、PLMN マップ、vector-gateway の AVクライアント、未完了の操作の件数 |
| `/subscribers` | 加入者の統合操作（一覧、作成、取得、変更、削除、Ki / OPc の取得） |
| `/operations` | 操作の記録の確認と、やり直し（`retry`）・閉じる（`dismiss`） |
| `/clients`、`/policies`、`/sessions` | 本PoCの RADIUSクライアント・認可ポリシー（加入者の停止・再開 `/policies/{imsi}/status` を含む）・セッションの中継 |
| `/audit-logs`、`/prov/audit-logs`、`/aka/audit-logs` | provisioner と下流 2 つの監査ログ |
| `/aka/av-clients/{clientId}` | vector-gateway の AVクライアントの参照 |

詳しくは [API 仕様](docs/openapi/provisioner-api.yaml)を参照してください。

## 導入

Docker Engine と compose プラグインが必要です。本PoC（provisioning-api 0.4.0 以降。0.3.0 では加入者の停止・再開が使えません）と aka-only-server（管理API 0.2.0 以降）を導入しておき、provisioner を両方の管理クライアントとして登録します。手順は[運用ガイド](docs/operation-guide.md)の 2 章（同一ホスト）と 3 章（別ホスト）にあります。

流れは次の通りです。

1. `.env.example` を `.env` にコピーし、専用の Valkey のパスワードを書く。
2. `eapaka-provisioner gen-client-cert` で下流に提示するクライアント証明書を作り、表示された値を本PoCの `PROVISIONING_API_ADMIN_CLIENTS` と aka-only-server の `AKA_ADMIN_CLIENTS` に登録する。
3. 下流 2 つのサーバー証明書を `certs/` に置く（所有者は UID 65532）。
4. `.env` に、管理クライアント（BFF など）、aka-only-server の URL、vector-gateway の AVクライアントID、本PoCと同じ PLMN マップを書く。
5. 起動して、下流に接続できることを確かめる（`docker compose ps` で healthy になるのは API が待ち受けていること。下流は `check-downstream` で見る）。

```bash
docker compose up -d
```

```bash
docker compose exec -T eapaka-provisioner /eapaka-provisioner check-downstream
```

6. `server-cert` でサーバー証明書を取り出し、管理クライアントに渡す。同じホストの管理クライアントは共有ネットワーク `eapaka-provisioner` に参加して `https://eapaka-provisioner:9446/admin/v1` で接続します。

## ドキュメント

| ファイル | 内容 |
|---|---|
| [docs/operation-guide.md](docs/operation-guide.md) | 導入（同一ホスト / 別ホスト）、鍵の置き場所、公開範囲、操作の記録の確認と対応、ログと監査ログ、バックアップ、更新、障害時の確認、環境変数 |
| [docs/design-overview.md](docs/design-overview.md) | 設計概要（鍵の置き場所、統合操作と補償、中継、データモデル、排他・再送・操作の記録、将来拡張） |
| [docs/openapi/provisioner-api.yaml](docs/openapi/provisioner-api.yaml) | API 仕様（OpenAPI 3.0） |

## 開発

Go 1.27.3 を使います。外部依存は `github.com/valkey-io/valkey-go` だけです。

```bash
go test ./...
```

Valkey を使う結合テストと、下流を相手にした契約テストは、接続先を環境変数で指定したときだけ動きます。Valkey の結合テストは論理データベース 1 番の全データを消すので、専用の Valkey を使ってください。

```bash
PROVISIONER_TEST_VALKEY_ADDR=127.0.0.1:16381 PROVISIONER_TEST_VALKEY_PASSWORD=<パスワード> go test ./internal/store/
```

```bash
PROVISIONER_TEST_CLIENT_CERT=<client.pem> PROVISIONER_TEST_PROV_URL=https://<host>:9444/admin/v1 PROVISIONER_TEST_PROV_SERVER_CERT=<prov-server.pem> PROVISIONER_TEST_AKA_URL=https://<host>:9443/admin/v1 PROVISIONER_TEST_AKA_SERVER_CERT=<aka-server.pem> go test -run Integration ./internal/provapi/ ./internal/akaapi/
```

契約テストは、テスト用の加入者・認可ポリシー（IMSI が `00101` で始まるもの）、RADIUSクライアント（`198.51.100.0/24` のアドレス）、AVクライアントを作り、終わったら削除します。aka-only-server には、起動前に AVクライアントを 1 つ登録しておいてください（`aka-only-server client add`）。

GitHub Actions（`.github/workflows/ci.yml`）で、push と pull request のたびに次を実行します。

| ジョブ | 内容 |
|---|---|
| テスト | gofmt の確認、`go vet`、race 検出つきのテスト（Valkey の結合テストを含む） |
| イメージと compose の設定 | Docker イメージのビルド、compose の設定の検査（下流が同一ホスト・別ホスト） |
| 下流との契約テスト | 本PoCと aka-only-server を固定のコミット（ci.yml の `POC_REF` / `AKA_REF`）で取得してビルド・起動し、`gen-client-cert` で作った証明書を登録して契約テストを実行する |

依存と標準ライブラリの既知の脆弱性は、`.github/workflows/govulncheck.yml` が push と pull request のたびと、毎週月曜に govulncheck で調べます。

## ライセンス

[LICENSE](LICENSE) を参照してください。
