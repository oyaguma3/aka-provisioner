# 運用ガイド

- 対象: eapaka-node-provisioner（統合API。コマンド名 `eapaka-provisioner`。以下「provisioner」）を導入・運用する人。
- 関連: [README](../README.md)、[設計概要](design-overview.md)、[API 仕様](openapi/provisioner-api.yaml)、本PoC（eapaka-radius-server-poc）の `docs/B-02_アプリケーションデプロイ手順書_r*.md` §14（aka-only-server への接続）・§15（Provisioning API の有効化）、aka-only-server の `docs/operation-guide.md`
- 本書の手順は、2026-10-10 に検証用の実機（Debian 13、Docker Engine と compose プラグイン、Tailscale）で実行して確かめた。別ホストの構成は、同じ tailnet の別のマシン（WSL）から確かめた。

## 1. 構成

provisioner は、本PoCの Provisioning API（provisioning-api。以下「prov」）と aka-only-server の管理API（以下「aka」）を組み合わせて、加入者を「IMSI＋鍵の置き場所＋認可ポリシー」として操作する。加入者のデータは下流の 2 つが持ち、provisioner は保存しない。

```
[BFF など] ──mTLS──> [provisioner] ──> Valkey（provisioner 専用）
                       ├─mTLS─> provisioning-api（本PoC）   … 加入者（鍵を本PoCに置くもの）・RADIUSクライアント・認可ポリシー
                       └─mTLS─> aka-only-server の管理API  … 加入者（鍵を aka-only-server に置くもの）
```

`docker compose` で 2 つのコンテナを動かす。

| コンテナ | 内容 |
|---|---|
| `eapaka-provisioner` | provisioner の API（`/admin/v1`）と、補償・やり直しのワーカー |
| `valkey` | provisioner 専用の Valkey。操作の記録、IMSI ごとのロック、`Idempotency-Key`、監査ログを保存する。ホストにもほかのプロジェクトにもポートを公開しない（下流の Valkey とは別） |

| 待ち受け | コンテナ内のポート | ホスト側の公開先（既定） |
|---|---|---|
| provisioner の API（mTLS） | 9446 | `127.0.0.1:9446`（`.env` の `PROVISIONER_PUBLISH`） |

| データ | 保存先 |
|---|---|
| 操作の記録、ロック、`Idempotency-Key` の記録、provisioner の監査ログ | ボリューム `valkey-data` |
| 自動生成した provisioner のサーバー証明書と秘密鍵 | ボリューム `provisioner-data` |
| 下流用の証明書（下に一覧）、持ち込みのサーバー証明書 | ホストの `certs/`（読み取り専用でマウント） |
| 加入者の鍵、認可ポリシー、RADIUSクライアント | 下流（本PoCと aka-only-server） |

1 つの provisioner が扱うのは、本PoCの 1 ノードと aka-only-server の 1 台である。下流の版は次を使う。

- 本PoCの provisioning-api 0.4.0 以降（本PoCの main の dbd97f5 以降）。0.3.0（def95e5 以降）でも動くが、加入者の停止・再開（2.10）が使えず、加入者の `status` も出ない。
- aka-only-server の管理API 0.2.0 以降（aka-only-server の main の 2a6dcf8 以降）。それより前の版では、aka の監査ログにトレースID が残らない。

### 1.1 証明書

CA は使わない。サーバー証明書は自己署名（または持ち込み）を相手に渡して信頼させ、クライアント証明書は SHA-256 フィンガープリントを相手に登録する。

| 証明書 | provisioner での置き場所 | 作り方 | 相手側での扱い |
|---|---|---|---|
| provisioner のサーバー証明書 | ボリューム `provisioner-data`（自動生成） | 初回起動時に自己署名を作る。SAN は `PROVISIONER_TLS_HOSTS` | `server-cert` で取り出し、管理クライアント（BFF など）が信頼する証明書にする |
| provisioner のクライアント証明書 | `certs/client.pem` | `eapaka-provisioner gen-client-cert`（2.2） | フィンガープリントを、本PoCの `PROVISIONING_API_ADMIN_CLIENTS` と aka-only-server の `AKA_ADMIN_CLIENTS` の両方に登録する |
| provisioning-api のサーバー証明書 | `certs/prov-server.pem` | 本PoC側で openssl で作る（B-02 §15.2） | ― |
| aka-only-server の管理API のサーバー証明書 | `certs/aka-server.pem` | aka-only-server が自動生成する。`admin-cert` で取り出す | ― |
| 管理クライアント（BFF など）のクライアント証明書 | ― | 管理クライアント側で作る | フィンガープリントを provisioner の `PROVISIONER_ADMIN_CLIENTS` に登録する |

### 1.2 ほかの操作手段との併用

provisioner は、同じ IMSI の操作が重ならないよう IMSI ごとにロックを取るが、このロックは provisioner を通る操作どうしでだけ効く。本PoCの Admin TUI、aka-only-server の管理 GUI（web-gui-for-aka-only-server）、下流の API を直接呼ぶ操作とは排他できない。provisioner を使う間は、同じ加入者をそれらで操作しない。

- 本PoCの Admin TUI での CSV の一括登録など、provisioner にない操作は Admin TUI で行ってよい。ただし、鍵を置く場所（4 章）を間違えると認証できないので、`GET /subscribers/{imsi}` の `issues` で確かめる。
- vector-gateway の AVクライアントの登録・変更は、aka-only-server の管理 GUI かコマンドで行う（provisioner は参照だけ）。

## 2. 導入（同一ホスト）

本PoCと aka-only-server と同じホストで動かす場合の手順を示す。別のホストの場合は 3 章を参照。

以下では、3 つのリポジトリを同じディレクトリに並べて置く。

```
~/eapaka-radius-server-poc/   本PoC（B-02 の手順で導入済み）
~/aka-only-server/            aka-only-server（README の手順で導入済み）
~/eapaka-node-provisioner/    このリポジトリ
```

前提:

- 本PoCは、vector-gateway を aka-only-server の AVクライアントとして接続済みであること（B-02 §14。`.env` の `VECTOR_GATEWAY_PLMN_MAP` で `01` を指定した PLMN の加入者の鍵を aka-only-server に置く）。aka-only-server を使わない場合は、2.3 を飛ばし、2.6 で aka の設定を空のままにする。
- aka-only-server は起動済みであること（共有ネットワーク `aka-av` を作るため）。

同じホストでは、provisioner のコンテナは下流の compose が作る共有の Docker ネットワークに参加し、ホスト名で下流に接続する。

| 下流 | 共有ネットワーク（既定の名前） | 作られるとき | provisioner からの URL |
|---|---|---|---|
| 本PoCの provisioning-api | `eapaka-prov` | 本PoCを `--profile provisioning` で起動したとき | `https://provisioning-api:9444/admin/v1` |
| aka-only-server の管理API | `aka-av` | aka-only-server を起動したとき | `https://aka-only-server:9443/admin/v1` |

共有ネットワークに参加するのは provisioner だけで、provisioner 専用の Valkey は参加しない。

### 2.1 取得して `.env` を作る

```bash
cd ~
```

```bash
git clone https://github.com/oyaguma3/eapaka-node-provisioner.git
```

```bash
cd ~/eapaka-node-provisioner
```

```bash
cp .env.example .env
```

```bash
chmod 600 .env
```

`.env` の `PROVISIONER_VALKEY_PASSWORD` を、provisioner 専用の Valkey のパスワードに書き換える（下流の Valkey のパスワードとは別のもの）。残りの項目は 2.6 で書く。全項目は 11 章を参照。

イメージをビルドする。下流の共有ネットワークがまだない段階では、`.env` の `COMPOSE_FILE` に重ねたファイルの外部ネットワークが見つからずに失敗するので、ここから 2.2 までは `-f compose.yaml` を付けて実行する。

```bash
docker compose -f compose.yaml build
```

### 2.2 下流に提示するクライアント証明書を作る

provisioner が下流 2 つに提示するクライアント証明書を作る。証明書と秘密鍵が 1 つの PEM にまとめて標準出力に出る。`-name` の識別名（例 `provisioner`）は、下流の監査ログの `mgmt_client` になる。

```bash
mkdir -p certs
```

```bash
docker compose -f compose.yaml run --rm --no-deps -T eapaka-provisioner gen-client-cert -name provisioner > certs/client.pem 2> gen.txt
```

```bash
cat gen.txt
```

```
クライアント証明書を作りました（CN=provisioner、有効期限 2029-01-11）。
SHA-256 フィンガープリント: 9433c7bb...ebc28b
下流の .env に次の値を登録してください（他の登録があればカンマ区切りで加える）:
  本PoC（eapaka-radius-server-poc）:
PROVISIONING_API_ADMIN_CLIENTS=provisioner=9433c7bb...ebc28b
  aka-only-server（鍵を aka-only-server に置く加入者を扱う場合）:
AKA_ADMIN_CLIENTS=provisioner=9433c7bb...ebc28b
```

- 1 つの証明書を、下流 2 つの両方に登録して使う（2.3、2.4）。
- 有効期間は既定で 825 日（`-days` で変えられる）。期限が切れると下流に拒否されるので、それまでに作り直して登録し直す。
- 証明書と秘密鍵を別のファイルにしたい場合は、`-out-cert` / `-out-key` で書き出し、`.env` の `PROVISIONER_CLIENT_KEY` に秘密鍵のコンテナ内のパス（`/certs/...`）を書く。
- 使い方は `docker compose -f compose.yaml run --rm --no-deps -T eapaka-provisioner gen-client-cert -h` で表示できる。

### 2.3 aka-only-server 側: provisioner を登録する

aka-only-server のディレクトリで作業する（aka-only-server の運用ガイド 3 章）。

1. `.env` の `AKA_ADMIN_CLIENTS` に、2.2 で表示された `AKA_ADMIN_CLIENTS=` の行の値を書く。ほかの管理クライアント（aka-only-server の管理 GUI など）が既にあれば、カンマで区切って加える。

   ```
   AKA_ADMIN_CLIENTS=aka-webgui=<既存の値>,provisioner=9433c7bb...ebc28b
   ```

2. 起動し直す。

   ```bash
   cd ~/aka-only-server
   ```

   ```bash
   docker compose up -d
   ```

3. 管理API のサーバー証明書を、provisioner の `certs/aka-server.pem` に取り出す。

   ```bash
   docker compose exec -T aka-only-server /aka-only-server admin-cert > ../eapaka-node-provisioner/certs/aka-server.pem
   ```

   同じホストでは、provisioner はホスト名 `aka-only-server` でサーバー証明書を検証する。`aka-only-server` は aka-only-server の `.env` の `AKA_ADMIN_TLS_HOSTS` の既定値に入っている。

4. vector-gateway の AVクライアントID を確かめる。vector-gateway に設定したクライアント証明書（本PoCの `deployments/certs/av-client.pem`）のフィンガープリントと同じものの ID を使う（2.6 の `PROVISIONER_AKA_AV_CLIENT_ID`）。

   ```bash
   docker compose exec -T aka-only-server /aka-only-server client list
   ```

   ```
   1	name=vector-gateway	enabled=true	network-name=""	not-after=2029-01-11T17:47:53Z	fingerprint=11fbbf25...59da033
   ```

   aka-only-server の管理 GUI の AVクライアントの一覧でも確かめられる。

### 2.4 本PoC側: provisioner を登録して provisioning-api を起動する

本PoCの `deployments/` で作業する（B-02 §15）。

1. provisioning-api のサーバー証明書がまだなければ作る（B-02 §15.2）。SAN には、provisioner が接続に使う名前 `DNS:provisioning-api` を必ず入れる（provisioner はホスト名を検証する）。別ホストの provisioner からも使う場合は、そのときに接続に使う VPN 側のアドレスも入れておく（3 章）。

   ```bash
   cd ~/eapaka-radius-server-poc/deployments
   ```

   ```bash
   mkdir -p certs/provisioning
   ```

   ```bash
   openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
     -keyout certs/provisioning/server.key -out certs/provisioning/server.pem \
     -days 825 -subj "/CN=provisioning-api" \
     -addext "subjectAltName=DNS:provisioning-api,DNS:localhost,IP:127.0.0.1"
   ```

   ```bash
   chmod 600 certs/provisioning/server.key
   ```

2. `.env` に、2.2 で表示された `PROVISIONING_API_ADMIN_CLIENTS=` の行と、ノード名を書く（B-02 §15.4）。ほかの管理クライアントが既にあれば、カンマで区切って加える。

   ```
   PROVISIONING_API_ADMIN_CLIENTS=provisioner=9433c7bb...ebc28b
   PROVISIONING_API_NODE_NAME=<ノード名>
   ```

3. provisioning-api を起動する（B-02 §15.5）。このとき共有ネットワーク `eapaka-prov` が作られる。以後、本PoCの `docker compose` の操作には `--profile provisioning` を付ける（B-02 §15.8）。`.env` に `COMPOSE_PROFILES=provisioning` を書いておけば、付けなくてよい（2026-10-10 に検証機で確認）。

   ```bash
   docker compose --profile provisioning up -d --build
   ```

4. provisioning-api のサーバー証明書を、provisioner の `certs/prov-server.pem` に写す。

   ```bash
   cp certs/provisioning/server.pem ~/eapaka-node-provisioner/certs/prov-server.pem
   ```

5. `.env` の `VECTOR_GATEWAY_PLMN_MAP` の値を控える（2.6 の `PROVISIONER_PLMN_MAP` に同じ値を書く）。

   ```bash
   grep '^VECTOR_GATEWAY_PLMN_MAP' .env
   ```

   ```
   VECTOR_GATEWAY_PLMN_MAP=44020:01
   ```

### 2.5 証明書の所有者を直す

provisioner のコンテナは UID 65532 で動くので、`certs/` のファイルの所有者をこの UID にして、パーミッションを 600 にする。

```bash
cd ~/eapaka-node-provisioner
```

```bash
sudo chown 65532:65532 certs/*.pem
```

```bash
sudo chmod 600 certs/*.pem
```

```bash
ls -l certs
```

```
-rw------- 1 65532 65532 611 ... aka-server.pem
-rw------- 1 65532 65532 779 ... client.pem
-rw------- 1 65532 65532 672 ... prov-server.pem
```

以後、`certs/` のファイルを置き換えるときは `sudo cp` で上書きする。

### 2.6 `.env` を書く

少なくとも次の項目を書く。全項目は 11 章を参照。

| 項目 | 内容 | 例 |
|---|---|---|
| `COMPOSE_FILE` | 重ねる compose ファイル。同一ホストの下流ごとに共有ネットワークの設定を重ねる（既定は両方） | `compose.yaml:compose.eapaka-prov.yaml:compose.aka-av.yaml` |
| `PROVISIONER_ADMIN_CLIENTS` | 管理クライアント（BFF など）の `識別名=フィンガープリント`（2.8）。1 つもなければ provisioner は起動しない | `bff=4bb320e6...2a2e2806` |
| `PROVISIONER_AKA_URL` | aka-only-server の管理API。aka-only-server を使わない場合は空 | `https://aka-only-server:9443/admin/v1` |
| `PROVISIONER_AKA_AV_CLIENT_ID` | vector-gateway の AVクライアントID（2.3 の 4） | `1` |
| `PROVISIONER_PLMN_MAP` | 本PoCの `VECTOR_GATEWAY_PLMN_MAP` と同じ値（2.4 の 5。4 章） | `44020:01` |

`PROVISIONER_PROV_URL` は、同一ホストなら既定（`https://provisioning-api:9444/admin/v1`）のままでよい。

### 2.7 起動して確かめる

本PoC（`--profile provisioning`）と aka-only-server が先に起動している必要がある（共有ネットワークをそれぞれの compose が作るため）。

```bash
docker compose up -d
```

`docker compose ps` で `eapaka-provisioner` が healthy になる（compose の healthcheck は `eapaka-provisioner healthcheck` で、API の待ち受けに TCP で接続できるかを見る。下流の状態は次の起動ログと `check-downstream` で見る）。

起動ログに、下流 2 つと vector-gateway の AVクライアントの確認の結果が出る。

```bash
docker compose logs eapaka-provisioner
```

```
{"level":"INFO","msg":"plmn map","entries":"44020=aka"}
{"level":"INFO","msg":"provisioning-api is available","server_version":"0.4.0","node_name":"simwifi"}
{"level":"INFO","msg":"aka-only-server is available","server_version":"dev"}
{"level":"INFO","msg":"av client of vector-gateway","av_client_id":1,"name":"vector-gateway"}
{"level":"INFO","msg":"listening","addr":"[::]:9446"}
```

下流に接続できなくても provisioner は起動する（その場合は WARN と原因の見当 `hint` が出る。10 章）。導入時や設定を変えた後は、`check-downstream` でも確かめられる。

```bash
docker compose exec -T eapaka-provisioner /eapaka-provisioner check-downstream
```

```
クライアント証明書のフィンガープリント: 9433c7bb...ebc28b
PLMN マップ: 44020=aka

本PoCの Provisioning API: https://provisioning-api:9444/admin/v1
  接続できました。provisioning-api 0.4.0（ノード simwifi、加入者 0、RADIUSクライアント 0、認可ポリシー 0）

aka-only-server の管理API: https://aka-only-server:9443/admin/v1
  接続できました。aka-only-server dev（加入者 0、AVクライアント 1）
  vector-gateway の AVクライアント: ID 1（vector-gateway、証明書の有効期限 2029-01-11）
```

すべて「接続できました」と出れば、下流との準備は終わりである。

### 2.8 管理クライアント（BFF など）をつなぐ

#### サーバー証明書を渡す

provisioner のサーバー証明書（自己署名）を取り出して、管理クライアントが信頼する証明書にする。標準エラーに SAN とフィンガープリントが出る。

```bash
docker compose exec -T eapaka-provisioner /eapaka-provisioner server-cert > provisioner-server.pem
```

```
SAN: localhost, eapaka-provisioner, 127.0.0.1、有効期限 2036-10-06
SHA-256 フィンガープリント: 44838a94...541dbbef
```

#### 管理クライアントを登録する

管理クライアントのクライアント証明書は、管理クライアントの側で作る。そのフィンガープリントを `.env` の `PROVISIONER_ADMIN_CLIENTS` に `識別名=フィンガープリント` で書く（複数はカンマ区切り。識別名は provisioner の監査ログの `mgmtClient` になる）。書き換えたら、provisioner を作り直す。

```bash
docker compose up -d
```

- フィンガープリントは、`openssl x509 -in <証明書> -noout -fingerprint -sha256` の出力（大文字、コロン区切り）もそのまま書ける。
- 動作確認用の証明書は、provisioner の `gen-client-cert` でも作れる（例: `-name bff`）。このとき標準エラーに出る下流向けの行は使わず、フィンガープリントだけを `PROVISIONER_ADMIN_CLIENTS` に書く。

#### 同じホストの管理クライアント

provisioner の compose は、同一ホストの管理クライアント向けの共有ネットワーク（既定の名前は `eapaka-provisioner`、変数 `PROVISIONER_SHARED_NETWORK`）を作り、provisioner だけを参加させる。同じホストで別の compose として動く管理クライアントは、ここに外部ネットワークとして参加し、`https://eapaka-provisioner:9446/admin/v1` で接続する。ホスト側に公開したポート（`127.0.0.1:9446`）には、別のコンテナからは届かない。

本PoCの管理 GUI（web-gui-for-eapaka-radius）は、provisioner 経由でつなぐ設定を持っている（その運用ガイドの 11 章。同一ホスト用の `compose.eapaka-provisioner.yaml`、直接接続からの切り替え、別ホスト、戻し方）。2026-10-10 に、本書の構成と合わせて検証機で確かめた。

管理クライアント側の compose の例（サービス名・イメージ等は管理クライアント側の手順に従う）:

```yaml
services:
  bff:
    # （イメージ・環境変数・ボリューム等は管理クライアント側の手順に従う）
    networks:
      - default              # 管理クライアント側の他のコンテナ（専用の Valkey 等）と通信するため
      - eapaka-provisioner   # provisioner に接続するため

networks:
  eapaka-provisioner:
    external: true
    name: eapaka-provisioner   # provisioner 側の PROVISIONER_SHARED_NETWORK と同じ名前
```

- サーバー証明書の SAN の既定値（`PROVISIONER_TLS_HOSTS`）に `eapaka-provisioner` が入っているので、ホスト名の検証が通る。
- 共有ネットワークは provisioner の compose が作るので、provisioner を先に起動する。
- 共有ネットワークから名前で解決できるのは provisioner だけで、provisioner 専用の Valkey や下流には届かない（2026-10-10 に確認）。
- 管理クライアントの専用の Valkey など、provisioner に接続しないコンテナは共有ネットワークに参加させない。provisioner は `valkey` という名前で専用の Valkey に接続しているため、似た名前のコンテナを共有ネットワークに入れると名前解決で取り違えるおそれがある。

接続の確認は、curl のコンテナを管理クライアントに見立てて行える。確認用のディレクトリに、provisioner のサーバー証明書（`provisioner-server.pem`）と、登録した管理クライアントの証明書・秘密鍵（例 `bff.pem`。証明書と秘密鍵を 1 つにしたもの）を置いて実行する。`--user` は、`bff.pem` を読めるホストのユーザーの UID:GID にする。

```bash
docker run --rm --network eapaka-provisioner --user "$(id -u):$(id -g)" -v "$PWD:/c:ro" curlimages/curl:8.11.1 -sS --cacert /c/provisioner-server.pem --cert /c/bff.pem --key /c/bff.pem https://eapaka-provisioner:9446/admin/v1/status
```

```json
{"version":"dev","startedAt":"...","plmnMap":[{"plmn":"44020","keyStore":"aka"}],"downstreams":{"prov":{"configured":true,"url":"https://provisioning-api:9444/admin/v1","reachable":true,"version":"0.4.0","nodeName":"simwifi","subscriberCount":0},"aka":{"configured":true,"url":"https://aka-only-server:9443/admin/v1","reachable":true,"version":"dev","subscriberCount":0}},"avClient":{"id":1,"exists":true,"enabled":true,"name":"vector-gateway"},"valkey":{"reachable":true},"operations":{"running":0,"retrying":0,"failed":0}}
```

確認が終わったら、管理クライアントの秘密鍵を含む確認用のディレクトリを消す。

### 2.9 加入者を作って認証を確かめる

provisioner の API の使い方は API 仕様（`docs/openapi/provisioner-api.yaml`）を参照。以下は、2.8 の curl のコンテナで、本PoCの RADIUSクライアントと、鍵の置き場所の違う 2 人の加入者を作る例である（`44020` を `01` にした PLMN マップの場合）。長くなるので、curl のコンテナの呼び出しをシェルの関数にしておく。

```bash
pv() { m=$1; p=$2; shift 2; docker run --rm --network eapaka-provisioner --user "$(id -u):$(id -g)" -v "$PWD:/c:ro" curlimages/curl:8.11.1 -sS --cacert /c/provisioner-server.pem --cert /c/bff.pem --key /c/bff.pem -H 'X-Operator-Id: tester01' -H 'Content-Type: application/json' -X "$m" "https://eapaka-provisioner:9446/admin/v1$p" "$@"; }
```

RADIUSクライアント（AP）を登録する。中継なので、本文は本PoCの Provisioning API と同じ形である。

```bash
pv POST /clients -d '{"ip":"192.0.2.10","secret":"<共有シークレット>","name":"ap-01"}'
```

鍵を本PoCに置く加入者（PLMN `44010`）と、aka-only-server に置く加入者（PLMN `44020`）を作る。置き場所は PLMN マップから決まる（`keyStore` は省略できる）。

```bash
pv POST /subscribers -d '{"imsi":"440100123456789","ki":"<Ki>","opc":"<OPc>","policy":{"default":"allow","rules":[]}}'
```

```json
{"imsi":"440100123456789","keyStore":"poc","key":{"amf":"8000","sqn":"000000000000","createdAt":"..."},"policy":{"default":"allow","rules":[]},"status":"active","issues":[]}
```

```bash
pv POST /subscribers -d '{"imsi":"440200123456789","ki":"<Ki>","opc":"<OPc>","policy":{"default":"allow","rules":[]}}'
```

```json
{"imsi":"440200123456789","keyStore":"aka","key":{"amf":"8000","sqn":"000000000000","sqnType":"inc32","allowPlain":false,"allowedClientIds":[1],"createdAt":"...","updatedAt":"..."},"policy":{"default":"allow","rules":[]},"status":"active","issues":[]}
```

`keyStore` が `aka` の加入者は、`allowedClientIds` に vector-gateway の AVクライアントID が入る。検証では、この 2 人を eapaka_test で認証し、どちらも Access-Accept になること、ポリシーを `deny` に変えると Access-Reject になること、削除すると Access-Reject になることを確かめた（2026-10-10）。

### 2.10 加入者を停止・再開する

加入者を登録したまま、一時的に認証できなくする（停止）、または戻す（再開）。停止の印は本PoCの認可ポリシーの状態（`status`）で、鍵の置き場所によらず本PoCの auth-server が認証を拒否する（本PoCの provisioning-api 0.4.0 以降。1 章）。2.9 の関数 `pv` で次のように行う。

```bash
pv PUT /policies/440200123456789/status -d '{"status":"suspended"}'
```

```json
{"imsi":"440200123456789","default":"allow","rules":[],"status":"suspended"}
```

再開は `{"status":"active"}` を送る。加入者の取得・一覧でも `status` で状態が分かる。

```bash
pv PUT /policies/440200123456789/status -d '{"status":"active"}'
```

- 効くのは次の認証からで、接続中のセッションは切れない（セッションの切断は扱わない）。停止中の認証は Access-Reject になり、本PoCの auth-server のログに `AUTH_SUBSCRIBER_SUSPENDED`（WARN）が出る。
- 認可ポリシーがない加入者は停止できない（404 `POLICY_NOT_FOUND`。認可ポリシーがなければ、もともと認証は拒否される）。`active` / `suspended` 以外の値は 400。どちらも本PoCの応答をそのまま返す。
- 認可ポリシーの置き換え（`PATCH /subscribers/{imsi}` の `policy`、`PUT /policies/{imsi}`）では状態は変わらない（停止中なら停止のまま）。加入者の作成では指定できず、常に `active` で作る。
- 認可ポリシーを削除すると停止の印も消える（作り直すと `active`）。
- 加入者の操作と同じく、同じ IMSI の操作が処理中なら 409（`OPERATION_IN_PROGRESS`）、未完了の操作があれば 409（`OPERATION_UNRESOLVED`。6.3）になる。`Idempotency-Key` も付けられる。
- 監査ログには `policy.suspend` / `policy.resume` で残る（7 章）。今と同じ状態への変更も 200 で、provisioner の監査ログには残る（本PoCの監査ログには残らない）。

検証では、2.9 の 2 人を停止すると eapaka_test でどちらも Access-Reject になること、停止中に `policy` を置き換えても停止のままであること、再開すると Access-Accept に戻ることを確かめた（2026-10-10）。

## 3. 別ホストで動かす場合

provisioner と下流を別のホストで動かす場合は、下流の管理API を VPN 側のアドレスで公開し、provisioner はそのアドレスで接続する。以下では、下流のホストの VPN 側のアドレスを `100.64.0.20` とする。

下流ごとに同一ホスト・別ホストを選べる。`.env` の `COMPOSE_FILE` には、同一ホストの下流の設定だけを重ねる。

| 本PoC | aka-only-server | `COMPOSE_FILE` |
|---|---|---|
| 同一ホスト | 同一ホスト | `compose.yaml:compose.eapaka-prov.yaml:compose.aka-av.yaml` |
| 同一ホスト | 別ホスト | `compose.yaml:compose.eapaka-prov.yaml` |
| 別ホスト | 同一ホスト | `compose.yaml:compose.aka-av.yaml` |
| 別ホスト | 別ホスト | `compose.yaml` |

### 3.1 本PoC側

1. provisioning-api のサーバー証明書の SAN に `IP:100.64.0.20` を加えて作る（2.4 の 1）。既に作ってある場合は作り直し、`docker compose --profile provisioning restart provisioning-api` で読み込ませて、provisioner に渡し直す。
2. `.env` に `PROVISIONING_API_BIND=100.64.0.20` を書き、provisioning-api を作り直す（B-02 §15.4）。

   ```bash
   docker compose --profile provisioning up -d provisioning-api
   ```

### 3.2 aka-only-server 側

1. `.env` の `AKA_ADMIN_PUBLISH` を `100.64.0.20:9443` にし、`AKA_ADMIN_TLS_HOSTS` に `100.64.0.20` を加える。

   ```
   AKA_ADMIN_PUBLISH=100.64.0.20:9443
   AKA_ADMIN_TLS_HOSTS=localhost,127.0.0.1,aka-only-server,100.64.0.20
   ```

2. 作り直す。

   ```bash
   docker compose up -d
   ```

3. 管理API のサーバー証明書は生成済みのものに SAN が反映されないので、作り直して再起動する（aka-only-server の運用ガイド 2.1）。`admin-cert reset` は新しい証明書を標準出力に出すので、それを provisioner に渡す。同じ証明書を使っているほかの管理クライアント（aka-only-server の管理 GUI など）にも渡し直す。

   ```bash
   docker compose exec -T aka-only-server /aka-only-server admin-cert reset > aka-server.pem
   ```

   ```bash
   docker compose restart aka-only-server
   ```

### 3.3 provisioner 側

1. 2.1・2.2 のとおり取得し、クライアント証明書を作って下流 2 つに登録する（2.3 の 1・2、2.4 の 2・3）。
2. 下流のサーバー証明書を `certs/prov-server.pem`（本PoCの `server.pem`）と `certs/aka-server.pem`（3.2 の 3）に置き、2.5 の手順で所有者を直す。
3. `.env` を書く（2.6）。`COMPOSE_FILE` は上の表から選び、別ホストの下流の URL を VPN 側のアドレスにする。

   ```
   COMPOSE_FILE=compose.yaml
   PROVISIONER_PROV_URL=https://100.64.0.20:9444/admin/v1
   PROVISIONER_AKA_URL=https://100.64.0.20:9443/admin/v1
   ```

4. 起動して、2.7 のとおり確かめる。

- `COMPOSE_FILE=compose.yaml` だけの構成では、provisioner は下流の共有ネットワークに参加しない（provisioner 自身の default と `eapaka-provisioner` だけ）。下流のホストの VPN 側のアドレスで接続できることを確かめた（2026-10-10）。
- 下流のログの `src_ip` は、provisioner のアドレスではなく、下流の compose ネットワークのゲートウェイのアドレスになることがある。下流で provisioner を見分けるのは `mgmt_client`（2.2 の識別名）である。

### 3.4 別ホストの管理クライアントから接続する

管理クライアントが別のホストにある場合は、provisioner の API を VPN 側のアドレスで公開する。以下では、provisioner のホストの VPN 側のアドレスを `100.64.0.30` とする。

1. `.env` の `PROVISIONER_PUBLISH` を `100.64.0.30:9446` にし、`PROVISIONER_TLS_HOSTS` に `100.64.0.30` を加える。

   ```
   PROVISIONER_PUBLISH=100.64.0.30:9446
   PROVISIONER_TLS_HOSTS=localhost,127.0.0.1,eapaka-provisioner,100.64.0.30
   ```

2. 自己署名のサーバー証明書は生成済みのものに SAN が反映されないので、ボリューム `provisioner-data`（サーバー証明書だけが入っている）を消して作り直す。ボリュームの実際の名前は `<プロジェクト名>_provisioner-data`（プロジェクト名は既定ではディレクトリ名）。

   ```bash
   docker compose down
   ```

   ```bash
   docker volume rm eapaka-node-provisioner_provisioner-data
   ```

   ```bash
   docker compose up -d
   ```

3. 新しいサーバー証明書を取り出して（2.8）、管理クライアントに渡し直す。

管理クライアントからは `https://100.64.0.30:9446/admin/v1` で接続する。

## 4. 鍵の置き場所（PLMN マップと AVクライアントID）

本PoCの vector-gateway は、IMSI の PLMN と `VECTOR_GATEWAY_PLMN_MAP` で、認証ベクターの取得先を決める。provisioner は同じ規則で加入者の鍵の置き場所を決める。鍵を違う所に置くと、その加入者は認証できない。

| PLMN マップの接続方式 | 鍵の置き場所（`keyStore`） | 鍵の操作先 |
|---|---|---|
| `01` | `aka` | aka-only-server の管理API |
| `00`、またはマップにない | `poc` | 本PoCの Provisioning API |

- IMSI の先頭 6 桁、次に先頭 5 桁の順でマップと照合する。認可ポリシーは、置き場所によらず本PoCに置く。
- `.env` の `PROVISIONER_PLMN_MAP` には、本PoCの `VECTOR_GATEWAY_PLMN_MAP` と同じ値を書く。provisioner からは本PoCの設定を読めないので、運用で揃える。provisioner の `/status` の `plmnMap` と起動ログの `plmn map` で、今の値を確かめられる。
- 本PoCの vector-gateway を passthrough モード（`VECTOR_GATEWAY_MODE=passthrough`）で動かしている場合は、`PROVISIONER_PLMN_MAP` を空にする（すべて `poc`）。
- マップに `00` / `01` 以外の接続方式があるとき、`01` があるのに `PROVISIONER_AKA_URL` が空のとき、`PROVISIONER_AKA_URL` があるのに `PROVISIONER_AKA_AV_CLIENT_ID` が空のときは、設定の誤りとして provisioner は起動しない。
- PLMN マップを変えても、既にいる加入者の鍵は移らない。本PoCと provisioner の両方のマップを変えた後は、`GET /subscribers/{imsi}` の `issues` に `KEY_MISSING`（置き場所に鍵がない）や `KEY_IN_OTHER_STORE`（置き場所でない方に鍵がある）が出るので、加入者を作り直す。

`PROVISIONER_AKA_AV_CLIENT_ID` には、vector-gateway が aka-only-server に登録されている AVクライアントの ID を書く（2.3 の 4）。

- `aka` の加入者を作るとき、許可するクライアントをこの ID だけにする。変更では、許可するクライアントを変えない。
- 起動時と `/status` で、この ID の AVクライアントがあって有効かを確かめる。ない・無効の場合も provisioner は起動するが、`aka` の加入者の作成は aka-only-server に断られる（502）。`check-downstream` と `/status` の `avClient` で確かめる。
- vector-gateway の証明書を更新するときは、aka-only-server の管理API（`PUT /admin/v1/clients/{clientId}/certificate`）で差し替える。ID が変わらないので、provisioner の設定と加入者の許可はそのまま使える（aka-only-server の運用ガイド 2.3）。

`GET /subscribers/{imsi}` の `issues` は、2 つのノードの食い違いを示す。正常なら空である。

| `issues` の値 | 状態 | 対処 |
|---|---|---|
| `KEY_MISSING` | 置き場所に鍵がない | 加入者を作り直す（ポリシーだけ残っている場合は、削除してから作る） |
| `POLICY_MISSING` | 認可ポリシーがない（本PoCは認証を拒否する） | `PATCH /subscribers/{imsi}` で `policy` を設定する |
| `KEY_IN_OTHER_STORE` | 置き場所でない方にも同じ IMSI の加入者がある | PLMN マップの食い違いか、ほかの操作手段での登録を疑う。要らない方を下流の GUI 等で消す |
| `AV_CLIENT_NOT_ALLOWED` | `aka` の加入者が vector-gateway の AVクライアントを許可していない | aka-only-server の管理 GUI 等で許可するクライアントに加える |
| `OTHER_STORE_UNREACHABLE` | 置き場所でない方の下流に接続できず、`KEY_IN_OTHER_STORE` を確かめられなかった | 下流への接続を確かめる（10 章） |

## 5. 公開範囲

Docker が公開したポートは、ufw の規則を通らずに外部から届く。公開範囲は、`.env` のバインド先のアドレスで制御する。

| 変数 | 既定値 | 考え方 |
|---|---|---|
| `PROVISIONER_PUBLISH` | `127.0.0.1:9446` | 同じホストの管理クライアントは共有ネットワークで接続するので、このままでよい。別ホストの管理クライアントがある場合だけ、VPN 側のアドレスにする（3.4） |
| 本PoCの `PROVISIONING_API_BIND` | `127.0.0.1` | provisioner が別ホストにある場合だけ、VPN 側のアドレスにする（3.1） |
| aka-only-server の `AKA_ADMIN_PUBLISH` | `127.0.0.1:9443` | provisioner が別ホストにある場合だけ、VPN 側のアドレスにする（3.2） |

- provisioner の API は mTLS で、登録していない証明書や証明書なしの接続は TLS ハンドシェイクで拒否する。管理クライアントは全権を持つ（ユーザーや権限の概念は provisioner にない）。
- インターネットに面したホストでは、`0.0.0.0` にしない。

## 6. 操作の記録の確認と対応

加入者の作成・変更・削除は 2 つのノードにまたがるので、provisioner は手順ごとの状態を「操作の記録」として専用の Valkey に残しながら進める。途中で失敗したら、作成・変更は補償（作ったものを消す、ポリシーを元に戻す）で元に戻し、削除は残りの削除を続ける。

| 状態 | 意味 | 対応 |
|---|---|---|
| `running` | 要求を処理中 | ― |
| `completed` | すべての手順が成功した | ― |
| `rolled_back` | 途中で失敗し、補償で元に戻した | ― |
| `retrying` | 補償（作成・変更）か残りの削除が失敗し、後で自動でやり直す | 下流が直れば自動で終わる。原因（多くは下流に接続できない）を直す |
| `failed` | 自動のやり直しをやめた | 手での対応が要る（6.3） |
| `dismissed` | 手で直した後に閉じた | ― |

- 下流に接続できないなど、書き込みを始める前に分かる失敗では、操作の記録は作られず、要求は 503（`DOWNSTREAM_UNAVAILABLE`）で断られる。
- 未完了の操作がある IMSI への新しい操作は、409（`OPERATION_UNRESOLVED`）で断られる（6.3）。
- 要求の中で補償できなかったとき、要求は 500（`OPERATION_INCOMPLETE`）で、`operationId` と残った手順を返す。
- 完了した記録（`completed` / `rolled_back` / `dismissed`）は、7 日後に消える。

### 6.1 自動のやり直し

provisioner の中のワーカーが、30 秒ごとに未完了の操作を見て、続きを行う。

- やり直しが失敗するたびに、次に試みるまでの時間を倍にする（要求の中の失敗の後は 30 秒、以後 60 秒、120 秒…で、最大 10 分）。実際に試みるのは、その時刻を過ぎた後のワーカーの回である。
- 操作の作成から 24 時間たっても終わらなければ、自動のやり直しをやめて `failed` にし、ERROR のログ `operation gave up retrying; manual action is required` を出す。
- provisioner が要求の途中で落ちた場合、その操作は `running` のまま残る。60 秒（ロックの有効期限）を過ぎると、ワーカーが拾って同じように続ける。
- 例外として、鍵の変更の途中で落ちた変更は、鍵が変わったかどうか分からない（変更前の Ki / OPc は残さないので戻せない）。ワーカーは何もせずに `failed` にし、ERROR のログ `update operation needs a manual check: the key may or may not have been changed` を出す（6.3）。
- ワーカーの処理は、完了か `failed` になったときに、監査ログに `operation.resume` として残る（操作者と管理クライアントは空、トレースID は元の操作のもの）。

### 6.2 確かめる

未完了の操作の件数は `/status` の `operations` に出る。

```bash
pv GET /status
```

```json
{..."operations":{"running":0,"retrying":1,"failed":2}}
```

一覧は `GET /operations`（`running` / `retrying` / `failed`。`?status=failed` で絞り込める）、1 件は `GET /operations/{operationId}`（完了したものも 7 日間）で取得する。

```bash
pv GET '/operations?status=failed'
```

```json
{"items":[{"id":"01a12200-...-00000000000b","kind":"subscriber.delete","imsi":"440200999999999","keyStore":"aka","status":"failed","steps":[{"name":"policy.delete","downstream":"prov","state":"done"},{"name":"subscriber.delete","downstream":"aka","state":"failed","error":{"cause":"DOWNSTREAM_UNAVAILABLE","detail":"aka DELETE /admin/v1/subscribers/440200999999999: ... no such host","time":"..."}}],"attempts":1,"operator":"tester01","mgmtClient":"bff","traceId":"...","createdAt":"...","updatedAt":"..."}],"total":1}
```

`steps` が手順ごとの状態（`pending` / `done` / `failed` / `compensated` / `skipped`）で、`failed` の手順の `error` が最後の失敗である。`traceId` で、provisioner と下流のログ・監査ログを突き合わせられる（7 章）。

### 6.3 手で対応する

| 状況 | 対応 |
|---|---|
| `retrying` | 原因（下流の停止、証明書の誤りなど）を直せば、次のワーカーの回で終わる。待たずに閉じたい場合は、下流の状態を手で直してから `dismiss` する |
| `failed`（鍵の変更が分からない変更以外） | 原因を直してから `POST /operations/{operationId}/retry`。続きをその場で 1 回行い、成功すれば `completed`（削除）か `rolled_back`（作成・変更）になる。失敗すれば `retrying` に戻り、そこから 24 時間、自動のやり直しを再開する |
| `failed`（鍵の変更が分からない変更） | `retry` はできない（409 `OPERATION_STATE_CONFLICT`）。下流の加入者の鍵（Ki / OPc 等）を確かめ、必要なら `PATCH /subscribers/{imsi}` で正しい値にしてから、`POST /operations/{operationId}/dismiss` で閉じる |

```bash
pv POST /operations/<operationId>/retry
```

```bash
pv POST /operations/<operationId>/dismiss
```

- `retry` と `dismiss` は、監査ログに `operation.retry` / `operation.dismiss` として、要求した操作者と管理クライアントつきで残る。
- `dismiss` は下流に何もしない。下流を手で直した後に使う。
- 未完了（`running` / `retrying` / `failed`）の操作がある IMSI には、加入者の作成・変更・削除と、認可ポリシーの PUT・DELETE・停止・再開ができない（409 `OPERATION_UNRESOLVED`。応答の `operationId` がその操作）。古い操作の続き（補償・やり直し）が、新しい操作の結果を消さないようにするためである（例: 作成の補償が残ったまま同じ IMSI を作り直すと、作り直した加入者が補償で消される）。その操作を `retry` で終わらせるか、下流を手で直して `dismiss` で閉じてから、送り直す。同じ `Idempotency-Key` で送り直してよい（この 409 は覚えない）。

  ```json
  {"title":"Conflict","status":409,"detail":"an unresolved operation (retrying) remains on the same IMSI; retry or dismiss it first: /operations/01a12300-...-000000000012","cause":"OPERATION_UNRESOLVED","operationId":"01a12300-...-000000000012"}
  ```

## 7. ログと監査ログ

| 種類 | 内容 | 見る場所 |
|---|---|---|
| provisioner のログ | 要求ごとの `request completed`（トレースID つき）、下流の呼び出しの失敗、補償・やり直しの経過、起動時の下流の確認、接続の拒否 | `docker compose logs eapaka-provisioner`（標準出力の JSON） |
| provisioner の監査ログ | 変更操作、加入者の作成・変更・削除で書き込みを始めた後の失敗、秘密の値（Ki / OPc、共有シークレット）の取得、操作の記録への対応 | `GET /audit-logs`（`PROVISIONER_AUDIT_MAX` 件、既定 10000 件まで）。標準出力にも `"msg":"audit"` で出る |
| 本PoCの provisioning-api の監査ログ | provisioner が行った下流の操作（`mgmt_client` は provisioner の識別名、`operator` は `X-Operator-Id`） | `GET /prov/audit-logs`。すべては本PoCのホストの `deployments/logs_on_host/provisioning-api.log` |
| aka-only-server の監査ログ | 同上 | `GET /aka/audit-logs`。aka-only-server の運用ガイド 6 章 |

```bash
docker compose logs -f eapaka-provisioner
```

- provisioner は、操作ごとのトレースID（要求の `X-Trace-ID`。なければ採番）を下流 2 つにも渡す。provisioner の監査ログの `traceId` は、下流の監査ログの `traceId` と同じになる。補償・やり直しも、元の操作のトレースID を渡す。
- provisioner の監査ログの加入者の操作には、操作の記録の ID（`operationId`）と、手順ごとの結果（`details.steps`）が入る。
- 中継した操作（RADIUSクライアント、認可ポリシー）の監査ログには、変えた項目の名前だけを残す（値は下流の監査ログにある）。
- 加入者の停止・再開（2.10）は `policy.suspend` / `policy.resume` で残る。どちらになるかは、本PoCの応答（変更後の状態）で決まる。
- Ki、OPc、共有シークレットは、provisioner のログにも監査ログにも出ない。
- 監査ログは `PROVISIONER_AUDIT_MAX` 件を超えると古いものから消える。長く残したい場合は、Docker のログを外部に保存する。
- Docker のログは既定では無制限に増える。`/etc/docker/daemon.json` などでローテーションを設定しておく。

## 8. バックアップと復元

provisioner のデータは 2 つのボリュームにある。加入者などのデータは下流にあるので、下流のバックアップも別に取る（本PoCは B-02 §9 と O-04、aka-only-server はその運用ガイド 7 章）。

| ボリューム | 内容 | 失った場合 |
|---|---|---|
| `<プロジェクト名>_valkey-data` | 操作の記録、`Idempotency-Key` の記録、監査ログ | 未完了の操作の記録が消え、その補償・やり直しが行われなくなる。下流の加入者を `issues` で確かめて手で直す |
| `<プロジェクト名>_provisioner-data` | 自己署名のサーバー証明書と秘密鍵 | 起動時に作り直される（管理クライアントに渡し直す） |

以下はプロジェクト名が `eapaka-node-provisioner` の場合の例である。

### 8.1 バックアップ

```bash
docker compose stop
```

```bash
docker run --rm -v eapaka-node-provisioner_valkey-data:/data:ro -v "$PWD":/backup valkey/valkey:9.1.2 tar czf /backup/valkey-data.tgz -C /data .
```

```bash
docker compose start
```

バックアップのファイルは、コンテナの中で作るので root の所有（パーミッション 600）になる。移すときは `sudo` を使う。

### 8.2 復元

ボリュームを空にしてから、バックアップを展開する。現在のデータは消える。

```bash
docker compose down
```

```bash
docker run --rm -v eapaka-node-provisioner_valkey-data:/data -v "$PWD":/backup valkey/valkey:9.1.2 sh -c 'rm -rf /data/* && tar xzf /backup/valkey-data.tgz -C /data'
```

```bash
docker compose up -d
```

バックアップの時点で未完了だった操作は、復元の後にワーカーがもう一度続ける。補償とやり直しは何度行っても結果が同じ操作（削除、ポリシーの置き換え）だけでできているので、既に終わっていても害はない。復元の後は `GET /operations` で確かめる。

## 9. 更新

更新の前にバックアップを取っておく（8 章）。データはボリュームに残る。

```bash
git pull
```

設定項目が増えていないかを確かめる。`git pull` は更新前の位置を `ORIG_HEAD` に残すので、`.env.example` の変更を表示できる。何も表示されなければ、そのまま進む。

```bash
git diff ORIG_HEAD HEAD -- .env.example
```

増えた項目があれば、`.env` に書き足す。

ビルドし直して起動する。`/status` の `version` と起動ログの `starting` に出るバージョンは、`PROVISIONER_VERSION`（既定 `dev`）で決まる。git のコミットを出す場合は、ビルドのときに指定する。

```bash
PROVISIONER_VERSION=$(git rev-parse --short HEAD) docker compose up -d --build
```

```bash
docker compose exec -T eapaka-provisioner /eapaka-provisioner check-downstream
```

- 下流（本PoC、aka-only-server）を更新した場合も、`check-downstream` で接続を確かめる。
- 処理中の要求があるときに作り直すと、その操作は `running` のまま残り、60 秒後にワーカーが続ける（6.1）。

## 10. 障害時の確認

`docker compose ps` で `eapaka-provisioner` が unhealthy なら、API が待ち受けていない。`docker compose logs eapaka-provisioner` で起動のエラー（設定の誤り、Valkey に接続できない、証明書の読み込みの失敗など）を確かめる。

停止（`docker compose stop` / `down`、更新）には最大 30 秒かかる。ワーカーが処理中の操作を最後まで行ってから終わるため（`stop_grace_period`）。

まず `check-downstream` で下流への接続を確かめる。接続できない場合は、原因の見当が表示される。同じ内容は `/status` の `downstreams.*.hint` と、起動ログの `hint` にも出る。

```bash
docker compose exec -T eapaka-provisioner /eapaka-provisioner check-downstream
```

```
aka-only-server の管理API: https://aka-only-server:9443/admin/v1
  接続できませんでした: aka GET /admin/v1/status: Get "https://aka-only-server:9443/admin/v1/status": dial tcp: lookup aka-only-server on 127.0.0.11:53: no such host
  aka-only-server のホスト名（aka-only-server）を解決できません。aka-only-server が起動しているか、同一ホストの場合は provisioner が共有ネットワーク（aka-av）に参加しているか確認してください。
```

| 症状・表示 | 確認すること |
|---|---|
| provisioner が起動しない | `docker compose logs eapaka-provisioner` の `error:`。`PROVISIONER_ADMIN_CLIENTS` が空・書式の誤り、PLMN マップと aka の設定の組み合わせ（4 章）、`PROVISIONER_VALKEY_PASSWORD`、`certs/` のファイルの有無と所有者（UID 65532。`permission denied` なら 2.5） |
| `network eapaka-prov declared as external, but could not be found`（`aka-av` も同様） | 下流を先に起動しているか（本PoCは `--profile provisioning`）。下流側と `.env` の共有ネットワークの名前（`PROVISIONING_SHARED_NETWORK` / `AKA_SHARED_NETWORK`）が一致しているか。別ホストの下流なら `COMPOSE_FILE` から外す（3 章）。クライアント証明書を作るとき（2.2）は `-f compose.yaml` を付ける |
| 「…が provisioner のクライアント証明書を受け付けませんでした」 | 下流の `.env` の `PROVISIONING_API_ADMIN_CLIENTS` / `AKA_ADMIN_CLIENTS` に、`check-downstream` が表示するフィンガープリントが登録されているか。登録した後に下流を作り直したか。証明書が有効期間内か。下流のログ（本PoCは `PROV_CLIENT_REJECTED`、aka-only-server は `client certificate rejected`） |
| 「…のサーバー証明書が、設定した証明書（PROVISIONER_…_SERVER_CERT）と一致しません」 | `certs/prov-server.pem` が本PoCの `server.pem` と同じか。`certs/aka-server.pem` が aka-only-server の `admin-cert` の出力と同じか（作り直した後は置き直して、provisioner を `docker compose restart eapaka-provisioner` で再起動する） |
| サーバー証明書に接続先の名前（IP アドレス）が入っていない | 下流のサーバー証明書の SAN に、URL のホスト名（同一ホストなら `provisioning-api` / `aka-only-server`）か IP アドレスが入っているか（3.1・3.2） |
| 「ホスト名を解決できません」 | 下流が起動しているか。provisioner が共有ネットワークに参加しているか（`COMPOSE_FILE`） |
| 「時間内に応答がありません」 | 接続先のアドレス、VPN、下流側の公開先（`PROVISIONING_API_BIND`、`AKA_ADMIN_PUBLISH`） |
| vector-gateway の AVクライアントがない・無効 | `PROVISIONER_AKA_AV_CLIENT_ID` が aka-only-server の `client list` の ID と合っているか（2.3 の 4）。AVクライアントが有効か |
| 管理クライアントが接続できない（TLS ハンドシェイクで切れる。curl は終了コード 56） | provisioner のログの `admin client certificate rejected`。`reason` が `not configured` なら `PROVISIONER_ADMIN_CLIENTS` に `fingerprint` の値が登録されていない（登録したら `docker compose up -d` で作り直す）、`outside validity period` なら期限切れ |
| 管理クライアントがサーバー証明書の検証に失敗する | 管理クライアントの信頼する証明書が `server-cert` の出力と同じか。接続に使う名前が SAN（`server-cert` の標準エラーに出る）に入っているか（3.4） |
| 503 `DOWNSTREAM_UNAVAILABLE` | 応答の `downstream`（`prov` / `aka`）の下流に接続できない。`check-downstream` で確かめる |
| 502 `DOWNSTREAM_ERROR` | 下流が想定外の応答を返した。応答の `downstream` と `cause`、provisioner のログの `downstream call failed`、下流のログ。aka の `CLIENT_NOT_FOUND` は `PROVISIONER_AKA_AV_CLIENT_ID` の誤り |
| 409 `OPERATION_IN_PROGRESS` | 同じ IMSI の操作か、同じ `Idempotency-Key` の要求が処理中。少し待ってからやり直す |
| 409 `OPERATION_UNRESOLVED` | 同じ IMSI に未完了の操作がある。応答の `operationId` の操作を `retry` か `dismiss` で片付けてから送り直す（6.3） |
| 500 `OPERATION_INCOMPLETE`、`/status` の `operations` の `retrying` / `failed` が 0 でない | 6 章 |
| 500 `SYSTEM_FAILURE` | provisioner 専用の Valkey に接続できない（`docker compose ps valkey`）。書き込みは断るが、読み取りは続けて行う |
| 加入者が認証できない | `GET /subscribers/{imsi}` の `issues`（4 章）。PLMN マップが本PoCの `VECTOR_GATEWAY_PLMN_MAP` と同じか。本PoC側のログ（vector-gateway の `BACKEND_EXTERNAL_ERR` など。B-02 §14.9） |
| 下流の `docker compose down` で `Network … Resource is still in use` と出る | provisioner が共有ネットワークに参加しているため、ネットワークが残っただけで、問題はない。下流を起動し直せば、provisioner は再起動なしで接続し直す（2026-10-10 に本PoCで確認） |

## 11. 環境変数

`.env` に書く。compose は `.env` の値をコンテナの環境変数として渡す。

| 変数 | 既定値 | 内容 |
|---|---|---|
| `COMPOSE_FILE` | `compose.yaml:compose.eapaka-prov.yaml:compose.aka-av.yaml` | 重ねる compose ファイル（3 章の表） |
| `PROVISIONING_SHARED_NETWORK` | `eapaka-prov` | 同一ホストの本PoCが作る共有ネットワークの名前（本PoC側と同じにする） |
| `AKA_SHARED_NETWORK` | `aka-av` | 同一ホストの aka-only-server が作る共有ネットワークの名前（aka-only-server 側と同じにする） |
| `PROVISIONER_SHARED_NETWORK` | `eapaka-provisioner` | provisioner が作る、同一ホストの管理クライアント向けの共有ネットワークの名前 |
| `PROVISIONER_VALKEY_PASSWORD` | （必須） | provisioner 専用の Valkey のパスワード |
| `PROVISIONER_ADMIN_CLIENTS` | （必須） | 管理クライアントの `識別名=フィンガープリント`（カンマ区切り。識別名は英数字と `.` `_` `-` の 64 文字まで） |
| `PROVISIONER_PROV_URL` | `https://provisioning-api:9444/admin/v1` | 本PoCの Provisioning API のベース URL |
| `PROVISIONER_AKA_URL` | （空: aka-only-server を扱わない） | aka-only-server の管理API のベース URL |
| `PROVISIONER_AKA_AV_CLIENT_ID` | （`PROVISIONER_AKA_URL` を設定したら必須） | vector-gateway の AVクライアントID |
| `PROVISIONER_PLMN_MAP` | （空: すべて `poc`） | PLMN マップ（本PoCの `VECTOR_GATEWAY_PLMN_MAP` と同じ値） |
| `PROVISIONER_CLIENT_KEY` | （空: クライアント証明書のファイルから読む） | 下流用のクライアント証明書の秘密鍵を別のファイルにした場合の、コンテナ内のパス（`/certs/...`） |
| `PROVISIONER_DOWNSTREAM_TIMEOUT` | `5s` | 下流の 1 回の呼び出しの上限時間。10 秒まで（超えると起動しない） |
| `PROVISIONER_PUBLISH` | `127.0.0.1:9446` | provisioner の API を公開するホスト側のアドレスとポート |
| `PROVISIONER_TLS_HOSTS` | `localhost,127.0.0.1,eapaka-provisioner` | 自己署名のサーバー証明書の SAN（生成済みの証明書には反映されない。3.4） |
| `PROVISIONER_TLS_CERT` / `PROVISIONER_TLS_KEY` | （空: 自己署名） | 持ち込みのサーバー証明書と秘密鍵のコンテナ内のパス（`/certs/...`）。ファイルを置き換えると、次の接続から読み直す |
| `PROVISIONER_LOG_LEVEL` | `info` | ログのレベル（debug / info / warn / error） |
| `PROVISIONER_AUDIT_MAX` | `10000` | 監査ログの保持件数 |
| `PROVISIONER_VERSION` | `dev` | `/status` とログに出すバージョン（ビルド時に埋め込む） |

コンテナの中では、このほかに `PROVISIONER_ADDR`（`:9446`）、`PROVISIONER_VALKEY_ADDR`（`valkey:6379`）、`PROVISIONER_CLIENT_CERT`（`/certs/client.pem`）、`PROVISIONER_PROV_SERVER_CERT`（`/certs/prov-server.pem`）、`PROVISIONER_AKA_SERVER_CERT`（`/certs/aka-server.pem`）を使う。compose を使わずに動かす場合は、これらも指定する（`PROVISIONER_TLS_CERT` / `_KEY` の既定は `/data/tls/cert.pem` / `key.pem`）。

下流の証明書（`certs/client.pem`、`prov-server.pem`、`aka-server.pem`）は起動時に読むので、置き換えたら `docker compose restart eapaka-provisioner` で再起動する。
