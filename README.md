# aka-provisioner

[![CI](https://github.com/oyaguma3/aka-provisioner/actions/workflows/ci.yml/badge.svg)](https://github.com/oyaguma3/aka-provisioner/actions/workflows/ci.yml)
[![govulncheck](https://github.com/oyaguma3/aka-provisioner/actions/workflows/govulncheck.yml/badge.svg)](https://github.com/oyaguma3/aka-provisioner/actions/workflows/govulncheck.yml)

> このリポジトリは、VPS に分けて置いて顧客を収容する **MVP セット**（[aka-mvpset](https://github.com/oyaguma3/aka-mvpset)）の、管理面の中継のノードです。PoC セット（[eapaka-pocset](https://github.com/oyaguma3/eapaka-pocset) の `pocset-v1.0.1`）の [eapaka-node-provisioner](https://github.com/oyaguma3/eapaka-node-provisioner) の履歴を引き継いで作りました（もとのコミット `0ca238d`）。
>
> MVP のための改修は、[設計概要](docs/design-overview.md)のとおりに MVP セットの段階 3・4 で行っています。段階 3-1 で、加入者の統合操作と補償をなくし、許した口だけを通す中継（`/admin/v2`）にしました。radsec-ca とエッジへの中継は段階 4-2 で足します。

管理面の aka-ossbss から、下流（[aka-vecgen](https://github.com/oyaguma3/aka-vecgen) と [aka-radius](https://github.com/oyaguma3/aka-radius) の管理API 2.0.0）への変更の経路を 1 つにまとめる中継です（コマンド名 `aka-provisioner`）。

```
[aka-ossbss] ──mTLS（X-Operator-Id / X-Trace-ID / Idempotency-Key）──> [aka-provisioner] ── Valkey（専用。冪等と監査ログ）
                                                                          ├─mTLS（サービス用の WireGuard）─> aka-vecgen の管理API
                                                                          └─mTLS（サービス用の WireGuard）─> aka-radius の管理API
```

## 特徴

- `/admin/v2/vecgen/...` と `/admin/v2/radius/...` を、下流の `/admin/v2/...` にそのまま中継します。許すのは決めた口だけで（口の一覧は [API 仕様](docs/openapi/provisioner-api.yaml)）、それ以外は 404（`ROUTE_NOT_ALLOWED`）です。aka-vecgen の鍵の読み出しと、RADIUS クライアントの書き込みは通しません。
- 本文は解釈せず、下流の応答（エラーを含む）をそのまま返します。下流の ProblemDetails には `downstream` を足し、`Location` は aka-provisioner のパスに書き換えます。
- 下流を書き換える要求では `Idempotency-Key` が必須です。同じキーの再送には 24 時間のあいだ最初の応答を返します（aka-vecgen・aka-radius は再送の判定を aka-provisioner に任せています）。
- 操作者（`X-Operator-Id`）とトレースID（`X-Trace-ID`）を下流に渡すので、aka-provisioner と下流の監査ログを突き合わせられます。
- 下流を書き換えた中継は監査ログに残します（Valkey の Stream と日ごとの JSONL。`action` は `vecgen.subscriber.create` のような `<下流>.<口>`、値は残しません）。
- 下流の登録簿・管理クライアントのピン・サーバー証明書はファイルに置き、`SIGHUP` で読み直します。
- 加入者のデータは持ちません（正本は下流、台帳は aka-ossbss）。Ki・OPc はログにも監査ログにも出しません。

| API（`/admin/v2`） | 内容 |
|---|---|
| `/status` | aka-provisioner の版・証明書・読み直しの日時と、下流ごとの状態（30 秒ごとに集めた下流の `/status`） |
| `/audit-logs` | aka-provisioner の監査ログ |
| `/vecgen/...` | aka-vecgen の管理API の中継（加入者、AV クライアントの読み出し、AV 用のサーバー証明書、監査ログ） |
| `/radius/...` | aka-radius の管理API の中継（加入者、セッション、利用の集計、RADIUS クライアントと取得先の読み出し、監査ログ） |

## 導入

Docker Engine と compose プラグインが要ります。aka-vecgen と aka-radius を導入しておき、aka-provisioner を両方の管理クライアントとして登録します。詳しい手順と注意（下流とのつなぎ方、証明書の入れ替え、Fluent Bit、公開範囲、バックアップと戻し、更新、障害時の確かめ）は[運用ガイド](docs/operation-guide.md)にあります。

1. `.env.example` を `.env` にコピーし、専用の Valkey のパスワード（`PROVISIONER_VALKEY_PASSWORD`）を書きます。
2. 管理クライアント（aka-ossbss）のピンのファイル `secrets/admin-clients` を書きます。1 行に `識別名=クライアント証明書の SHA-256 のフィンガープリント` で、同じ識別名に新旧 2 つを書けます。1 つもないと起動しません。**コンテナを一度でも動かす前に作ります**（ないと Docker が同じ名前のディレクトリを作ります）。監査ログのディレクトリ `audit/` も作ります。
3. 下流に提示するクライアント証明書を作り、表示された 1 行（`provisioner=<フィンガープリント>`）を、aka-vecgen と aka-radius の管理クライアントのピンのファイルに足して、それぞれ `SIGHUP` で読み直します。標準出力には証明書と秘密鍵が続けて出るので、そのまま 1 つのファイルにして、登録簿の `clientCert` と `clientKey` の両方に書きます。

```bash
docker compose build
```

```bash
docker compose run --rm --no-deps -T aka-provisioner gen-client-cert -name provisioner > downstreams/client.pem
```

4. 下流の登録簿 `downstreams/downstreams.json` を書きます（見本は `downstreams/downstreams.example.json`）。下流の管理API のサーバー証明書（aka-vecgen は `aka-vecgen admin-cert` で出すもの、aka-radius は `deployments/certs/admin/server.pem`）を同じディレクトリに置きます。パスは登録簿のファイルからの相対で書けます。

| 項目 | 内容 |
|---|---|
| `clientCert`・`clientKey` | aka-provisioner のクライアント証明書と秘密鍵（全下流に提示する。同じファイルでよい） |
| `vecgen.url`・`radius.url` | 下流の管理API の基底 URL（`https://<ホスト>:<ポート>/admin/v2`。サーバー証明書の SAN にあるホスト名か IP） |
| `vecgen.serverCert`・`radius.serverCert` | 下流の管理API のサーバー証明書（自己署名。信頼の起点にする） |

5. コンテナは UID 65532 で動くので、`downstreams/`・`secrets/admin-clients`・`audit/` をこの UID で読み書きできるようにします。

```bash
sudo chown -R 65532:65532 downstreams secrets/admin-clients audit
```

```bash
sudo chmod 600 downstreams/client.pem secrets/admin-clients
```

6. 起動して、下流につながることを確かめます。

```bash
docker compose up -d
```

```bash
docker compose exec -T aka-provisioner /aka-provisioner check-downstream
```

7. `server-cert` でサーバー証明書を取り出し、aka-ossbss に渡します。同じホストの aka-ossbss は共有ネットワーク `aka-provisioner` に参加して `https://aka-provisioner:9446/admin/v2` で接続します。

```bash
docker compose exec -T aka-provisioner /aka-provisioner server-cert > provisioner-server.pem
```

下流の登録簿・ピン・サーバー証明書を直したら、`docker compose kill -s HUP aka-provisioner` で読み直します（ログの `*_RELOADED`。読めなければ前のまま続け、`*_RELOAD_ERR`）。下流と同じホストで試すときは、`compose.aka-radius-admin.yaml`・`compose.aka-av.yaml` を重ねます（`.env` の `COMPOSE_FILE`）。

ログは管理面の Fluent Bit（同じ compose）を通して `logs_on_host/` のファイルに書きます。aka-ossbss のログも、aka-ossbss の重ね合わせでここに送ります。ファイルはホストの logrotate で日ごとに圧縮し、2 週間持ちます。

### 環境変数

| 名前 | 内容 | 既定 |
|---|---|---|
| `PROVISIONER_ADDR` | API の待ち受け | `:9446` |
| `PROVISIONER_TLS_CERT`・`_TLS_KEY`・`_TLS_HOSTS` | API のサーバー証明書と鍵（なければ自己署名を作る）、自己署名の SAN | `/data/tls/cert.pem`・`/data/tls/key.pem`・`localhost,127.0.0.1,aka-provisioner` |
| `PROVISIONER_ADMIN_CLIENTS_FILE` | 管理クライアントのピンのファイル | `/run/secrets/admin-clients` |
| `PROVISIONER_DOWNSTREAMS_FILE` | 下流の登録簿のファイル | `/etc/aka-provisioner/downstreams/downstreams.json` |
| `PROVISIONER_DOWNSTREAM_TIMEOUT` | 下流の 1 回の呼び出しのタイムアウト（30 秒まで） | `5s` |
| `PROVISIONER_VALKEY_ADDR`・`_VALKEY_PASSWORD` | 専用の Valkey | `valkey:6379`・（必須） |
| `PROVISIONER_AUDIT_MAX` | 監査ログの Stream の件数の上限（おおよそ） | `100000` |
| `PROVISIONER_AUDIT_DIR`・`_AUDIT_RETENTION_DAYS` | 監査ログの日ごとのファイルの置き場所と、持つ日数 | `/data/audit`・`14` |
| `PROVISIONER_HEALTH_ADDR` | ループバックの healthcheck の口（`GET /healthz`。Valkey に届くか） | `127.0.0.1:9090` |
| `PROVISIONER_LOG_LEVEL` | ログの段階 | `info` |

compose の `.env` に書く値（公開先 `PROVISIONER_PUBLISH`、共有ネットワークの名前、Fluent Bit のポートなど）は `.env.example` にあります。

### コマンド

| コマンド | 内容 |
|---|---|
| `serve` | サーバーを起動する（既定） |
| `check-downstream` | 下流の登録簿を読み、全下流の `/status` に接続できるか確かめる（つながらなければ原因の見当を出す） |
| `gen-client-cert -name <識別名>` | 下流に提示するクライアント証明書と秘密鍵を作り、下流のピンのファイルに書く 1 行を出す |
| `server-cert` | API のサーバー証明書を出す（なければ自己署名を作る） |
| `healthcheck` | ループバックの healthcheck の口に問い合わせる（compose の healthcheck） |

Valkey のバックアップと戻しは `scripts/backup-valkey.sh`・`scripts/restore-valkey.sh` です（運用ガイド 8 章）。

## ドキュメント

| ファイル | 内容 |
|---|---|
| [docs/design-overview.md](docs/design-overview.md) | 設計概要（MVP。決定事項と段階の記録） |
| [docs/openapi/provisioner-api.yaml](docs/openapi/provisioner-api.yaml) | API 仕様（2.0.0。中継の口の一覧と、自分の口） |
| [docs/operation-guide.md](docs/operation-guide.md) | 運用ガイド（導入、下流と管理クライアント、Fluent Bit、公開範囲、ログと監査ログ、バックアップと戻し、更新、障害時の確かめ） |

## 開発

Go 1.27.3 を使います。外部依存は `github.com/valkey-io/valkey-go` だけです。

```bash
go test ./...
```

Valkey を使う結合の試験は、接続先を環境変数で指定したときだけ動きます。論理データベース 1 番の全データを消すので、専用の Valkey を使ってください。

```bash
PROVISIONER_TEST_VALKEY_ADDR=127.0.0.1:16391 go test ./internal/store/
```

下流との契約の試験は、本物の aka-vecgen・aka-radius と aka-provisioner を `scripts/contract.sh` でループバックに起動してから流します（Valkey は Docker で立てます）。下流は aka-mvpset の `versions.env` の固定のコミットからビルドします。

```bash
scripts/contract.sh up <バイナリのディレクトリ> <作業ディレクトリ>
```

```bash
. <作業ディレクトリ>/contract.env && go test -count=1 ./internal/contract/
```

```bash
scripts/contract.sh down <作業ディレクトリ>
```

GitHub Actions（`.github/workflows/ci.yml`）で、push と pull request のたびに次を実行します。

| ジョブ | 内容 |
|---|---|
| テスト | gofmt の確認、`go vet`、race 検出つきの試験（Valkey の結合の試験を含む） |
| イメージと compose の設定 | イメージのビルド、compose の設定の検査（下流が別ホスト・同一ホスト）、compose での起動と healthcheck、Fluent Bit のファイルへの書き込み |
| 運用ガイドの手順の通し確認 | compose で起動し、mTLS で `/status`、`check-downstream`、管理クライアントのピンの入れ替えと `SIGHUP`、Valkey のバックアップと戻し |
| 下流との契約の試験 | aka-vecgen・aka-radius を固定のコミット（ci.yml の `VECGEN_REF`・`RADIUS_REF`）でビルドし、口の一覧と下流の OpenAPI を突き合わせ、`scripts/contract.sh` で起動して契約の試験を流す |

依存と標準ライブラリの既知の脆弱性は、`.github/workflows/govulncheck.yml` が push と pull request のたびと、毎週月曜に govulncheck で調べます。

## ライセンス

[LICENSE](LICENSE) を参照してください。
