# eapaka-node-provisioner 設計概要

- 状態: 初版（2026-10-10）。設計の確認事項に合意済み。§16 のステップ 7（運用ガイド・README、simwifi での確認）まで完了。加入者の停止・再開（§15 の 4）を加えた。導入と運用の手順は `docs/operation-guide.md`。
- 対象: 統合API（コマンド名 `eapaka-provisioner`。以下「provisioner」）。
- 関連:
  - 本PoC（eapaka-radius-server-poc）の Provisioning API: `docs/D-13_Provisioning_API詳細設計書_r*.md`、`docs/openapi/provisioning-api.yaml`
  - aka-only-server の管理API: `docs/design-overview.md`、`docs/openapi/admin-api.yaml`
  - Vector Gateway の接続方式と PLMN マップ: 本PoCの `docs/D-12_Vector_Gateway_詳細設計書_r*.md`
  - 手本にする実装: web-gui-for-eapaka-radius（以下「BFF」）の `internal/provapi`、web-gui-for-aka-only-server の `internal/adminapi`
- API の契約: `docs/openapi/provisioner-api.yaml`（0.3.0）。項目・エラーの詳細はそちらに書き、本書では方針を書く。

## 1. 位置づけ

EAP-AKA RADIUS PoC（以下「本PoC」）の Provisioning API と、aka-only-server の管理API を組み合わせて操作する統合API。加入者を「IMSI＋鍵の置き場所＋認可ポリシー」として扱い、2 つのノードにまたがる操作の調整（順序、排他、失敗時の補償）を受け持つ。

```
[BFF など] ──mTLS（X-Operator-Id / X-Trace-ID / Idempotency-Key）──> [provisioner] ──> Valkey（provisioner 専用）
                                                                       ├─mTLS─> provisioning-api（本PoC。以下「prov」）
                                                                       └─mTLS─> aka-only-server 管理API（以下「aka」）
```

- 加入者の鍵の置き場所は、本PoCの Vector Gateway の接続方式に対応する 2 つ:
  - `poc`（接続方式 `00`）: 鍵（Ki / OPc）を本PoCの `sub:{IMSI}` に持つ。prov の `/subscribers` で操作する。
  - `aka`（接続方式 `01`）: 鍵を aka-only-server が持ち、本PoCの vector-gateway が aka-only-server の AVクライアントとして認証ベクターを取得する。aka の `/subscribers` で操作する。
- 認可ポリシー（`policy:{IMSI}`）は、置き場所によらず本PoCに持つ。prov の `/policies` で操作する。
- 正本は下流の 2 つ（prov / aka）であり、provisioner は加入者のデータを保存しない。provisioner の Valkey に持つのは、操作の記録・排他・再送の判定・監査ログだけである（§8）。
- 画面のロジックは持たない（BFF の責務）。ユーザーや権限の概念も持たず、管理クライアントを全権として扱う（下流と同じ）。

## 2. 範囲

| 区分 | 内容 |
|---|---|
| 対象 | 本PoC 1 ノードと aka-only-server 1 台の組。加入者の統合操作（作成・取得・変更・削除・鍵の取得・一覧）、RADIUSクライアント・認可ポリシー（加入者の停止・再開を含む）・セッション・監査ログの中継、vector-gateway の AVクライアントの参照、操作の記録と補償・やり直し |
| 対象外 | 複数ノード、AVクライアントと AV用サーバー証明書の管理（aka 版 GUI で行う）、aka-only-server のサーバーログの参照、CSV の一括操作（本PoCの Admin TUI だけで行う）、セッションの切断、操作者ごとの権限、IPv6 の RADIUSクライアント |

将来の拡張は §14。

## 3. 技術方針

他の Go のリポジトリ（aka-only-server、BFF 2 つ）と同じ。

- 言語: Go 1.27.2。標準ライブラリを優先する（`net/http` のメソッドつき ServeMux と `r.PathValue`、`crypto/tls`、`encoding/json/v2`、`log/slog`）。
- 外部依存は `github.com/valkey-io/valkey-go` だけとする（プロジェクト全体で認めてきたもの）。
- module パス: `github.com/oyaguma3/eapaka-node-provisioner`。コマンド名は `eapaka-provisioner`。
- 設定は環境変数だけで受け取り、名前の接頭辞は `PROVISIONER_` とする（§11）。
- 配備: Docker Compose（provisioner と専用の Valkey）。コンテナは distroless の nonroot（UID 65532）で動かす。
- 下流の API のクライアントは、BFF の `internal/provapi` と aka 版 GUI の `internal/adminapi` をコピーして手直しする（共有モジュールにはしない）。
- コードのコメントとドキュメントは日本語で書く。
- ブランチは `main` に直接コミットする（他の GUI と aka-only-server と同じ）。

## 4. 鍵の置き場所の決め方

本PoCの vector-gateway は、IMSI の PLMN と `VECTOR_GATEWAY_PLMN_MAP` で接続方式を決める（D-12 §3）。鍵をそれと違う所に置くと、その加入者は認証できない。provisioner は同じ規則で置き場所を決める。

- 環境変数 `PROVISIONER_PLMN_MAP` に、本PoCの `VECTOR_GATEWAY_PLMN_MAP` と同じ形式（`PLMN:ID,PLMN:ID,...`）で同じ値を設定する。両者は運用で揃える（provisioner からは本PoCのマップを読めない）。`/status` にマップを出し、目で比べられるようにする。
- IMSI の先頭 6 桁、次に先頭 5 桁の順でマップと照合する（D-12 §3.4 と同じ）。`01` に当たれば `aka`、それ以外（一致なし、`00`）は `poc`。
- マップに `00` / `01` 以外の ID があれば、設定の誤りとして起動しない（provisioner はそれ以外の接続方式を扱えない）。aka の接続先（`PROVISIONER_AKA_URL`）を設定していないのに `01` があれば、同じく起動しない。
- 加入者の作成で呼び出し側が `keyStore` を指定した場合、マップから決まるものと違えば 400（`KEY_STORE_MISMATCH`）とする。省略すればマップから決める。
- 置き場所の変更（`poc` と `aka` の間で鍵を移す）は扱わない。PLMN 単位で切り替わるため、1 加入者だけを移すことはできない（§14 のジョブで検討する）。

## 5. vector-gateway の AVクライアント

`aka` の加入者は、aka-only-server の「許可クライアントID の集合」（`allowedClientIds`）に、本PoCの vector-gateway の AVクライアントID を含める必要がある（aka-only-server の設計 §7.2）。

- 環境変数 `PROVISIONER_AKA_AV_CLIENT_ID` にその ID を設定する（aka 版 GUI の AVクライアントの一覧で確かめる）。
- 起動時と `/status` で、aka の `GET /clients/{id}` によりその ID が存在して有効かを確かめ、ログと `/status` に出す。存在しなくても provisioner は起動する（`aka` の加入者の作成は aka が `CLIENT_NOT_FOUND` で断る）。
- `aka` の加入者の作成では `allowedClientIds` をこの ID だけにする。
- `aka` の加入者の変更では `allowedClientIds` を送らない（aka 側に他の AVクライアントの ID があっても消さない）。
- PLMN マップで `01` に当たる IMSI の aka 側の加入者は、本PoCの加入者として扱う（§15 の 2）。

## 6. 加入者の統合操作

### 6.1 リソースの形（概要）

項目の詳細は OpenAPI で定める。

```json
{
  "imsi": "001010000000001",
  "keyStore": "aka",
  "key": { "amf": "8000", "sqn": "000000000020", "sqnType": "inc32", "allowPlain": false, "createdAt": "..." },
  "policy": { "default": "allow", "rules": [] },
  "status": "active",
  "issues": []
}
```

- `key` は置き場所の加入者の属性（Ki / OPc は含まない）。`sqnType` / `allowPlain` / `allowedClientIds` / `updatedAt` は `aka` のときだけ含む。
- `policy` は prov の認可ポリシー。ない場合は省略する。
- `status` は加入者の状態（`active` / `suspended`）で、prov の認可ポリシーの `status` をそのまま出す（§15 の 4）。認可ポリシーがない場合は省略する。`policy` の中に入れないのは、`policy` を変更（PATCH）の本文と同じ形に保つため（状態は作成・変更では指定できない）。
- `issues` は 2 つのノードの状態の食い違い（下表）。正常なら空。

| `issues` の値 | 状態 |
|---|---|
| `KEY_MISSING` | 置き場所に鍵（加入者）がない |
| `POLICY_MISSING` | 認可ポリシーがない（本PoCは認証を拒否する。D-09 §8.7） |
| `KEY_IN_OTHER_STORE` | 置き場所でない方にも同じ IMSI の加入者がある（`poc` では、aka 側の加入者が vector-gateway の AVクライアントID を許可している場合だけ。それ以外は本PoCと関係のない加入者とみなす。`aka` では、prov に加入者がある場合） |
| `AV_CLIENT_NOT_ALLOWED` | `aka` の加入者の `allowedClientIds` に vector-gateway の AVクライアントID がない |
| `OTHER_STORE_UNREACHABLE` | 置き場所でない方の下流に接続できず、`KEY_IN_OTHER_STORE` を確かめられなかった（取得はそのまま返す） |

### 6.2 操作

| 操作 | 下流への呼び出し（順） | 失敗したとき |
|---|---|---|
| 作成 `POST /subscribers` | ① 3 か所の存在確認（prov の加入者・aka の加入者・ポリシー。aka の加入者は、置き場所が `aka` なら有無、`poc` なら AVクライアントID を許可しているもの）。どれかがあれば 409（`conflicts` にある場所） ② 置き場所に加入者を作成 ③ ポリシーを PUT | ③ が失敗したら ② を削除して戻す（補償） |
| 取得 `GET /subscribers/{imsi}` | prov の加入者・aka の加入者・ポリシーを並行して取得 | 3 か所のどこにもなければ 404 |
| 変更 `PATCH /subscribers/{imsi}` | ① 変更前のポリシーを記録 ② ポリシーを PUT（`policy` を指定したとき） ③ 置き場所の加入者を PATCH（鍵の属性を指定したとき） | ③ が失敗したら ② を変更前に戻す（変更前になければ削除） |
| 削除 `DELETE /subscribers/{imsi}` | ① ポリシーを削除 ② 置き場所の加入者を削除（どちらも 404 は削除済みとして扱う）。置き場所でない方の加入者（`KEY_IN_OTHER_STORE`）は削除しない | 戻せないため、残りを後でやり直す（前に進める） |
| 鍵の取得 `GET /subscribers/{imsi}/keys` | 置き場所の `/subscribers/{imsi}/keys` | — |
| 一覧 `GET /subscribers` | §6.4 | — |

- 作成では、鍵の値（Ki / OPc）と `policy` を必須とする。ポリシーのない加入者は認証できないため。
- 作成で鍵を先に作るのは、途中の状態（鍵だけある）でも認証が拒否されるため（ポリシーがない）。削除でポリシーを先に消すのも同じ理由（以後の認証は拒否される）。
- 変更では、変更前のポリシーは記録できるが、変更前の Ki / OPc は provisioner の Valkey に残さない（§8）。このため、戻す必要のあるポリシーの変更を先に行い、戻せない鍵の変更を最後にする。
- `keyStore` は変更できない（§4）。
- 変更で鍵の項目を指定したが置き場所に加入者がない場合は 404（`USER_NOT_FOUND`）。`policy` だけの変更は、鍵がなくても行える（ポリシーだけが残った状態を直せるように）。
- 作成と変更の入力は、下流と同じ規則で provisioner が先に検証する（IMSI、16進の桁数、ポリシーの規則。規則は OpenAPI に書く）。下流への書き込みを始めてから検証で断られて補償する、という流れを通常は起こさないため。下流の検証は最後の守りとして残る（断られたら補償する）。
- 存在確認（①）から書き込みまでの間に、Admin TUI や aka 版 GUI で同じ IMSI を作られた場合は、下流の 409 で失敗して補償する。
- 置き場所が `poc` の作成で、置き場所でない方の aka に接続できない場合は、その確認だけを省いて作成する（aka 側の加入者は認証に影響しないため）。応答の `issues` に `OTHER_STORE_UNREACHABLE` が入る。置き場所の下流と prov に接続できなければ、書き込む前に 503 を返す。
- 書き込みを始めたら、管理クライアントとの接続が切れても最後まで行う（途中で止めると補償の要る状態が残るため）。

### 6.3 下流のエラーの返し方

- 下流の 400（呼び出し側の入力に由来するもの）と 409 は、同じステータスと `cause` で返す。`invalidParams` の `param` は統合リソースの項目名に付け替える（例: ポリシーの `rules[0].vlanId` → `policy.rules[0].vlanId`）。
- 下流に接続できない・タイムアウトは 503（`DOWNSTREAM_UNAVAILABLE`）、下流の想定外の応答（5xx、ProblemDetails でない応答、呼び出し側では直せない 4xx。例: aka が `CLIENT_NOT_FOUND` で設定の AVクライアントID を断った）は 502（`DOWNSTREAM_ERROR`）。どちらも ProblemDetails の拡張項目 `downstream`（`prov` / `aka`）と、分かれば下流の `cause` を含める。接続できない原因の見当（diagnose）はログと `/status` に出す。

### 6.4 一覧

prov の加入者、aka の加入者、ポリシーの 3 つの一覧を、同じ `cursor`（IMSI）と `prefix` で取り寄せ、IMSI の昇順にマージする。

- 3 つとも IMSI の昇順で返すので、それぞれから `limit` 件を取れば、和集合の先頭 `limit` 件は正しく求まる。和集合が `limit` 件を超えるか、どれかに続きがあれば、返した最後の IMSI を `nextCursor` にする。
- aka の加入者は、PLMN マップで `01` に当たる IMSI だけを対象にする。aka-only-server には本PoCと関係のない加入者もいるため。除いた分だけ件数が足りなくなれば、aka の次のページを取り寄せる。
- 各項目は取得（§6.1）と同じ形にする（3 つの一覧の項目から組み立てられる）。ただし `poc` の IMSI について aka 側の加入者は調べない（`KEY_IN_OTHER_STORE` は取得でだけ分かる場合がある）。
- 正確な `total`（和集合の件数）は返さない（§14 の一覧の改善）。

## 7. 中継する操作

| provisioner のパス | 下流 | 備考 |
|---|---|---|
| `/clients`、`/clients/{clientId}`、`/clients/{clientId}/secret` | prov の同じパス | RADIUSクライアント |
| `/policies`、`/policies/{imsi}` | prov の同じパス | 加入者の操作と同じ IMSI のロックを取る（§9.1） |
| `/policies/{imsi}/status`（PUT） | prov の同じパス | 加入者の停止・再開（§15 の 4）。加入者の操作と同じ IMSI のロックを取る（§9.1） |
| `/sessions` | prov の `/sessions` | 読み取りだけ |
| `/prov/audit-logs` | prov の `/audit-logs` | 読み取りだけ。下流の形のまま返す |
| `/aka/audit-logs` | aka の `/audit-logs` | 読み取りだけ。下流の形のまま返す |
| `/aka/av-clients/{clientId}` | aka の `GET /clients/{clientId}` | 読み取りだけ。vector-gateway の AVクライアントの確認用 |

- 要求と応答の形は、下流と同じにする（BFF が `ProvAPI` の実装を差し替える程度で付け替えられるように）。要求の本文は検証せずにそのまま送り、下流の応答（エラーを含む）をそのまま返す（ProblemDetails に `downstream` を加える）。`Location` は provisioner のパスに書き換える。
- パスの値（IMSI、ID）は、provisioner で形式を確かめてから下流に送る。下流のクライアントも、値を 1 つのセグメントとしてエスケープし、空・`.`・`..` は送らない（`/clients/1%2Fsecret` のような値が、別の操作 `/clients/1/secret` として下流に届かないようにする）。
- 下流の監査ログは、下流ごとに形が違う（prov は `details` が文字列、aka は `detail` がオブジェクト）ため、`/audit-logs` の引数で切り替えず、別のパスにする。provisioner 自身の監査ログは `/audit-logs`（§10.2）。aka を設定していなければ `/aka/...` は 404（`DOWNSTREAM_NOT_CONFIGURED`）。
- 中継の書き込みにも、`X-Operator-Id` の転送、監査ログ、`Idempotency-Key`（§9.2）を適用する。

## 8. データモデル（provisioner 専用 Valkey）

| キー | 型 | 内容 |
|---|---|---|
| `op:{opId}` | Hash | 操作の記録（§9.3）。種類、IMSI、置き場所、状態、手順ごとの状態（JSON）、試行回数、次に処理してよい時刻、操作者、管理クライアント、トレースID、作成・更新日時、変更前のポリシー。完了（`completed` / `rolled_back` / `dismissed`）したものは 7 日で消す。記録の書き込みは MULTI / EXEC で `ops:active` と一緒に行う |
| `ops:active` | Sorted Set | 未完了の操作の ID（スコアは次に処理してよい時刻） |
| `lock:imsi:{imsi}` | String | IMSI ごとのロック（値は取得者のトークン。有効期限つき） |
| `idem:{mgmtClient}:{keyHash}` | Hash | 再送の判定（§9.2）。要求のハッシュ、処理中かどうか、応答（ステータスと本文）。有効期限 24 時間 |
| `audit` | Stream | provisioner の監査ログ（§10.2）。件数の上限つき |

- 秘密の値（Ki / OPc、共有シークレット）は保存しない。`op:` には変更前のポリシー（秘密の値を含まない）だけを残す。`idem:` に残す応答は書き込みの応答で、秘密の値を含まない。要求は本文を保存せず、ハッシュ（SHA-256）だけを残す。
- AOF を有効にして `appendfsync always` とする（aka-only-server と同じ。操作の記録を補償・やり直しに使うため、BFF の `everysec` より強くする）。ポートは公開せず、`requirepass` を設定する。
- Valkey に接続できないとき、書き込み（ロックと操作の記録が要る）は 500（`SYSTEM_FAILURE`。下流と同じく Valkey のエラーは 500）で断り、下流は呼ばない。読み取りは続けて行う。

## 9. 排他・再送・操作の記録

### 9.1 IMSI ごとのロック

- 加入者の作成・変更・削除と、ポリシーの PUT・DELETE・停止・再開は、`lock:imsi:{imsi}` を `SET NX PX` で取ってから行う。取れなければ待たずに 409（`OPERATION_IN_PROGRESS`）を返す。
- 有効期限は、1 つの操作にかかる最大の時間（下流の呼び出しのタイムアウト × 手順の数）より長くする（既定 60 秒）。解放はトークンを比べて消す（Lua）。
- ロックは provisioner を通る操作どうしでだけ効く。Admin TUI、aka 版 GUI、下流の API を直接呼ぶ操作とは排他できない（下流に条件つきの書き込みがないため）。provisioner を使う間は、同じ加入者をそれらで操作しない運用とする。

### 9.2 再送の判定（Idempotency-Key）

- 書き込みの要求に任意のヘッダー `Idempotency-Key`（印字可能 ASCII 1〜128 文字）を付けられる。
- 同じ管理クライアントから同じキーで届いた要求は、24 時間のあいだ、最初の応答をそのまま返す（下流は呼ばない）。返したことは応答ヘッダー `Idempotent-Replayed: true` で示す。
- 最初の要求がまだ処理中なら 409（`OPERATION_IN_PROGRESS`）、同じキーで要求の内容（メソッド・パス・本文）が違えば 422（`IDEMPOTENCY_KEY_MISMATCH`）。
- ヘッダーがなければ判定しない（従来どおり、作成のやり直しは 409 で分かる）。
- 応答を覚えるのは 4xx までとする。5xx（下流に接続できない等）は覚えずに消し、同じキーでやり直せるようにする。同じ IMSI の操作が処理中の 409（`OPERATION_IN_PROGRESS`）と、同じ IMSI に未完了の操作がある 409（`OPERATION_UNRESOLVED`。§9.3）も一時的なので覚えない。
- 処理中の記録の有効期限は、ロックと同じ 60 秒とする（プロセスが落ちて処理中のまま残っても、その時間が過ぎればやり直せる）。完了したら 24 時間に延ばす。
- 同じ要求かどうかは、メソッド・パス（エスケープしたもの）・クエリ・本文のハッシュで比べる。

### 9.3 操作の記録と補償

複数の手順からなる操作（加入者の作成・変更・削除）は、下流を呼ぶ前に `op:{opId}` を作り、手順を進めるたびに状態を書き込む。

| 状態 | 意味 |
|---|---|
| `running` | 要求を処理中 |
| `completed` | すべての手順が成功した |
| `rolled_back` | 途中で失敗し、補償で元に戻した |
| `retrying` | 補償（作成・変更）または残りの手順（削除）が失敗し、後でやり直す |
| `failed` | やり直しの上限に達した。手での対応が要る |
| `dismissed` | 手で直した後に閉じた |

- **要求の中の補償:** 途中で失敗したら、その場で補償する。戻せたら `rolled_back` とし、最初の失敗のエラーに `operationId` と `rolledBack: true` を付けて返す。
- **反映されたかどうか:** 下流が 4xx で断った手順は「反映されていない」とみなし、補償しない（作成の 409 は、provisioner の外で作られたものなので消さない）。接続できない・タイムアウト・5xx の手順は、書き込まれたかどうか分からないので、反映された可能性があるとみなして補償する（既にないものの削除は成功とするので安全）。
- **補償の手順:** 作成は、ポリシーを PUT した可能性があれば `policy.delete`、加入者を作った可能性があれば `subscriber.compensate` を、この順に加えて行う（存在確認で、どちらもなかったことを確かめてある）。変更は、ポリシーを PUT した可能性があれば `policy.restore`（変更前に戻す。変更前になければ削除）を加える。要求の中で行わなかった手順は `skipped` にする。変更前のポリシーは状態（`status`）を含めずに覚え、PUT で戻す。prov のポリシーの PUT は状態を変えないので、停止中の加入者の補償で停止が解けることはない（§15 の 4）。
- **記録が書けないとき:** 最初の記録が書けなければ、下流には何もせず 500（`SYSTEM_FAILURE`）を返す。書き込みを始めた後に記録が書けなくなったら、成功として返さずにその場で補償する（最後の「完了」だけが残らないと、ワーカーが成功した操作を取り消してしまうため）。例外として、鍵の変更が済んだ後の「完了」が書けない場合は、鍵は戻せないので、1 回だけ書き直しを試みたうえで成功として返す（それでも書けなければ記録は実行中のまま残り、ワーカーが `failed` にする。下の「鍵の変更が分からない変更」）。削除は、記録が書けなくても前に進める。
- **実行中の記録:** 実行中の操作の「次に処理してよい時刻」は、作成時刻にロックの有効期限（60 秒）を足したもの。これを過ぎても実行中なら、要求が落ちたとみなす。
- **戻せなかったとき:** `retrying` にし、500（`OPERATION_INCOMPLETE`）に `operationId` と残った手順を付けて返す。ERROR のログを出す。
- **やり直し:** provisioner の中のワーカーが 30 秒ごとに `ops:active` を見て、処理してよい時刻を過ぎた操作の IMSI のロックを取り、記録を読み直してから続きを行う（ロックが取れなければ次の回に回す）。作成と変更は補償を続け（後ろに戻す）、削除は残りの削除を続ける（前に進める）。失敗するたびに次に試みるまでの時間を倍にする（30 秒から始めて最大 10 分）。記録の「自動のやり直しをやめる時刻」（作成から 24 時間。手でやり直すと、その時点から 24 時間に延びる）を過ぎたら `failed` にする。`failed` は `ops:active` のスコアを `+inf` にして、自動では拾わない。
- **ワーカーが下流に渡すもの:** 元の操作の操作者（`X-Operator-Id`）とトレースID（`X-Trace-ID`）を渡す。下流の監査ログでは、補償も元の操作と同じトレースID で突き合わせられる。
- **鍵の変更が分からない変更:** 実行中のまま残った変更の記録で、鍵の手順が終わっておらず、その前のポリシーの手順が済んでいる（またはポリシーの手順がない）ものは、鍵の変更が反映されたかどうか分からない（変更前の Ki / OPc は残さないので戻せない）。ワーカーは自動では何もせず `failed` にして ERROR のログを出す（2026-10-10 決定）。手でのやり直し（`retry`）もできず（409）、人が下流の状態を確かめて直した後に `dismiss` で閉じる。ポリシーの手順が終わっていなければ、鍵には進んでいないので、ポリシーを変更前に戻す。
- **プロセスが落ちたとき:** `running` のまま更新されない操作も、ロックの有効期限を過ぎればワーカーが拾って同じように処理する。下流を呼んだ後、記録を書く前に落ちた場合に備え、補償とやり直しは何度行っても結果が同じ操作（削除の 404 は成功とみなす、ポリシーの PUT）だけで組む。
- **未完了の操作がある IMSI への新しい操作:** 同じ IMSI に未完了（`running` / `retrying` / `failed`）の記録があれば、加入者の作成・変更・削除と、ポリシーの PUT・DELETE・停止・再開を 409（`OPERATION_UNRESOLVED`。その操作の `operationId` 付き）で断り、下流は呼ばない（§15 の 3）。古い操作の続き（補償・やり直し）が、新しい操作の結果を消さないようにするため（例: 作成の補償が残ったまま作り直すと、作り直した加入者が補償で消される）。IMSI のロックを取った後に `ops:active` の記録を読んで確かめる（ロックの間は同じ IMSI の記録は増えず、ロックを持つ実行中の操作もないので、見つかる `running` は要求が落ちたもの）。未完了の操作は通常は少ないので、索引は設けずに全件を読む。`retry` と `dismiss`、ワーカーは対象外（片付けるための操作のため）。
- **確認と手での対応:** `GET /operations`（`running` / `retrying` / `failed` の一覧）、`GET /operations/{opId}`（完了したものも保持期間のあいだ）、`POST /operations/{opId}/retry`（`failed` の続きをその場で 1 回行い、失敗すれば `retrying` に戻して自動のやり直しを再開する）、`POST /operations/{opId}/dismiss`（`retrying` / `failed` を手で直した後に `dismissed` にして閉じる）。どちらも IMSI のロックを取る。件数は `/status` にも出す。
- **監査ログ:** ワーカーの処理は、結果が完了（`completed` / `rolled_back`）か `failed` になったときだけ `operation.resume` として残す（`retrying` のままの途中経過は残さない。操作者と管理クライアントは空、トレースID は元の操作のもの）。手での操作は `operation.retry` / `operation.dismiss` として、要求者つきで残す。監査ログを残す処理は `internal/audit` にまとめ、HTTP の要求とワーカーの両方が使う。
- 操作の ID は UUID version 7（時刻順に並ぶ。Go の標準ライブラリで作れる）。

## 10. 認証・操作者・トレース・ログ

### 10.1 provisioner の API の認証

- 下流と同じく mTLS 必須とし、クライアント証明書の SHA-256 フィンガープリントを固定する。管理クライアントは `PROVISIONER_ADMIN_CLIENTS`（`識別名=フィンガープリント,...`）で静的に設定する。識別名は監査ログの `mgmtClient` になる。
- サーバー証明書は自己署名（ファイルがなければ起動時に生成し、ボリュームに保存する。SAN は `PROVISIONER_TLS_HOSTS`）または持ち込み。BFF のブラウザ向け HTTPS と同じ作り。`eapaka-provisioner server-cert` で現在の証明書（PEM）とフィンガープリントを表示し、BFF に検証用として渡す。
- TLS 1.2 以上、HTTP/2 に対応する。

### 10.2 操作者・トレースID・監査ログ

- `X-Operator-Id`（`^[A-Za-z0-9._@-]{1,64}$`）を受け取り、そのまま下流 2 つに渡す。下流の監査ログには「操作者＝BFF の利用者、`mgmt_client`＝provisioner」が残る。
- `X-Trace-ID`（印字可能 ASCII 1〜64 文字）を受け取るか採番し（16 バイトの乱数の 16 進 32 桁）、同じ値を下流 2 つに渡して応答のヘッダーでも返す。prov と aka（管理API 0.2.0 以降）は、ログと監査ログに記録する（§15 の 1）。
- provisioner の監査ログは、標準出力（JSON）に出し、あわせて Valkey の Stream `audit` に保存する（上限 `PROVISIONER_AUDIT_MAX`、既定 10000）。保存に失敗しても操作は成功として扱い、ERROR のログを出す（prov と同じ）。
- 監査ログの項目: `id`、`time`、`operator`、`mgmtClient`、`action`、`target`、`traceId`、`operationId`、`result`（`completed` / `rolled_back` / `retrying` / `failed` / `dismissed`）、`details`（下流ごとの結果。秘密の値は含まない）。`action` は下流と同じ命名（`subscriber.create`、停止・再開の `policy.suspend` / `policy.resume` 等）に、`operation.resume`（自動のやり直しの結果。操作者と管理クライアントは空）、`operation.retry`、`operation.dismiss` を加える。
- 記録するのは、変更操作が成功したとき、加入者の作成・変更・削除で下流への書き込みを始めた後に失敗したとき、秘密の値を取得したとき。書き込む前に断った要求（入力の誤り、409 等）は監査ログに残さない（アプリケーションログには残る）。
- 秘密の値の取得（`/subscribers/{imsi}/keys`、`/clients/{clientId}/secret`）も、そのたびに監査ログに残す（値は残さない）。
- 停止・再開の `action` は、応答（変更後の認可ポリシー）の状態で決める。今と同じ状態への変更は、prov は書き込まず監査ログにも残さないが、provisioner は受け付けた要求として残す（§15 の 4）。
- 中継した操作の `details` は、下流の名前（`downstream`）と、変更なら変えた項目の名前（`fields`）だけとする。中継の本文は検証しない方針で、秘密の値を含みうるため、値は残さない（変更前後の値は下流の監査ログにある）。RADIUSクライアントの作成の `target` は、下流が採番した ID。

### 10.3 アプリケーションログ

- `log/slog` の JSON を標準出力に出す（ローテーションは Docker に任せる）。要求ごとの `request completed`、下流の呼び出しの結果、補償・やり直しの経過、起動時の下流への接続の確認を出す。
- 秘密の値はログに出さない（要求・応答の本文はログに出さない）。

## 11. 設定（環境変数）

主なもの。一覧と既定値は `.env.example` と運用ガイドに書く。

| 変数 | 内容 | 既定 |
|---|---|---|
| `PROVISIONER_ADDR` | 待ち受け | `:9446` |
| `PROVISIONER_TLS_CERT` / `_TLS_KEY` / `_TLS_HOSTS` | サーバー証明書・秘密鍵（なければ生成）、生成時の SAN | `/data/tls/...`、`localhost,127.0.0.1,eapaka-provisioner` |
| `PROVISIONER_ADMIN_CLIENTS` | 管理クライアント（`識別名=フィンガープリント,...`）。空なら起動しない | — |
| `PROVISIONER_CLIENT_CERT` / `_CLIENT_KEY` | 下流 2 つに使うクライアント証明書と秘密鍵（鍵が空なら証明書のファイルから読む） | `/certs/client.pem`、（空） |
| `PROVISIONER_PROV_URL` / `_PROV_SERVER_CERT` | prov のベースURL、検証に使うサーバー証明書 | `https://provisioning-api:9444/admin/v1`、`/certs/prov-server.pem` |
| `PROVISIONER_AKA_URL` / `_AKA_SERVER_CERT` | aka のベースURL、検証に使うサーバー証明書。URL が空なら `aka` を扱わない | （空）、`/certs/aka-server.pem` |
| `PROVISIONER_PLMN_MAP` | PLMN マップ（§4） | （空。すべて `poc`） |
| `PROVISIONER_AKA_AV_CLIENT_ID` | vector-gateway の AVクライアントID（§5）。aka を使うなら必須 | — |
| `PROVISIONER_DOWNSTREAM_TIMEOUT` | 下流の 1 回の呼び出しのタイムアウト | `5s` |
| `PROVISIONER_VALKEY_ADDR` / `_VALKEY_PASSWORD` | 専用 Valkey | `valkey:6379` |
| `PROVISIONER_AUDIT_MAX` | 監査ログの件数の上限 | `10000` |
| `PROVISIONER_LOG_LEVEL` | ログの水準 | `info` |

- 下流のクライアント証明書は 1 つを 2 つの下流に登録する（本PoCの `PROVISIONING_API_ADMIN_CLIENTS` と aka-only-server の `AKA_ADMIN_CLIENTS` に同じフィンガープリントを登録する）。`eapaka-provisioner gen-client-cert -name <識別名>` で作る（BFF の `gen-client-cert` と同じ使い方。標準エラーに両方の `.env` に貼れる行を出す）。
- 導入時の確認用に `eapaka-provisioner check-downstream`（下流 2 つの `/status` と AVクライアントを確かめる）を用意する。起動時にも同じ確認をしてログに出す。接続できなくても provisioner は起動する（Valkey には接続できないと起動しない）。
- 設定の組み合わせの誤り（PLMN マップに `01` があるのに `PROVISIONER_AKA_URL` が空、`PROVISIONER_AKA_URL` があるのに `PROVISIONER_AKA_AV_CLIENT_ID` が空など）は、起動時にエラーにする。本PoCの vector-gateway を passthrough モードで動かしている場合は、PLMN マップを空にする。
- 加入者の作成は、存在確認を含めて下流を最大 5 回呼ぶ。BFF から呼ぶときは、BFF 側の呼び出しのタイムアウト（現状 10 秒）を provisioner の最大の処理時間より長くする（BFF の付け替えの作業で行う）。

## 12. 配置（Docker Compose）

| 構成 | 内容 |
|---|---|
| `compose.yaml` | provisioner と専用 Valkey。BFF 向けの共有ネットワーク（既定名 `eapaka-provisioner`、変数 `PROVISIONER_SHARED_NETWORK`）を作り、provisioner だけを参加させる。ポートの公開は `${PROVISIONER_PUBLISH:-127.0.0.1:9446}` |
| `compose.eapaka-prov.yaml` | 本PoCと同一ホストのとき重ねる。external の `eapaka-prov` に provisioner を参加させる |
| `compose.aka-av.yaml` | aka-only-server と同一ホストのとき重ねる。external の `aka-av` に provisioner を参加させる |

- 重ねるファイルは `.env` の `COMPOSE_FILE` で選ぶ（BFF と同じ）。下流ごとに同一ホスト・別ホストを選べる。
- 同一ホストの下流は先に起動しておく（共有ネットワークがないと provisioner は起動できない）。
- 別ホストの下流には、VPN のアドレスで接続する（下流のサーバー証明書の SAN にそのアドレスを入れる）。
- 専用 Valkey は共有ネットワークに参加させない。

使用中のポート: aka-only-server 8443（AV）・9443（管理）、provisioning-api 9444、aka 版 GUI 8444、BFF 8445、provisioner 9446。

## 13. 試験

- **単体テスト:** 下流を `httptest` の偽物にして、各手順での失敗（接続できない、409、400、5xx）を注入し、補償・やり直し・応答を確かめる。Valkey を使う部分は、接続先を環境変数で指定したときだけ実際の Valkey で試す。
- **契約テスト:** 下流の接続先を環境変数（`PROVISIONER_TEST_CLIENT_CERT`、`PROVISIONER_TEST_PROV_URL` / `_PROV_SERVER_CERT`、`PROVISIONER_TEST_AKA_URL` / `_AKA_SERVER_CERT`）で指定したときだけ実行する（`internal/provapi`・`internal/akaapi` の `TestIntegration*`）。テスト用の IMSI（`00101...`）・IP（`198.51.100.0/24`）・AVクライアントを作り、最後に消す。下流の監査ログ（`/audit-logs`）に、操作者・管理クライアント・トレースID が残ることも確かめる。
- **CI（GitHub Actions）:** 本PoC と aka-only-server を固定のコミット（`POC_REF` / `AKA_REF`）で checkout してバイナリでビルド・起動し、契約テストの相手にする。下流の API を変えたら、その main に入った後のコミットに更新する。依存と標準ライブラリの既知の脆弱性は、別のワークフローが push と pull request のたびと毎週 govulncheck で調べる。
- **起動しての確認:** 手元（WSL）では下流をバイナリと専用の Valkey コンテナで起動して通しで確かめる。simwifi では、同一ホスト（共有ネットワーク）と別ホスト（Tailscale のアドレス）で、BFF なしで curl から操作し、`aka` の加入者は eapaka_test で認証まで確かめる。

## 14. 将来拡張

| 項目 | 内容 | 前提・注意 |
|---|---|---|
| 一覧の改善 | 正確な `total`、属性（置き場所、`issues` 等）での検索、下流が持たない付加情報（ラベル、メモ等） | 加入者の台帳（置き場所や付加情報の記録）を provisioner の Valkey に持つ必要がある。下流が正本のままなので、Admin TUI 等での直接の変更による台帳とのずれの検出と、どちらを正とするかの規則がセットで要る |
| 時間のかかるジョブ | 置き場所の移行（PLMN 単位で `poc` と `aka` の間で鍵を移す）、大量の登録・削除 | 操作の記録（§9.3）を、進み具合を残して途中から再開できるジョブに広げる。鍵の移行では Ki / OPc を一時的に扱うため、保存の方法を別に決める |
| 整合性の定期確認 | 全加入者の `issues` を定期的に調べて保存し、一覧で返す | 一覧の改善と合わせて検討する |
| 複数ノード | 本PoC・aka-only-server を複数扱う | 加入者をどのノードに置くかの決め方（設定、全ノードへの問い合わせ、台帳）を決める |
| AVクライアントの管理 | aka の AVクライアントの登録・変更の中継 | 現状は aka 版 GUI で行う |
| 下流の条件つきの書き込み | ETag と `If-Match` で、provisioner の外からの変更との競合を検出する | 下流 2 つの API の変更が要る |
| 操作者ごとの権限 | 管理クライアントごとの読み取り専用等 | 下流と合わせて検討する |

## 15. 設計の途中で見つかった事項

いずれも 2026-10-10 に決定。

| # | 内容 | 決定 |
|---|---|---|
| 1 | aka-only-server の管理API は `X-Trace-ID` を受け取らず、監査ログにも残さない（2026-10-10 に確認）。provisioner の操作と aka の監査ログを突き合わせる手がかりが、操作者と時刻だけになる | aka-only-server の管理API に、prov と同じ作法の `X-Trace-ID`（受け取り・採番・応答で返す・ログと監査ログの `traceId`）を加える。OpenAPI・実装・テストと aka 版 GUI のクライアントもあわせて直す。provisioner の API 仕様の作成より前に行う。**実施済み（2026-10-10）**: aka-only-server 管理API 0.2.0（`2a6dcf8`）、aka 版 GUI（`b65d30a`）。認証ベクターAPI への追加は aka-only-server の今後の課題（同リポジトリの設計概要 §13.1） |
| 2 | PLMN マップで `01` に当たる aka 側の加入者を、他の AVクライアントと共有している場合の削除 | provisioner はそれらを本PoCの加入者として扱い、削除では aka 側の加入者ごと削除する（他の AVクライアントとは共有しない前提）。共有が要る場合は、削除で自分の AVクライアントID を外すだけにする案に切り替える |
| 3 | 加入者の作成・変更・削除が、同じ IMSI の未完了（retrying / failed）の操作の記録を見ていない。作成の補償が残ったまま作り直すと、後でワーカーが補償を続けて作り直した加入者を消すおそれがある（ステップ 7 で運用ガイドを書く中で見つかった） | 同じ IMSI に未完了の記録があれば、加入者の作成・変更・削除とポリシーの PUT・DELETE を 409（`OPERATION_UNRESOLVED`）で断る（§9.3）。API 仕様を 0.2.0 にした |
| 4 | 外部 OSS/BSS のサンプル実装（eapaka-ossbss-sample。同リポジトリの設計概要 §8・§15）に、加入者を登録したまま一時的に使えなくする（停止）・戻す（再開）手段が要る | 本PoCの認可ポリシーに状態 `status`（`active` / `suspended`）を加えた（Provisioning API 0.4.0。本PoC `dbd97f5`）。provisioner は `PUT /policies/{imsi}/status` を中継し（ロック、未完了の操作の確認、`Idempotency-Key`、監査ログ `policy.suspend` / `policy.resume`）、加入者の統合リソースに `status` を加える。加入者の操作としての別の口（`/subscribers/{imsi}/suspend` 等）は作らない（停止は本PoCの 1 ノードの操作で、補償の要る複数ノードの操作ではないため）。作成・変更の本文では状態を指定できない。本文の検証は prov に任せ（中継の作法）、監査ログは同じ状態への変更も残す。API 仕様を 0.3.0 にした |

## 16. 実装ステップ

各ステップの終わりに「作ったもの」と「実際に動かして確かめたこと」を報告して確認をもらう。

1. API 仕様（`docs/openapi/provisioner-api.yaml`）の作成 … 作成済み（0.1.0。2026-10-10）。§15 の 3 で 0.2.0
2. 骨組み: 設定、ログ、mTLS のサーバー（フィンガープリントの固定、サーバー証明書の生成）、`/status`、サブコマンド（`gen-client-cert`、`server-cert`、`check-downstream`）、専用 Valkey、compose … 実装済み（2026-10-10）。手元と simwifi（同一ホスト）で確認済み
3. 下流のクライアント（prov / aka。diagnose を含む）と契約テスト、CI … 実装済み（2026-10-10）。型つきの呼び出しは加入者の統合操作で使うもの（prov の加入者・認可ポリシー、aka の加入者・AVクライアント）。中継（RADIUSクライアント、セッション、監査ログ）は要求と応答をそのまま渡す `Relay` で行う
4. 中継する操作（§7）、ロック、`Idempotency-Key`、監査ログ … 実装済み（2026-10-10）。手元で下流をバイナリで起動して確認済み
5. 加入者の統合操作（§6）と操作の記録・要求の中の補償 … 実装済み（2026-10-10）。統合操作は `internal/subscriber`（ステップ 6 のワーカーも使う）、入力の検証とエラーの応答は `internal/api`。手元で下流をバイナリで起動して確認済み
6. やり直しのワーカーと `/operations` … 実装済み（2026-10-10）。手元で下流をバイナリで起動し、要求が落ちた記録を Valkey に置いて確認済み
7. 運用ガイド・README、simwifi での確認（同一ホスト・別ホスト、eapaka_test での認証） … 完了（2026-10-10）。運用ガイドは `docs/operation-guide.md`。simwifi の同一ホスト（本PoCの全体・aka-only-server・provisioner を compose で起動）で、provisioner から作った `poc` と `aka` の加入者が eapaka_test で認証できること、変更・削除・中継・監査ログ・`/operations`（要求が落ちた記録を置いて、ワーカーのやり直し・`retry`・`dismiss`）を確認した。別ホストは、手元（WSL）のバイナリの provisioner から Tailscale のアドレスで simwifi の下流に接続して同じく認証まで確かめ、simwifi の provisioner を `COMPOSE_FILE=compose.yaml` だけにした構成と、別ホストの管理クライアントからの接続も確かめた

加入者の停止・再開（§15 の 4）は、ステップ 7 の後に加えた（2026-10-10）。手元で下流をバイナリで起動して契約テストと操作を確かめ、simwifi で eapaka_test による認証（停止で Reject、再開で Accept。`poc` と `aka` の両方）を確かめた。

BFF（web-gui-for-eapaka-radius）からの接続は、BFF のリポジトリで行った（2026-10-10。BFF の設計概要 §12）。BFF は接続先を設定で選び（既定は provisioning-api に直接）、provisioner 経由のときは加入者・操作の記録・監査ログの画面が provisioner の API を使う。
