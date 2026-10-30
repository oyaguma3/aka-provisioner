# 運用ガイド

- 対象: aka-provisioner を MVP セットの管理面の VPS で動かす人。
- 関連: [README](../README.md)（導入の流れ、コマンドと設定の一覧）、[設計概要](design-overview.md)（決定事項と段階の記録）、[API 仕様](openapi/provisioner-api.yaml)、aka-ossbss の[運用ガイド](https://github.com/oyaguma3/aka-ossbss/blob/main/docs/operation-guide.md)、aka-vecgen の[運用ガイド](https://github.com/oyaguma3/aka-vecgen/blob/main/docs/operation-guide.md)、aka-radius の[運用ガイド](https://github.com/oyaguma3/aka-radius/blob/main/docs/operation-guide.md)

## 1. 構成と置き場所

`docker compose` で 3 つのコンテナを動かす。

| コンテナ | 内容 |
|---|---|
| `aka-provisioner` | 中継。UID 65532 で動く（distroless）。`docker compose ps` で healthy なら、専用の Valkey に届いている |
| `valkey` | `Idempotency-Key` の記録（24 時間）と監査ログの Stream。AOF を書き込みごとに同期する。ホストにもほかのプロジェクトにもポートを公開しない |
| `fluent-bit` | 管理面のログをアプリごとのファイル（`logs_on_host/`）に書く。aka-ossbss のログも受ける（5 章） |

| 口 | ポート | つなぐ相手 | 認証 |
|---|---|---|---|
| API | 9446 | aka-ossbss（同じ VPS の共有の Docker ネットワーク `aka-provisioner`） | mTLS。管理クライアントのピン（ファイル） |
| healthcheck | `127.0.0.1:9090`（コンテナの中） | コンテナの中だけ | なし（Valkey に届くか） |
| 下流への接続 | aka-vecgen の 9443、aka-radius の 9444 | データ面の VPS（サービス用の WireGuard） | mTLS。1 つのクライアント証明書を全下流に登録する |

```
[aka-ossbss] ──mTLS（aka-provisioner）──> 9446 aka-provisioner ── Valkey
                                               ├─mTLS（WireGuard）─> aka-vecgen 9443
                                               └─mTLS（WireGuard）─> aka-radius 9444
[運用者] ──SSH（運用用の WireGuard と踏み台）──> コマンド（check-downstream、gen-client-cert など）
```

radsec-ca とエッジへの中継は、段階 4-2 で足す。

ノードの上に置くもの（`compose.yaml` のあるディレクトリ）:

| ファイル | 内容 | 持ち主と権限 |
|---|---|---|
| `.env` | Valkey のパスワードと設定 | 運用者、600 |
| `secrets/admin-clients` | 管理クライアント（aka-ossbss）のピン（4 章）。Docker の secrets で渡す | UID 65532、600 |
| `downstreams/` | 下流の登録簿 `downstreams.json`、クライアント証明書と秘密鍵、下流のサーバー証明書（3 章） | UID 65532。秘密鍵は 600 |
| `audit/` | 監査ログの日ごとのファイル（7.2） | UID 65532 |
| `logs_on_host/` | アプリごとのログのファイル（5 章） | root（Fluent Bit が書く） |
| `certs/` | 持ち込みのサーバー証明書を使うときだけ（4.3） | UID 65532 |
| ボリューム `<プロジェクト名>_provisioner-data` | 自己署名のサーバー証明書と秘密鍵 | — |
| ボリューム `<プロジェクト名>_valkey-data` | Valkey のデータ | — |

加入者・テナントなどのデータは持たない（正本は下流、台帳は aka-ossbss）。Ki・OPc はログにも監査ログにも Valkey にも残らない。

## 2. 導入

ホストに要るのは Docker Engine と compose プラグインだけである（Debian 13 を主に想定する）。VPS の上ではイメージをビルドしない（9 章）。aka-vecgen と aka-radius を先に導入しておく。

1. `.env` を作り、`PROVISIONER_VALKEY_PASSWORD` を長い乱数にする（`openssl rand -hex 24` など。下流の Valkey とは別の値）。

   ```bash
   cp .env.example .env && chmod 600 .env
   ```

2. ピンのファイルと監査ログのディレクトリを作る。**コンテナを一度でも動かす前に作る**（ないと Docker が同じ名前のディレクトリを作る。できてしまったら、そのディレクトリを消してからファイルを作る）。

   ```bash
   mkdir -p secrets audit certs && touch secrets/admin-clients
   ```

3. 下流とつなぐ（3 章）。クライアント証明書を作り、登録簿を書く。
4. aka-ossbss のクライアント証明書のピンを書く（4.1）。ピンが 1 つもないと、起動しない。
5. コンテナが読めるようにする。

   ```bash
   sudo chown -R 65532:65532 downstreams secrets/admin-clients audit
   ```

   ```bash
   sudo chmod 600 downstreams/client.pem secrets/admin-clients
   ```

6. 起動し、下流につながることを確かめる。

   ```bash
   docker compose up -d
   ```

   ```bash
   docker compose exec -T aka-provisioner /aka-provisioner check-downstream
   ```

7. サーバー証明書を取り出して aka-ossbss に渡す（4.1）。

次のどれかに当たると起動しない（ログの `event_id`。終了して、compose の `restart` で起動し直す）。

| `event_id` | 原因 | 直し方 |
|---|---|---|
| `ADMIN_CLIENTS_LOAD_ERR` | ピンのファイルがない・読めない・形が違う・1 つもない | 4.1 |
| `DOWNSTREAMS_LOAD_ERR` | 登録簿が読めない、形が違う、証明書や鍵が読めない | 3.1 |
| `SERVER_CERT_LOAD_ERR` | サーバー証明書が読めない、組が合わない | 4.3 |
| `VALKEY_CONN_ERR` | Valkey に届かない、パスワードが違う | `docker compose logs valkey` |
| `AUDIT_DIR_ERR` | `audit/` に UID 65532 で書けない | 持ち主と権限を直す |
| `SYS_LISTEN_ERR` | healthcheck の口のポートが使えない | ログの `error` |
| 設定の誤り（`error:` の行だけ） | 環境変数の値の形が違う | ログを見る（項目は README の「環境変数」） |

下流につながらなくても起動する（ログの `DOWNSTREAM_UNREACHABLE`。`/status` にも出る）。その間、その下流への中継は 503 になる。

## 3. 下流とつなぐ

aka-provisioner は 1 つのクライアント証明書で全下流につなぐ。下流の側では aka-provisioner を管理クライアントとして登録し、aka-provisioner の側では下流のサーバー証明書を信頼の起点にする。

### 3.1 クライアント証明書と登録簿

1. クライアント証明書を作る（証明書と秘密鍵が 1 つの PEM に入る。有効日数は `-days`、既定 825 日）。下流のピンのファイルに書く 1 行が標準エラーに出る。

   ```bash
   docker compose run --rm --no-deps -T aka-provisioner gen-client-cert -name provisioner > downstreams/client.pem
   ```

2. 出た 1 行（`provisioner=<フィンガープリント>`）を、aka-vecgen と aka-radius のピンのファイルに足し、それぞれ `SIGHUP` で読み直す（aka-vecgen の運用ガイド 4.1、aka-radius の運用ガイド 5.1）。
3. 下流の管理API のサーバー証明書をもらい、`downstreams/` に置く。aka-vecgen は `docker compose exec -T aka-vecgen /aka-vecgen admin-cert` の出力、aka-radius は `deployments/certs/admin/server.pem`。どちらも SAN に、aka-provisioner がつなぐ WireGuard のアドレスを入れておく（aka-vecgen は `VECGEN_ADMIN_TLS_HOSTS`、aka-radius は `admin-cert create -hosts`）。
4. 登録簿 `downstreams/downstreams.json` を書く（見本は `downstreams/downstreams.example.json`。項目は README）。パスは登録簿のファイルからの相対で書ける。

   ```json
   {
     "clientCert": "client.pem",
     "clientKey": "client.pem",
     "vecgen": { "url": "https://10.20.0.2:9443/admin/v2", "serverCert": "vecgen-admin.pem" },
     "radius": { "url": "https://10.20.0.2:9444/admin/v2", "serverCert": "radius-admin.pem" }
   }
   ```

5. 動いているなら読み直させる（`docker compose kill -s HUP aka-provisioner`）。`check-downstream` で確かめる。

登録簿は `downstreams/` のディレクトリごとマウントしているので、`mv` で置き換えてもよい。読み直しでは、ピン・登録簿・サーバー証明書をそれぞれ読み直す（ログの `*_RELOADED`）。読めない・形が違うときは前のまま続ける（`*_RELOAD_ERR`）。登録簿を読み直すと、次の要求から新しいクライアントを使い、すぐに下流の状態を集め直す。

`check-downstream` は全下流の `/status` を呼び、つながらなければ原因の見当を出す（10 章）。同じ内容は `/status` の `downstreams[].hint` にも出る。

### 3.2 クライアント証明書を入れ替える

1. 新しいクライアント証明書を、別の名前で作る（`downstreams/` は UID 65532 の持ち物なので `sudo` で書く）。

   ```bash
   docker compose run --rm --no-deps -T aka-provisioner gen-client-cert -name provisioner | sudo tee downstreams/client-new.pem > /dev/null
   ```

   ```bash
   sudo chown 65532:65532 downstreams/client-new.pem && sudo chmod 600 downstreams/client-new.pem
   ```

2. 出た 1 行を全下流のピンのファイルに足し、それぞれ `SIGHUP` で読み直す（新旧の両方を受ける）。
3. 登録簿の `clientCert`・`clientKey` を新しいファイルに替え、`docker compose kill -s HUP aka-provisioner` で読み直す。
4. `check-downstream` と `/status` の `clientCertificate.fingerprint` で確かめる。
5. 全下流のピンのファイルから古い行を消し、それぞれ `SIGHUP` で読み直す。古いファイルを消す。

### 3.3 下流のサーバー証明書が変わったとき

下流がサーバー証明書を作り直したら（aka-vecgen の `admin-cert reset`、aka-radius の `admin-cert create -force`）、`downstreams/` のファイルを差し替えて `docker compose kill -s HUP aka-provisioner` を送る。**差し替えるまでの間、その下流への中継は 503 になる。** 下流の `SIGHUP` と続けて行う。

### 3.4 同じホストで試すとき

本番の下流は別の VPS にある。同じホストで試すときは、`.env` の `COMPOSE_FILE` に `compose.aka-radius-admin.yaml`・`compose.aka-av.yaml` を足して下流の共有ネットワークに入り、`https://aka-radius-admin:9444/admin/v2`・`https://aka-vecgen:9443/admin/v2` でつなぐ（下流を先に起動する）。aka-radius の Fluent Bit と同じホストに置くときは `PROVISIONER_FLUENT_BIT_PORT` を変える。

## 4. 管理クライアント

管理クライアントは aka-ossbss だけで、全権として扱う。誰が何をしてよいかは aka-ossbss が決める。

### 4.1 管理クライアントを登録する

ピンのファイル `secrets/admin-clients` に、1 行に 1 つ、`識別名=クライアント証明書の SHA-256 のフィンガープリント` を書く。

```
# aka-ossbss（同じ VPS）
ossbss=0123…cdef
```

- 手順は aka-ossbss の運用ガイド 3 章。aka-ossbss の `gen-client-cert` が、この 1 行を標準エラーに出す。`openssl x509 -in client.pem -noout -fingerprint -sha256` の出力（コロン区切り・大文字）もそのまま書ける。
- `#` で始まる行と空行は無視する。同じ識別名を複数の行に書ける（入れ替え。4.2）。同じフィンガープリントを 2 回書くのは誤り。
- 識別名は監査ログの `mgmtClient` に残る。
- 1 つもないファイルでは起動しない（読み直しでは前のまま続ける）。
- 直したら `docker compose kill -s HUP aka-provisioner` で読み直す（ログの `ADMIN_CLIENTS_RELOADED` の `fingerprints` が行の数、`/status` の `adminClients`・`adminClientsLoadedAt`）。
- **ファイルはファイルごとマウントしているので、その場で書き換える。** `mv` や `sed -i`（新しいファイルに置き換える）で書き換えると、動いているコンテナは古い中身を読み続け、読み直しても効かない。そうしてしまったら `docker compose up -d --force-recreate aka-provisioner` で作り直す。
- 登録していない証明書の接続は TLS のハンドシェイクで断り、ログに `ADMIN_CLIENT_REJECTED`（`reason`・`fingerprint`・`src_ip`）を出す。登録の誤りは、この `fingerprint` と比べて確かめる。

aka-ossbss に渡すサーバー証明書は、次のコマンドで取り出す（SAN と期限、フィンガープリントが標準エラーに出る）。

```bash
docker compose exec -T aka-provisioner /aka-provisioner server-cert > provisioner-server.pem
```

### 4.2 管理クライアントの証明書を入れ替える

1. aka-ossbss で新しいクライアント証明書を作る（aka-ossbss の運用ガイド 3.2）。
2. ピンのファイルに、同じ識別名で新しいフィンガープリントの行を足し、`SIGHUP` で読み直す（新旧の両方を受ける）。

   ```bash
   echo "ossbss=<新しいフィンガープリント>" | sudo tee -a secrets/admin-clients > /dev/null
   ```

   ```bash
   docker compose kill -s HUP aka-provisioner
   ```

3. aka-ossbss を新しい証明書に切り替える。
4. 古い行を消し（その場で書き換える）、もう一度 `SIGHUP` で読み直す。

   ```bash
   sudo sh -c 'grep -v "<古いフィンガープリント>" secrets/admin-clients > /tmp/admin-clients.new && cat /tmp/admin-clients.new > secrets/admin-clients && rm /tmp/admin-clients.new'
   ```

### 4.3 サーバー証明書を作り直す

自己署名のサーバー証明書（有効期間 10 年）は、初めての起動で `PROVISIONER_TLS_HOSTS` の SAN で作り、ボリュームに置く。SAN を変えたときと、秘密鍵が漏れたおそれがあるときは、ボリュームごと作り直す。

```bash
docker compose rm -sf aka-provisioner
```

```bash
docker volume rm <プロジェクト名>_provisioner-data
```

```bash
docker compose up -d
```

作り直したら `server-cert` で取り出し、aka-ossbss の `certs/provisioner-server.pem` を差し替える（aka-ossbss の運用ガイド 3.3）。

持ち込みの証明書を使うときは、`certs/` に置いて `.env` の `PROVISIONER_TLS_CERT`・`PROVISIONER_TLS_KEY` にコンテナの中のパス（`/certs/…`）を書く。証明書を差し替えたら `SIGHUP` で読み直す（ログの `SERVER_CERT_RELOADED`）。

期限は `/status` の `clientCertificate`・`serverCertificate`・`downstreams[].serverCertificate` で見られる。

## 5. 管理面の Fluent Bit

aka-provisioner の compose が管理面の Fluent Bit を動かす。Docker の `fluentd` のログドライバーからログを受け、アプリごとのファイルに書く。`docker compose logs` でも見られる。Fluent Bit が止まっていても、ほかのコンテナは起動できる（`fluentd-async`）。

| ファイル（`logs_on_host/`） | ログ |
|---|---|
| `provisioner.log`・`provisioner-valkey.log` | aka-provisioner とその Valkey |
| `ossbss.log`・`ossbss-postgres.log`・`ossbss-cloudflared.log` | aka-ossbss とその PostgreSQL・cloudflared（aka-ossbss の `compose.fluent-bit.yaml` を重ねたとき） |

ファイルはホストの logrotate で日ごとに圧縮し、2 週間持つ。監視用の PC が取りに行く（運用用の WireGuard と踏み台を通して rsync over SSH）。

`/etc/logrotate.d/aka-provisioner`（パスは置き場所に合わせる）:

```
/opt/aka-provisioner/logs_on_host/*.log {
    daily
    rotate 14
    compress
    delaycompress
    missingok
    notifempty
    create 0644 root root
    dateext
    dateformat -%Y%m%d
}
```

## 6. 公開範囲

| ポート | 既定の公開先 | 本番 |
|---|---|---|
| API 9446 | `127.0.0.1`（`PROVISIONER_PUBLISH`） | そのまま。aka-ossbss は共有の Docker ネットワークでつなぐ。別のホストの管理クライアントから使うときだけ、サービス用の WireGuard のアドレスにする |
| Valkey 6379 | 公開しない | そのまま |
| Fluent Bit 24224 | `127.0.0.1`（`PROVISIONER_FLUENT_BIT_PORT`） | そのまま（Docker のログドライバーだけが使う） |

- Docker が公開したポートは、ufw の規則を通らずに外から届く。公開範囲は `.env` のバインド先のアドレスで決める。`0.0.0.0` にはしない。
- 下流へは外向きに接続する（WireGuard の経路をホストで持つ）。nftables の規則は段階 5 の配置の文書で決める。
- 同じホストの管理クライアントが接続に使う名前は、サーバー証明書の SAN（`PROVISIONER_TLS_HOSTS`。既定で `aka-provisioner` を含む）に入れておく。

## 7. ログと監査ログ

| 記録 | 内容 | 置き場所 | 持つ期間 |
|---|---|---|---|
| ログ | 中継の要求、断った要求、下流の状態の変化、起動と読み直し、誤り | `logs_on_host/provisioner.log` | ノードで 2 週間（5 章） |
| 監査ログ | 下流を書き換えた中継（成功したとき） | Valkey の Stream と `audit/` の日ごとのファイル | Stream は `PROVISIONER_AUDIT_MAX` 件（既定 100,000）、ファイルは `PROVISIONER_AUDIT_RETENTION_DAYS` 日（既定 14） |

要求と応答の本文は、ログにも監査ログにも出さない。IMSI はマスクしない（aka-vecgen・aka-radius と同じ。ログを取りに行く監視用の PC の保管とアクセスできる人も、ノードと同じく限る）。

### 7.1 ログ

JSON（1 行に 1 つ）で出す。どの行にも `event_id` があり、要求に関わる行には `trace_id` がある。

| `event_id` | 内容 |
|---|---|
| `ADMIN_REQUEST` | API の要求（`trace_id`、操作者、状態の符号、時間） |
| `ADMIN_CLIENT_REJECTED` | TLS のハンドシェイクで証明書を断った（`not configured`、`outside validity period`） |
| `RELAY_DENIED` | 許していない口を断った（404 `ROUTE_NOT_ALLOWED`） |
| `IDEMPOTENT_REPLAYED` | 同じ `Idempotency-Key` の再送に、最初の応答を返した |
| `DOWNSTREAM_UNREACHABLE` / `DOWNSTREAM_REACHABLE` | 下流につながらなくなった / またつながった（下流の状態は 30 秒ごとに集める。変わったときだけ出る） |
| `DOWNSTREAM_ERROR` | 下流が ProblemDetails でない誤りを返した（502） |
| `ADMIN_CLIENTS_LOADED` / `ADMIN_CLIENTS_RELOADED` / `ADMIN_CLIENTS_RELOAD_ERR` | ピンのファイルを読んだ / 読み直した / 読み直せず前のまま続ける |
| `DOWNSTREAMS_LOADED` / `DOWNSTREAMS_RELOADED` / `DOWNSTREAMS_RELOAD_ERR` | 登録簿を読んだ / 読み直した / 読み直せず前のまま続ける |
| `SERVER_CERT_LOADED` / `SERVER_CERT_RELOADED` / `SERVER_CERT_RELOAD_ERR` | サーバー証明書を読んだ / 読み直した / 読み直せず前のまま続ける |
| `AUDIT` / `AUDIT_WRITE_ERR` / `AUDIT_MAINTAIN_ERR` | 監査ログを書いた / 書けなかった（中継は成功のまま） / ファイルの圧縮か削除に失敗した |
| `SYSTEM_ERROR` | 中の誤り（Valkey に届かないときなど） |
| `HEALTH_VALKEY_ERR` / `HEALTH_SERVE_ERR` | healthcheck で Valkey に届かない / healthcheck の口の誤り |
| `STARTING` | 起動 |
| `*_LOAD_ERR` など | 起動の失敗（2 章の表） |

### 7.2 監査ログ

- 日ごとのファイル `audit/audit-YYYYMMDD.jsonl`（日付は日本時間）。項目は `time`・`operator`・`mgmtClient`・`action`・`target`・`detail`・`traceId`・`streamId`（aka-vecgen・aka-radius と同じ）。前の日までのファイルは gzip で圧縮し、持つ日数を過ぎたら消す。どちらも起動のときと 1 時間ごとに行う。監視用の PC が取りに行く。
- `GET /admin/v2/audit-logs` は Stream を新しい順に返す（aka-ossbss の画面 `/audit/provisioner` で見られる）。
- `action` は `<下流>.<口>`（`vecgen.subscriber.create`・`update`・`state`・`reinstate`・`delete`、`radius.subscriber.…`）。`detail` は `{"downstream":…,"fields":[本文の項目の名前]}` で、値は残さない。
- 読み出しと一括取得（`POST …/subscribers/batch-get`）は残さない。下流が断った要求（4xx・5xx）も残さない。
- 一括投入では 1 行ごとに残る（1 回 1 万件）。Stream は件数の上限で古いものから押し出されるので、長く残すのはファイルのほうである。
- ファイルに書けなかったときは、中継は成功のまま返し、ログに `AUDIT_WRITE_ERR` を出す。このログは監視で拾う（`audit/` の空きと権限を確かめる）。

### 7.3 トレースID で記録をたどる

aka-ossbss が `X-Trace-ID` で渡した値を、aka-provisioner のログ（`trace_id`）と監査ログ（`traceId`）に残し、そのまま下流に渡す（なければ採番する）。一括投入の行は `job-<ジョブの ID>`、突き合わせの修正は `reconcile-<実行の番号>` である。

1. aka-ossbss の監査ログか画面で、操作のトレースID を調べる。
2. aka-provisioner で同じトレースID を探す。

   ```bash
   grep -h '"trace_id":"<トレースID>"' logs_on_host/provisioner.log
   ```

   ```bash
   sudo sh -c "grep -h '\"traceId\":\"<トレースID>\"' audit/audit-*.jsonl; zgrep -h '\"traceId\":\"<トレースID>\"' audit/audit-*.jsonl.gz"
   ```

3. 下流で同じトレースID を探す（aka-vecgen の運用ガイド 8.3、aka-radius の運用ガイド 9.3）。

## 8. バックアップと戻し

| 守るもの | 取り方 | 失うと |
|---|---|---|
| Valkey（`Idempotency-Key` の記録、監査ログの Stream） | `scripts/backup-valkey.sh`（止めずにスナップショットを取る） | **業務は続く**。24 時間以内の再送が下流に届くが、aka-ossbss は「既にある」「ない」として扱う。Stream の監査ログは消える（ファイルは残る） |
| `.env`、`downstreams/`（クライアント証明書の秘密鍵を含む）、`secrets/admin-clients` | ファイルの写し（運用者だけが読める場所に） | クライアント証明書を作り直して全下流に登録し直す（3.2）、ピンを書き直す（4.1） |
| ボリューム `provisioner-data`（サーバー証明書） | 取らなくてよい | 起動のときに作り直されるので、aka-ossbss に渡し直す（4.3） |
| `audit/`、`logs_on_host/` | 監視用の PC が取りに行く | 取りに行っていない分の記録が消える |

バックアップ:

```bash
mkdir -p backups && chmod 700 backups
```

```bash
scripts/backup-valkey.sh backups/valkey-$(date +%Y%m%d).rdb
```

戻し方（今のデータは消える）:

```bash
scripts/restore-valkey.sh backups/valkey-20261031.rdb
```

```bash
docker compose up -d
```

- スクリプトは素の `docker compose` を使う。プロジェクト名や重ね合わせを変えて起動しているときは、`COMPOSE_PROJECT_NAME`・`COMPOSE_FILE` を付けて動かす。動いている valkey が見つからなければ何もしない（別のボリュームを触らないため）。
- AOF が有効な Valkey は、AOF がないとスナップショットを読まずに空で始まる。ボリュームに写しを置いて起動するだけでは戻らないので、必ずスクリプトで戻す（aka-vecgen・aka-radius と同じ）。
- 戻すと、写しの後の `Idempotency-Key` の記録と Stream の監査ログは消える。下流のデータは変わらない。

バックアップの頻度と保管する期間（RPO・RTO）は、段階 5 の前に決める。

## 9. 更新

MVP セットの VPS の上ではイメージをビルドしない。イメージは手元でビルドして VPS に送る（送り方は段階 5 の配置の文書で決める）。

1. 更新の前に Valkey のバックアップを取る（8 章）。
2. `.env.example` の増えた項目を `.env` に足す。
3. イメージを入れ替え、`docker compose up -d` で作り直す（最大 15 秒で止まり、処理中の要求を終えてから閉じる。止まっている間、aka-ossbss の操作は「結果を確かめています」になることがある）。
4. `check-downstream` と `/status` の `version` で確かめる。

起動のログと `/status` の `version` に出る版は、ビルドのときの `PROVISIONER_VERSION`（既定 `dev`）で決まる。

```bash
PROVISIONER_VERSION=$(git rev-parse --short HEAD) docker compose build
```

下流を更新したときも、`check-downstream` で確かめる。下流を止めている間の中継は 503 になり、戻ると再起動なしでつながる。

## 10. 障害時の確かめ

まず `check-downstream` で下流への接続を確かめる。

| 症状・表示 | 確かめること |
|---|---|
| aka-provisioner が起動しない | 起動のログの `event_id`（2 章の表） |
| `docker compose ps` で unhealthy | 専用の Valkey に届かない。`docker compose exec aka-provisioner /aka-provisioner healthcheck`、`docker compose logs valkey` |
| 「…が provisioner のクライアント証明書を受け付けませんでした」「…が接続を切りました」 | 下流のピンのファイルに、`check-downstream` が出すフィンガープリントがあるか。書いた後に下流へ `SIGHUP` を送ったか。証明書の期限。下流のログの `ADMIN_CLIENT_REJECTED` |
| 「…のサーバー証明書が、登録簿の serverCert と一致しません」 | 下流がサーバー証明書を作り直していないか（3.3） |
| 「…のサーバー証明書の SAN に…入っていません」 | 下流のサーバー証明書の SAN に、登録簿の `url` のホスト名か IP があるか |
| 「…のサーバー証明書が有効期間外です」 | 下流で作り直す（3.3） |
| 「…のホスト名…を解決できません」 | 同じホストの試しでは、下流が動いているか、共有ネットワークに入っているか（3.4） |
| 「…に接続できません」「…から時間内に応答がありません」 | 下流が動いているか、登録簿の `url`、WireGuard の経路、下流の公開先（`VECGEN_ADMIN_PUBLISH`・`RADIUS_ADMIN_BIND`）と nftables。タイムアウトは `PROVISIONER_DOWNSTREAM_TIMEOUT` |
| `network … declared as external, but could not be found` | 同じホストの試しで、下流を先に起動しているか。本番では `COMPOSE_FILE` から重ね合わせを外す |
| 下流の `docker compose down` で `Network … Resource is still in use` | aka-provisioner が共有ネットワークに入っているため、ネットワークが残っただけで問題はない |
| 管理クライアントが接続できない（TLS のハンドシェイクで切れる。curl は終了コード 56） | ログの `ADMIN_CLIENT_REJECTED` の `reason`（`not configured` はピンにない、`outside validity period` は期限切れ）と `fingerprint` |
| ピンを書き換えたのに効かない | `SIGHUP` を送ったか。`ADMIN_CLIENTS_RELOAD_ERR` が出ていないか。`mv`・`sed -i` で置き換えていないか（4.1） |
| コンテナの起動で `/run/secrets/admin-clients` の mount の誤り、`… is not a file` | ピンのファイルを置き換えた、または作る前にコンテナを動かした（2 章の 2、4.1）。ファイルを作り直して `--force-recreate` |
| 監査ログのファイルが増えない | `AUDIT_WRITE_ERR`・`AUDIT_MAINTAIN_ERR`（`audit/` の持ち主と空き） |

API の誤り（ProblemDetails の `cause`）:

| 状態の符号と `cause` | 意味 | 確かめること |
|---|---|---|
| 404 `ROUTE_NOT_ALLOWED` | 許していない口（aka-vecgen の鍵の読み出し、RADIUS クライアントの書き込みなど） | [API 仕様](openapi/provisioner-api.yaml)の口の一覧 |
| 400 `MANDATORY_IE_MISSING` | 書き込みに `Idempotency-Key` がない | 呼び出し側 |
| 409 `OPERATION_IN_PROGRESS` | 同じ `Idempotency-Key` の要求が処理中 | 少し待って送り直す（処理中の記録は 60 秒で消える） |
| 422 `IDEMPOTENCY_KEY_MISMATCH` | 同じ `Idempotency-Key` で内容が違う | 呼び出し側 |
| 503 `DOWNSTREAM_UNAVAILABLE` | 応答の `downstream` の下流につながらない | `check-downstream` |
| 502 `DOWNSTREAM_ERROR` | 下流が ProblemDetails でない誤りを返した | ログの `DOWNSTREAM_ERROR`、下流のログ |
| 500 `SYSTEM_FAILURE` | 専用の Valkey に届かない | `docker compose ps valkey`。書き込みは断る |

下流の誤り（`downstream` つきの `cause`）は、そのまま中継している。意味は下流の管理API の仕様を見る。
