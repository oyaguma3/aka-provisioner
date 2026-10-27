# aka-provisioner 設計概要（下書き）

- 作成日: 2026-10-26
- 状態: 下書き。§16 の確認事項は 2026-10-26 にすべて推奨案で合意した（§3.2 の決定事項 1〜8）。
- 置き場所: 2026-10-27（MVP セットの段階 1）に、aka-mvpset の `docs/nodes/aka-provisioner.md` から本リポジトリの `docs/design-overview.md` に移した。
- 対象: 管理面の中継のノード aka-provisioner（もとは eapaka-node-provisioner。以下「もとの実装」）。
- 関連:
  - aka-mvpset の[要件と構成の概要](https://github.com/oyaguma3/aka-mvpset/blob/main/docs/requirements-overview.md)（以下「概要」）: §5（配置）、§6.4（台帳と変更の経路）、§10（一括投入）、§12（鍵の経路、管理用の mTLS のピン）、§13（ログ）、§17.1（改修）
  - 課金レコード: [charging-records.md](https://github.com/oyaguma3/aka-mvpset/blob/main/docs/charging-records.md) §8.1（利用の集計の中継）
  - 下流の設計概要: [aka-vecgen](https://github.com/oyaguma3/aka-vecgen/blob/main/docs/design-overview.md)、[aka-radius](https://github.com/oyaguma3/aka-radius/blob/main/docs/design-overview.md)、radsec-ca の `docs/design-overview.md` §9、radsec-edge の `docs/design-overview.md` §12.1・§15・§23（段階 4）
  - もとの実装: eapaka-node-provisioner。今の作りは本リポジトリの [`docs/poc/design-overview.md`](poc/design-overview.md)（以下「もとの設計概要」）と `docs/openapi/provisioner-api.yaml`（0.3.0）

## 1. 位置づけ

```
[aka-ossbss] ──mTLS（X-Operator-Id / X-Trace-ID / Idempotency-Key）──> [aka-provisioner] ── Valkey（冪等と監査ログ）
                                                                          ├─mTLS（サービス用の WireGuard）─> aka-vecgen 管理API（データ面）
                                                                          ├─mTLS（サービス用の WireGuard）─> aka-radius 管理API（データ面）
                                                                          ├─mTLS（管理面のノードの中）────> radsec-ca 管理API
                                                                          └─mTLS（サービス用の WireGuard）─> radsec-edge 管理API（エッジ ×n）
```

- 管理面の aka-ossbss から、下流（aka-vecgen、aka-radius、radsec-ca、エッジ）への**変更の経路を 1 つにまとめる中継**。下流の管理クライアントは aka-provisioner だけにする（概要 §6.4）。
- 受け持つのは、下流への mTLS（1 つのクライアント証明書で全下流につなぐ）、再送の判定（`Idempotency-Key`）、操作者とトレースID の受け渡し、監査ログ、下流の状態のまとめ。
- 加入者・テナント・AP などのデータは持たない（正本は下流、台帳は aka-ossbss）。画面と権限も持たず、管理クライアント（aka-ossbss）を全権として扱う（もとの実装のまま）。
- もとの実装の中心だった「2 つのノードにまたがる加入者の統合操作と補償」は、MVP では vecgen と radius のデータが独立し（概要の決定事項 6）、aka-ossbss が別々に操作するので、なくす（§16 の D1）。

## 2. 範囲

| 区分 | 内容 |
|---|---|
| 対象 | aka-vecgen・aka-radius・radsec-ca・エッジ（複数）の管理API の中継（許した口だけ）、`Idempotency-Key`、監査ログ、`/status`、下流の登録簿と証明書の読み直し |
| 対象外 | 加入者の統合操作と補償（§16 の D1）、一括投入のジョブ（aka-ossbss の中で動かす。概要 §10.2）、中央の整合のループと望む状態（aka-ossbss。radsec-edge §15）、台帳、画面、操作者ごとの権限、冗長化 |

## 3. 決定事項

### 3.1 概要と他のノードで決めたこと

| # | 内容 | 出どころ |
|---|---|---|
| 1 | 加入者・認可ポリシーの変更の経路は aka-ossbss → aka-provisioner だけ。下流の管理クライアントは aka-provisioner だけ | 概要 §6.4 |
| 2 | vecgen と radius を別々に中継する（2 つのノードにまたがる取り消しをなくす）。一括取得・セッション・状態の操作・利用の集計を中継する。鍵の経路を限る | 概要 §17.1、§12.2、課金レコード §8.1 |
| 3 | 鍵（Ki・OPc）が通るのは、一括投入とシステム管理者の個別の登録（aka-vecgen の登録）だけ | 概要の決定事項 16、§12.2 |
| 4 | 管理クライアントのピンはファイルにして `SIGHUP` で読み直し、同じ識別名に新旧 2 つを書ける。監査ログは Stream と日ごとの JSONL | aka-vecgen の決定事項 7・9 |
| 5 | MVP で変える API は新しい大きい版にし、PoC セットとの互換は持たせない（`/admin/v2`） | 段階 0 の進め方 S5 |
| 6 | エッジと CA への中継は中身を解釈せず、冪等・監査ログ・操作者の付け替えだけを行う。望む状態は持たない | radsec-edge §15・§23.2 の 6 |
| 7 | aka-vecgen と aka-radius は `Idempotency-Key` を扱わず、再送の判定は aka-provisioner に任せる | aka-vecgen の決定事項 11、aka-radius §11.1 |

### 3.2 本書で決めたこと

| # | 内容 | 決定日 |
|---|---|---|
| 1 | 加入者の統合操作と補償をなくす。操作の記録・やり直しのワーカー・`/operations`・IMSI のロック・PLMN マップ・`issues` もなくす（§5。確認事項 D1） | 2026-10-26 |
| 2 | 中継は `/admin/v2/{vecgen,radius,ca}/...` と `/admin/v2/edges/{edgeId}/...` の形で、下流ごとに許す口の一覧（allowlist）を持つ。本文は解釈しない。aka-vecgen の鍵の読み出しは通さない（§6。D2） | 2026-10-26 |
| 3 | 下流を書き換える要求では `Idempotency-Key` を必須にする。radsec-ca とエッジには受け取ったキーをそのまま渡す（§7。D3） | 2026-10-26 |
| 4 | 下流の登録簿（vecgen・radius・ca・エッジ）とクライアント証明書をファイルにし、`SIGHUP` で読み直す。radsec-edge §23.2 の 6 の環境変数 `PROVISIONER_EDGES` を改める。radsec-ca と radsec-edge の管理クライアントのピンも、ファイルと `SIGHUP` にそろえる（段階 4 で直す）（§8。D4） | 2026-10-26 |
| 5 | module パスは `github.com/oyaguma3/aka-provisioner`、コマンド名は `aka-provisioner`、環境変数の接頭辞はもとの `PROVISIONER_` のまま（§4。D5） | 2026-10-26 |
| 6 | 監査ログは下流を書き換える中継が成功したときに残し、`action` は `<下流>.<口の名前>`。Valkey の Stream（上限 100,000 件）と日ごとの JSONL に書く（§10。D6） | 2026-10-26 |
| 7 | 管理面のノードのログも、データ面と同じく Fluent Bit でファイルに書き、日ごとに圧縮して 2 週間持つ。aka-ossbss・radsec-ca も同じ Fluent Bit に送る（§10。D7） | 2026-10-26 |
| 8 | aka-provisioner は RADIUS クライアントの書き込みと共有シークレットの読み出しを中継しない。radsec-edge §14.1 の 4 を、データ面のノードで aka-radius のコマンドで登録する手順に改める（段階 4 で直す）（§5、§15。D8） | 2026-10-26 |

## 4. 技術方針

- もとの実装の方針を引き継ぐ。Go 1.27.3、標準ライブラリを優先、外部依存は `github.com/valkey-io/valkey-go` だけ。
- module パスは `github.com/oyaguma3/aka-provisioner`、コマンド名は `aka-provisioner`、環境変数の接頭辞は `PROVISIONER_`（もとのまま。§16 の D5）。
- 配備は Docker Compose（aka-provisioner と専用の Valkey）。管理面のノードに aka-ossbss・radsec-ca と並べる。
- ブランチは `main` に直接コミットする（もとの実装と同じ）。

## 5. もとの実装からなくすもの

| もとの実装 | MVP | 理由 |
|---|---|---|
| 加入者の統合リソース（`keyStore`、`key`、`policy`、`status`、`issues`）と、作成・変更・削除の補償 | なくす。aka-ossbss が vecgen 側と radius 側を別々に操作する | データが独立した（概要の決定事項 6）。1 つの操作は 1 つの下流で完結する |
| 操作の記録（`op:`、`ops:active`）、やり直しのワーカー、`/operations` | なくす | 補償の要る操作がなくなる。エッジと CA は宣言的で、ずれは aka-ossbss の整合のループが直す |
| IMSI ごとのロック | なくす | 下流の 1 回の操作は下流の中で原子的（Lua）。複数の手順の間の排他が要らなくなる |
| PLMN マップと鍵の置き場所の決め方、vector-gateway の AV クライアント ID の確かめ | なくす | 取得先は IMSI ごとに aka-radius が持つ。AV クライアントは一括投入の設定で aka-ossbss が選ぶ |
| 本PoC の Provisioning API・aka-only-server の管理API 0.2.0 の型つきのクライアント | なくし、口の一覧つきの中継に統一する | 下流の中身を解釈しない |
| `/clients` の書き込みと `/clients/{id}/secret` の中継 | なくす | RADIUS クライアントは aka-radius のコマンドで管理する（aka-radius の決定事項 6。§16 の D8） |
| `/subscribers/{imsi}/keys` の中継 | なくす | 鍵の読み出しは aka-vecgen で既定で無効。管理面を通さない（概要 §12.2） |

## 6. 中継

### 6.1 口の形

すべて `/admin/v2` の下。下流の名前をパスの最初に置き、残りを下流のパスとして送る。

| aka-provisioner のパス | 下流 |
|---|---|
| `/vecgen/...` | aka-vecgen の `/admin/v2/...` |
| `/radius/...` | aka-radius の `/admin/v2/...` |
| `/ca/...` | radsec-ca の `/...`（パスに接頭辞を付けない作り。radsec-edge §23.2 の 2） |
| `/edges/{edgeId}/...` | そのエッジの `/...` |
| `/status`、`/audit-logs` | aka-provisioner 自身（§9、§10） |

### 6.2 許す口の一覧

下流ごとに、許すメソッドとパスの形の一覧（allowlist）を aka-provisioner のコードに持ち、それ以外は 404（`ROUTE_NOT_ALLOWED`）にする（§16 の D2）。下流を変えても aka-provisioner を変えずに済む「何でも通す」作りにしないのは、鍵の経路を限り（概要 §12.2）、管理面から変えてはいけない操作を通さないため。

| 下流 | 許す口（要点） | 通さない口 |
|---|---|---|
| aka-vecgen | `GET /status`、加入者の一覧・一括取得・登録・取得・変更・状態・戻し・削除、`GET /clients`、`GET /av-server-certificate`、`GET /audit-logs` | `GET /subscribers/{imsi}/keys` |
| aka-radius | `GET /status`、加入者の一覧・一括取得・登録・取得・変更・状態・戻し・削除、`GET /sessions`、`GET /usage`、`GET /clients`、`GET /sources`、`GET /audit-logs` | — （書き込みの口のない RADIUS クライアントと取得先は、もともと下流にない） |
| radsec-ca | radsec-ca §9.3 の口（CA の作成と停止、発行、失効、一覧、CRL、`/status`）と、§19 の持ち込みの CA の口 | — |
| エッジ | radsec-edge §12.1 の口（テナント、AP、CSR と証明書、反映、CRL の取り直し、再起動、ログ、最後の接続、`/status`） | — |

- 一覧は、下流の OpenAPI と突き合わせる試験で、口の書き漏れと余計な口を見つける（§13）。
- パスの値（IMSI、ID、名前）は、形を確かめ、1 つのセグメントとしてエスケープしてから送る（もとの実装のまま。`/a%2Fb` が別の口として届かないように）。

### 6.3 中継の作法

- 要求の本文は解釈せずに送り、下流の応答（エラーを含む）をそのまま返す。ProblemDetails には拡張の項目 `downstream`（`vecgen` / `radius` / `ca` / `edge:<ID>`）を足す。`Location` は aka-provisioner のパスに書き換える。
- 下流に接続できない・タイムアウトは 503（`DOWNSTREAM_UNAVAILABLE`）、下流が ProblemDetails でない応答を返したら 502（`DOWNSTREAM_ERROR`）。どちらも `downstream` つき（もとの実装のまま）。
- `X-Operator-Id` と `X-Trace-ID` は、受け取った値をそのまま下流に渡す（`X-Trace-ID` がなければ採番する）。一括投入の行の操作では、aka-ossbss が `X-Trace-ID` にジョブの ID を入れる（概要 §10.4）。
- 下流の呼び出しのタイムアウトは `PROVISIONER_DOWNSTREAM_TIMEOUT`（既定 5 秒）。もとの実装は 1 つの操作で下流を 5 回まで呼んだが、MVP では 1 回だけなので、ロックの有効期限との兼ね合いの制限（10 秒まで）はなくす。
- 本文の大きさの上限は 1 MiB（一括取得の 500 件の応答、CSR、CRL を通せるように）。
- 要求と応答の本文はログに出さない（鍵を含みうるため）。

## 7. 再送の判定（Idempotency-Key）

- 下流を書き換える要求（`POST`・`PUT`・`PATCH`・`DELETE`）には `Idempotency-Key` を必須にする（なければ 400 `MANDATORY_IE_MISSING`。§16 の D3）。読み出しには付けない。
- 判定の作りはもとの実装のまま: 同じ管理クライアントから同じキーで届いた要求は 24 時間のあいだ最初の応答を返す（`Idempotent-Replayed: true`）。処理中なら 409（`OPERATION_IN_PROGRESS`）、内容が違えば 422（`IDEMPOTENCY_KEY_MISMATCH`）。覚えるのは 2xx と 4xx だけで、5xx は覚えない。
- radsec-ca とエッジは自分でも `Idempotency-Key` を必須にしている（radsec-ca §9.2、radsec-edge §23.2 の 2）。aka-provisioner は受け取ったキーをそのまま下流に渡す（下流の判定と重なるが、aka-provisioner の Valkey が消えても下流で二重の発行を防げる）。
- aka-vecgen と aka-radius にはキーを渡さない（扱わないため）。
- 一括投入の行の `Idempotency-Key` は、aka-ossbss がジョブの ID と IMSI から作る（概要 §10.3）。
- Valkey に残すのは要求のハッシュと応答だけ。登録の要求の本文（鍵を含む）は残さない。応答は鍵を含まない（下流が返さない）。

## 8. 下流の登録簿と証明書

### 8.1 ファイル

下流の登録簿をファイル（`PROVISIONER_DOWNSTREAMS_FILE`。JSON）に置き、`SIGHUP` で読み直す（§16 の D4）。

```json
{
  "clientCert": "/run/secrets/client.pem",
  "clientKey": "/run/secrets/client.key",
  "vecgen": { "url": "https://10.x.x.x:9443/admin/v2", "serverCert": "/run/secrets/vecgen-admin.pem" },
  "radius": { "url": "https://10.x.x.x:9444/admin/v2", "serverCert": "/run/secrets/radius-admin.pem" },
  "ca":     { "url": "https://radsec-ca:9448", "serverCert": "/run/secrets/ca-admin.pem" },
  "edges": [
    { "id": "edge-01", "url": "https://10.x.x.x:9447", "serverFingerprint": "…" }
  ]
}
```

- 読み直しで、ファイルが読めない・形が違う・証明書が読めない場合は、前の登録簿のまま続け、ログに出す。
- エッジを足すときは、エッジの導入（radsec-edge §14.1）の後にこのファイルに足して `SIGHUP` を送る（aka-provisioner を止めない）。radsec-edge §23.2 の 6 で決めた環境変数 `PROVISIONER_EDGES` は、このファイルに改める。
- 管理クライアントのピンも、同じく `PROVISIONER_ADMIN_CLIENTS_FILE` に置いて `SIGHUP` で読み直す（§3.1 の 4）。
- 読み直しは `docker compose kill -s HUP aka-provisioner`。

### 8.2 クライアント証明書の入れ替え

管理用の mTLS のピンの入れ替え（概要 §12.3）は、次の順で行う。手順は、ノードを横断する運用手順書に書く。

1. 新しいクライアント証明書を作る（`aka-provisioner gen-client-cert`）。
2. 全下流のピンのファイルに、新旧の 2 つを並べて書き、それぞれ `SIGHUP` で読み直す（aka-vecgen・aka-radius はファイル、radsec-ca・エッジは段階 4 でそろえる。§16 の D4）。
3. aka-provisioner の登録簿のクライアント証明書を新しいものに替えて `SIGHUP` を送る。
4. `/status` で全下流につながることを確かめてから、下流のピンのファイルから古いものを消す。

## 9. 状態（`GET /admin/v2/status`）

- aka-provisioner の版、起動日時、Valkey、登録簿と管理クライアントのピンの読み直しの日時、クライアント証明書の期限。
- 下流ごとの状態: つながるか（最後に確かめた時刻）、下流の `/status` の要点（版、加入者の数、証明書の期限など）、サーバー証明書の期限。下流の `/status` は 30 秒ごとに集めて覚えておき、要求のたびには呼ばない（監視が頻繁に呼んでも下流に負荷をかけない）。
- この口は mTLS なので、監視ノードの blackbox_exporter からは呼べない。コンテナの healthcheck の口はループバックに置き（aka-radius の決定事項 13 と同じ）、下流の状態の監視は aka-ossbss の画面と、aka-provisioner のログの `event_id`（`DOWNSTREAM_UNREACHABLE` など）から行う。

## 10. ログと監査ログ

- ログは JSON（`log/slog`）で標準出力に出す。`event_id` と `trace_id` を付ける（例: `RELAY_COMPLETED`、`RELAY_DENIED`、`DOWNSTREAM_UNREACHABLE`、`DOWNSTREAMS_RELOADED`、`ADMIN_CLIENTS_RELOADED`、`IDEMPOTENT_REPLAYED`）。要求と応答の本文は出さない。
- 監査ログは、下流を書き換える中継が成功したとき（2xx）に残す。項目はもとの実装と同じ（時刻、操作者、管理クライアント、`action`、対象、トレースID、`details`）。`action` は `<下流>.<口の名前>`（例: `vecgen.subscriber.create`、`radius.subscriber.state`、`ca.cert.issue`、`edge.tenant.put`）。`details` は下流の名前と、変えた項目の名前だけ（値は残さない。もとの実装のまま）。
- 置き場所は Valkey の Stream（件数の上限つき）と日ごとの JSONL のファイル（`PROVISIONER_AUDIT_DIR`）。aka-vecgen・aka-radius と同じ形にする（§3.1 の 4）。
- 一括投入では 1 行ごとに監査ログが残る（1 回 1 万件）。Stream の上限は 100,000 件にする（もとの実装は 10,000 件）。
- ログの経路は、管理面のノードでもデータ面と同じく Fluent Bit でファイルに書き、日ごとに圧縮して 2 週間持つ（§16 の D7。aka-radius の決定事項 10）。

## 11. 設定（環境変数）

| 名前 | 内容 | 既定 |
|---|---|---|
| `PROVISIONER_ADDR` | 待ち受け | `:9446` |
| `PROVISIONER_TLS_CERT` / `_TLS_KEY` / `_TLS_HOSTS` | サーバー証明書（なければ自己署名を作る） | もとのまま |
| `PROVISIONER_ADMIN_CLIENTS_FILE` | 管理クライアントのピンのファイル | `/run/secrets/admin-clients` |
| `PROVISIONER_DOWNSTREAMS_FILE` | 下流の登録簿のファイル（§8.1） | `/run/secrets/downstreams.json` |
| `PROVISIONER_DOWNSTREAM_TIMEOUT` | 下流の 1 回の呼び出しのタイムアウト | `5s` |
| `PROVISIONER_VALKEY_ADDR` / `_VALKEY_PASSWORD` | 専用の Valkey | もとのまま |
| `PROVISIONER_AUDIT_MAX` / `PROVISIONER_AUDIT_DIR` | 監査ログの Stream の上限、JSONL の書き出し先 | `100000` / `/data/audit` |
| `PROVISIONER_LOG_LEVEL` | ログの段階 | `info` |

- なくす: `PROVISIONER_ADMIN_CLIENTS`（ファイルへ）、`PROVISIONER_CLIENT_CERT` / `_CLIENT_KEY`・`_PROV_URL`・`_AKA_URL` などの下流ごとの変数（登録簿へ）、`PROVISIONER_PLMN_MAP`、`PROVISIONER_AKA_AV_CLIENT_ID`。
- コマンド: `aka-provisioner serve`（既定）、`gen-client-cert`、`server-cert`、`check-downstream`（登録簿の全下流の `/status` を確かめる）、`healthcheck`。

## 12. 配備

- 管理面のノードの compose に、aka-provisioner と専用の Valkey を置く。aka-ossbss との間は共有の Docker のネットワーク（もとの実装の共有のネットワーク）でつなぎ、ホストに公開しない。
- radsec-ca とは、同じノードの中の共有の Docker のネットワークでつなぐ。aka-vecgen・aka-radius・エッジには、サービス用の WireGuard のアドレスでつなぐ（下流のサーバー証明書の SAN にそのアドレスを入れる）。
- Valkey は AOF の `appendfsync always`（もとのまま。冪等の記録を失わないため）。

## 13. 試験

| 層 | 内容 |
|---|---|
| 単体 | 口の一覧（許す口・通さない口・パスの値のエスケープ）、冪等（必須、処理中、内容の違い、5xx を覚えない）、登録簿とピンの読み直し（形の誤りで前の設定が残る）、`Location` の書き換え、ProblemDetails の `downstream` |
| 契約 | 下流（aka-vecgen、aka-radius、radsec-ca、radsec-edge）を固定のコミットで CI で起動し、許す口の一覧の全部を呼んで、下流の OpenAPI と口が合っていること、操作者・トレースID が下流の監査ログに残ることを確かめる。エッジと CA は段階 4-2 で足す |
| 規模 | aka-ossbss の一括投入 1 万件（毎秒 5.6 件以上）を、冪等の記録と監査ログを含めて通せること（概要 §16.1） |

## 14. 進め方

| 段階 | 内容 | 終わりの確かめ |
|---|---|---|
| 1 | リポジトリ `aka-provisioner` を作り、eapaka-node-provisioner の履歴ごと移す。改名だけ | 今の CI が通る |
| 3-1 | 統合操作・操作の記録・ワーカー・ロック・PLMN マップ・型つきのクライアントをなくし、口の一覧つきの中継（`/admin/v2/vecgen/...`、`/radius/...`）にする。`Idempotency-Key` の必須化、登録簿とピンのファイル、監査ログの書き出し、`/status` | 単体、aka-vecgen・aka-radius との契約の試験 |
| 3-2 | 運用ガイド（クライアント証明書の入れ替え、エッジの足し方） | 手元の compose |
| 4-2 | radsec-ca とエッジの中継を足す（radsec-edge §23.1 の 4-2） | 契約の試験（エッジと radsec-ca を CI で起動） |

- 段階 3-1 は aka-vecgen・aka-radius の管理API 2.0.0（それぞれの段階 2）の後に行う。

## 15. 他の文書への反映

次を直す（決定事項 4・8。radsec-edge・radsec-ca の本文は、段階 4 に入るときにまとめて直す。概要 §18.1 の注）。

| 文書 | 直すところ |
|---|---|
| radsec-edge の設計概要 | §14.1 の 4（RADIUS クライアントの登録は、データ面のノードで aka-radius のコマンドで行う。§16 の D8）、§23.2 の 6（エッジの一覧は環境変数ではなく登録簿のファイル。§16 の D4）、§16 の provisioner の行 |
| radsec-ca・radsec-edge の設計概要 | 管理クライアントのピンをファイルにして `SIGHUP` で読み直す（`CA_ADMIN_CLIENTS`・`EDGE_ADMIN_CLIENTS` の環境変数を改める。§16 の D4） |

## 16. 確認事項（2026-10-26 にすべて推奨案で合意。§3.2 の決定事項 1〜8）

| # | 事項 | 推奨 | 理由 |
|---|---|---|---|
| D1 | 統合操作と補償 | なくす。操作の記録・ワーカー・`/operations`・IMSI のロック・PLMN マップ・`issues` もなくす（§5） | vecgen と radius のデータが独立し、1 つの操作が 1 つの下流で完結する。エッジと CA は宣言的で、ずれは aka-ossbss の整合のループが直す。もとの実装の大半の複雑さ（補償、やり直し、鍵の変更が分からない変更）がなくなる |
| D2 | 中継の形 | `/admin/v2/{vecgen,radius,ca}/...` と `/admin/v2/edges/{edgeId}/...` の形で、下流ごとに許す口の一覧（allowlist）を持つ。本文は解釈しない。aka-vecgen の鍵の読み出しは通さない（§6） | 下流ごとに型つきのクライアントを書くより小さく、下流の口が増えても一覧に足すだけで済む。「何でも通す」にしないことで、鍵の経路と管理面から変えてはいけない操作を aka-provisioner で絞れる |
| D3 | `Idempotency-Key` | 書き込みでは必須にする。radsec-ca とエッジには受け取ったキーをそのまま渡す（§7） | aka-vecgen・aka-radius は再送の判定を aka-provisioner に任せたので、付け忘れを入口で断る。aka-ossbss はいつも付ける（一括投入はジョブの ID と IMSI から作る） |
| D4 | 下流の登録簿とピン | 下流の登録簿（vecgen・radius・ca・エッジ）とクライアント証明書をファイルにし、`SIGHUP` で読み直す。radsec-edge §23.2 の 6 の環境変数 `PROVISIONER_EDGES` を改める。radsec-ca と radsec-edge の管理クライアントのピンも、同じくファイルと `SIGHUP` にそろえる（段階 4 で直す） | エッジは 2 台ずつ増えていくので、足すたびに aka-provisioner を止めたくない。クライアント証明書の入れ替え（概要 §12.3）を、どのノードも再起動なしで同じ手順で行えるようにする |
| D5 | 名前 | module `github.com/oyaguma3/aka-provisioner`、コマンド `aka-provisioner`、接頭辞はもとの `PROVISIONER_` のまま | `PROVISIONER_` は他のノードと紛れない |
| D6 | 監査ログ | 書き込みの中継が成功したときに残し、`action` は `<下流>.<口の名前>`。Stream（上限 100,000 件）と日ごとの JSONL（§10） | 一括投入 1 回で 1 万行の記録が残るので、上限を上げる。形は aka-vecgen・aka-radius とそろえる |
| D7 | 管理面のログの経路 | データ面と同じく Fluent Bit でファイルに書き、日ごとに圧縮して 2 週間持つ。管理面のノードの aka-ossbss・radsec-ca も同じ Fluent Bit に送る（§10） | 監視用の PC がどのノードからも同じ形で取りに行けるようにする（概要 §13）。aka-ossbss の設計概要でも確かめる |
| D8 | RADIUS クライアントの登録の流れ | aka-provisioner は `/clients` の書き込みと共有シークレットの読み出しを中継しない。radsec-edge §14.1 の 4（OSS/BSS から provisioner の `/clients` で登録する）を、データ面のノードで aka-radius のコマンドで登録する手順に改める（段階 4 で直す） | aka-radius の決定事項 6 に合わせる。エッジを足すときは WireGuard のピアの設定でデータ面のノードに入るので、同じ機会に登録できる。共有シークレットが管理面を通らない |
