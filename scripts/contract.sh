#!/usr/bin/env bash
# 契約の試験（internal/contract）のために、本物の下流（aka-vecgen・aka-radius の管理API、radsec-ca、エッジ）と aka-provisioner を
# ループバックで起動する（設計概要 §13。段階 3-1 の U4、段階 4-2 の P12）。CI の契約の試験のジョブと、手元の確かめで使う。
#
#   scripts/contract.sh up   <バイナリのディレクトリ> <作業ディレクトリ>
#   scripts/contract.sh down <作業ディレクトリ>
#
# バイナリのディレクトリには aka-vecgen・aka-radius・aka-provisioner を置く（下流は aka-mvpset の versions.env の
# 固定のコミットからビルドしたもの）。Valkey は 3 つとも Docker のコンテナで立てる（127.0.0.1 の 16380〜16382）。
# up が終わると、作業ディレクトリの contract.env に試験の環境変数を書く（go test の前に読み込む）。
# 使う口: aka-vecgen の管理API 19443・認証ベクターAPI 18443、aka-radius の管理API 19444、aka-provisioner 19446。
#
# 環境変数 CA_SRC・EDGE_SRC に radsec-ca・radsec-edge のソース（固定のコミット）を与えると、この 2 つもイメージをビルドして compose のコンテナで起動し、
# 登録簿に足して SIGHUP で読み直す（radsec-ca は aka-provisioner の後に起動する。P2）。登録簿には届かない 2 台目のエッジも置く。
# 使う口: radsec-ca の管理API 19448・配布の口 18449、エッジの管理API 19447、テナントの口 20993。
# compose のプロジェクトは contract-ca・contract-edge、テナントのネットワーク radsec-edge-tenants の名前は固定。
set -euo pipefail

valkey_password=contract
containers=(contract-valkey-vecgen contract-valkey-radius contract-valkey-prov)
shared_network=contract-shared
radsecproxy_image=radsec-edge/radsecproxy:1.11.4-pc1

# own <UID:GID> <パス>... はコンテナの UID に読み書きさせる。sudo があれば手順どおり chown し、なければ権限を広げる（手元の確かめ）。
own() {
  local owner=$1
  shift
  if sudo -n true 2>/dev/null; then
    sudo chown -R "$owner" "$@"
  else
    chmod -R a+rwX "$@"
  fi
}

ca_compose() {
  docker compose -p contract-ca --project-directory "$work/ca" --env-file "$work/ca/.env" \
    -f "$CA_SRC/compose.yaml" -f "$CA_SRC/compose.managed.yaml" -f "$work/ca/contract.yaml" "$@"
}

edge_compose() {
  docker compose -p contract-edge --project-directory "$work/edge" --env-file "$work/edge/.env" \
    -f "$EDGE_SRC/compose.yaml" -f "$EDGE_SRC/compose.managed.yaml" -f "$work/edge/contract.yaml" "$@"
}

# work は up の作業ディレクトリ（ca_compose・edge_compose が使う）。
work=

# radsec_up は radsec-ca とエッジを起動し、登録簿に足して aka-provisioner に読み直させる。
radsec_up() {
  local bin=$1 prov_pin=$2
  docker build -q --build-arg VERSION=contract -t radsec-ca:contract "$CA_SRC" > /dev/null
  docker build -q --build-arg VERSION=contract -t radsec-edge:contract "$EDGE_SRC" > /dev/null
  docker image inspect "$radsecproxy_image" > /dev/null 2>&1 || docker build -q -t "$radsecproxy_image" "$EDGE_SRC/images/radsecproxy" > /dev/null
  docker network create "$shared_network" > /dev/null

  # エッジ。テナントのネットワークを作るので先に起動する（radsec-ca の配布の口にこのネットワークで届く）。
  mkdir -p edge/host/data edge/host/tenants edge/host/audit edge/secrets
  echo "$prov_pin" > edge/secrets/admin-clients
  : > edge/secrets/tunnel-token
  own 10084:10083 edge/host/data edge/host/tenants edge/host/audit edge/secrets/admin-clients
  cat > edge/.env <<ENV
EDGE_VERSION=contract
EDGE_HOST_DIR=$work/edge/host
EDGE_DOCKER_GID=$(stat -c %g /var/run/docker.sock)
EDGE_URN_PREFIX=example
EDGE_DNS_DOMAIN=radsec.example.net
EDGE_UPSTREAM_SECRET=contract-secret-123
EDGE_BIND_ADDR=127.0.0.1
EDGE_TLS_HOSTS=127.0.0.1
EDGE_AGENT_PUBLISH_ADDR=127.0.0.1
EDGE_CRL_BASE_URL=http://radsec-ca:8449
EDGE_ACCESS_TEAM_DOMAIN=contract.cloudflareaccess.com
EDGE_ACCESS_AUD=contract-aud
ENV
  cat > edge/contract.yaml <<'YAML'
services:
  radsec-edge:
    ports: !override
      - "127.0.0.1:19447:9447"
YAML
  edge_compose up -d --wait --no-build radsec-edge radsec-edge-agent

  # radsec-ca。管理API は試験のためだけにループバックに公開する（compose.managed.yaml は公開しない）。
  mkdir -p ca/secrets ca/audit
  openssl rand -hex 32 > ca/secrets/kek
  echo "$prov_pin" > ca/secrets/admin-clients
  own 65532:65532 ca/secrets ca/audit
  cat > ca/.env <<ENV
CA_VERSION=contract
CA_ORG=Contract Org
CA_URN_PREFIX=example
CA_DNS_DOMAIN=radsec.example.net
CA_BIND_ADDR=127.0.0.1
CA_TLS_HOSTS=127.0.0.1
CA_ACCESS_TEAM_DOMAIN=contract.cloudflareaccess.com
CA_ACCESS_AUD=contract-aud
CA_INITIAL_ADMIN_EMAIL=admin@example.com
CA_MERAKI_API_BASE=https://127.0.0.1:1/api/v1
PROVISIONER_SHARED_NETWORK=$shared_network
ENV
  cat > ca/contract.yaml <<'YAML'
services:
  radsec-ca:
    ports: !override
      - "127.0.0.1:19448:9448"
      - "127.0.0.1:18449:8449"
    networks:
      - tenants

networks:
  tenants:
    external: true
    name: radsec-edge-tenants
YAML
  ca_compose up -d --wait --no-build radsec-ca

  # 登録簿に足して読み直させる。サーバー証明書は起動してから取り出す（radsec-ca は serverCert、エッジは serverFingerprint）。
  ca_compose exec -T radsec-ca /radsec-ca server-cert > downstreams/ca-admin.pem 2> /dev/null
  edge_compose exec -T radsec-edge /radsec-edge server-cert > edge/server.pem 2> /dev/null
  local edge_fp
  edge_fp=$(openssl x509 -in edge/server.pem -noout -fingerprint -sha256 | cut -d= -f2)
  cat > downstreams/downstreams.json <<JSON
{"clientCert":"client.pem","clientKey":"client.key",
 "vecgen":{"url":"https://127.0.0.1:19443/admin/v2","serverCert":"vecgen-admin.pem"},
 "radius":{"url":"https://127.0.0.1:19444/admin/v2","serverCert":"radius-admin.pem"},
 "ca":{"url":"https://127.0.0.1:19448","serverCert":"ca-admin.pem"},
 "edges":[{"id":"edge-01","url":"https://127.0.0.1:19447","serverFingerprint":"$edge_fp"},
          {"id":"edge-02","url":"https://127.0.0.1:1","serverFingerprint":"$(printf 'ab%.0s' $(seq 1 32))"}]}
JSON
  kill -HUP "$(cat aka-provisioner.pid)"
  # check-downstream は届かない 2 台目のエッジで失敗し、原因の見当を出す。ほかの下流にはつながる。
  (
    # shellcheck source=/dev/null
    . ./provisioner.env
    if "$bin/aka-provisioner" check-downstream > check-radsec.txt 2>&1; then
      echo "届かないエッジがあるのに check-downstream が成功しました" >&2
      exit 1
    fi
    cat check-radsec.txt
    test "$(grep -c '接続できました' check-radsec.txt)" = 4
    grep -A1 '^edge:edge-02:' check-radsec.txt | grep -q '接続できませんでした'
    grep -qE 'edge:edge-02 (に接続できません|から時間内に応答がありません)' check-radsec.txt
  )
  cat >> contract.env <<ENV
export PROVISIONER_TEST_EDGE_ID=edge-01
export PROVISIONER_TEST_EDGE_DOWN_ID=edge-02
export PROVISIONER_TEST_WORK=$work
export PROVISIONER_TEST_CA_AUDIT_DIR=$work/ca/audit
export PROVISIONER_TEST_EDGE_AUDIT_DIR=$work/edge/host/audit
ENV
}

radsec_down() {
  local work=$1
  if [ ! -d "$work/ca" ] && [ ! -d "$work/edge" ]; then
    return
  fi
  # テナントのコンテナは agent が作るので compose では消えない（試験のテナントの名前だけを消す）。
  docker rm -f radsecproxy-acme > /dev/null 2>&1 || true
  docker compose -p contract-ca down -v --remove-orphans > /dev/null 2>&1 || true
  docker compose -p contract-edge down -v --remove-orphans > /dev/null 2>&1 || true
  docker network rm "$shared_network" radsec-edge-tenants > /dev/null 2>&1 || true
  # UID の違うコンテナが作ったファイルはコンテナの中から消す。
  docker run --rm -v "$work:/w" busybox:1.37.0 rm -rf /w/ca /w/edge > /dev/null 2>&1 || true
}

up() {
  local bin
  bin=$(cd "$1" && pwd)
  mkdir -p "$2"
  work=$(cd "$2" && pwd)
  cd "$work"

  local port=16380
  for c in "${containers[@]}"; do
    docker rm -f "$c" >/dev/null 2>&1 || true
    docker run -d --rm --name "$c" -p "127.0.0.1:$port:6379" valkey/valkey:9.1.2 \
      valkey-server --requirepass "$valkey_password" >/dev/null
    port=$((port + 1))
  done
  for c in "${containers[@]}"; do
    for _ in $(seq 1 30); do
      docker exec "$c" valkey-cli -a "$valkey_password" --no-auth-warning ping 2>/dev/null | grep -q PONG && break
      sleep 1
    done
  done

  # aka-provisioner のクライアント証明書（全下流に登録する）と、管理クライアント（aka-ossbss の代わり）の証明書。
  mkdir -p downstreams prov-tls prov-audit vecgen-audit radius-audit radius-sources radius-admin
  "$bin/aka-provisioner" gen-client-cert -name provisioner-ci -out-cert downstreams/client.pem -out-key downstreams/client.key 2> gen.txt
  local prov_pin
  prov_pin=$(grep -E '^provisioner-ci=[0-9a-f]{64}$' gen.txt)
  "$bin/aka-provisioner" gen-client-cert -name ossbss-ci > ossbss.pem 2> ossbss.txt
  grep -E '^ossbss-ci=[0-9a-f]{64}$' ossbss.txt > prov-admin-clients
  echo "$prov_pin" > vecgen-admin-clients
  echo "$prov_pin" > radius-admin-clients

  # aka-vecgen。aka-radius の取得先のクライアント証明書を AV クライアントとして登録してから起動する。
  export VECGEN_VALKEY_ADDR=127.0.0.1:16380 VECGEN_VALKEY_PASSWORD=$valkey_password \
    VECGEN_KEK_FILE=$work/kek VECGEN_AUDIT_DIR=$work/vecgen-audit \
    VECGEN_ADMIN_ADDR=127.0.0.1:19443 VECGEN_AV_TLS_ADDR=127.0.0.1:18443 \
    VECGEN_ADMIN_TLS_HOSTS=localhost,127.0.0.1 VECGEN_AV_TLS_HOSTS=localhost,127.0.0.1 \
    VECGEN_ADMIN_CLIENTS_FILE=$work/vecgen-admin-clients
  openssl rand -hex 32 > kek
  chmod 600 kek
  "$bin/aka-vecgen" client gen-cert -name radius-ci > radius-sources/client.pem
  "$bin/aka-vecgen" client add -name radius-ci -cert radius-sources/client.pem > vecgen-client.txt
  nohup "$bin/aka-vecgen" serve > aka-vecgen.log 2>&1 < /dev/null &
  echo $! > aka-vecgen.pid
  for _ in $(seq 1 30); do
    if "$bin/aka-vecgen" admin-cert > downstreams/vecgen-admin.pem 2>/dev/null && grep -q 'BEGIN CERTIFICATE' downstreams/vecgen-admin.pem &&
      "$bin/aka-vecgen" av-cert > radius-sources/vecgen-av.pem 2>/dev/null && grep -q 'BEGIN CERTIFICATE' radius-sources/vecgen-av.pem; then
      break
    fi
    sleep 1
  done
  unset VECGEN_VALKEY_ADDR VECGEN_VALKEY_PASSWORD VECGEN_KEK_FILE VECGEN_AUDIT_DIR VECGEN_ADMIN_ADDR VECGEN_AV_TLS_ADDR \
    VECGEN_ADMIN_TLS_HOSTS VECGEN_AV_TLS_HOSTS VECGEN_ADMIN_CLIENTS_FILE

  # aka-radius の admin。取得先の登録簿は aka-vecgen を指す 1 つ（admin は ID と証明書の期限だけを読む）。
  cat > radius-sources/sources.json <<'JSON'
{"sources":[{"id":"vecgen","name":"aka-vecgen","baseUrl":"https://127.0.0.1:18443/nudm-ueau/v1",
  "clientCertFile":"client.pem","clientKeyFile":"client.pem","serverCaFile":"vecgen-av.pem","timeout":"3s"}]}
JSON
  export RADIUS_VALKEY_HOST=127.0.0.1 RADIUS_VALKEY_PORT=16381 RADIUS_VALKEY_PASSWORD=$valkey_password \
    RADIUS_AUDIT_DIR=$work/radius-audit RADIUS_ADMIN_LISTEN_ADDR=127.0.0.1:19444 \
    RADIUS_ADMIN_TLS_CERT=$work/radius-admin/server.pem RADIUS_ADMIN_TLS_KEY=$work/radius-admin/server.key \
    RADIUS_ADMIN_CLIENTS_FILE=$work/radius-admin-clients RADIUS_SOURCES_FILE=$work/radius-sources/sources.json \
    RADIUS_HEALTH_ADDR=127.0.0.1:19091
  "$bin/aka-radius" admin-cert create -hosts localhost,127.0.0.1 -out radius-admin > /dev/null
  cp radius-admin/server.pem downstreams/radius-admin.pem
  nohup "$bin/aka-radius" admin > aka-radius.log 2>&1 < /dev/null &
  echo $! > aka-radius.pid
  unset RADIUS_VALKEY_HOST RADIUS_VALKEY_PORT RADIUS_VALKEY_PASSWORD RADIUS_AUDIT_DIR RADIUS_ADMIN_LISTEN_ADDR \
    RADIUS_ADMIN_TLS_CERT RADIUS_ADMIN_TLS_KEY RADIUS_ADMIN_CLIENTS_FILE RADIUS_SOURCES_FILE RADIUS_HEALTH_ADDR

  # aka-provisioner。
  cat > downstreams/downstreams.json <<'JSON'
{"clientCert":"client.pem","clientKey":"client.key",
 "vecgen":{"url":"https://127.0.0.1:19443/admin/v2","serverCert":"vecgen-admin.pem"},
 "radius":{"url":"https://127.0.0.1:19444/admin/v2","serverCert":"radius-admin.pem"}}
JSON
  cat > provisioner.env <<ENV
export PROVISIONER_ADDR=127.0.0.1:19446
export PROVISIONER_TLS_CERT=$work/prov-tls/cert.pem
export PROVISIONER_TLS_KEY=$work/prov-tls/key.pem
export PROVISIONER_TLS_HOSTS=localhost,127.0.0.1
export PROVISIONER_ADMIN_CLIENTS_FILE=$work/prov-admin-clients
export PROVISIONER_DOWNSTREAMS_FILE=$work/downstreams/downstreams.json
export PROVISIONER_VALKEY_ADDR=127.0.0.1:16382
export PROVISIONER_VALKEY_PASSWORD=$valkey_password
export PROVISIONER_AUDIT_DIR=$work/prov-audit
export PROVISIONER_HEALTH_ADDR=127.0.0.1:19092
ENV
  (
    # shellcheck source=/dev/null
    . ./provisioner.env
    for _ in $(seq 1 30); do
      "$bin/aka-provisioner" check-downstream > check-downstream.txt 2>&1 && break
      sleep 1
    done
    cat check-downstream.txt
    nohup "$bin/aka-provisioner" serve > aka-provisioner.log 2>&1 < /dev/null &
    echo $! > aka-provisioner.pid
    for _ in $(seq 1 30); do
      "$bin/aka-provisioner" healthcheck 2>/dev/null && break
      sleep 1
    done
    "$bin/aka-provisioner" healthcheck
  )

  local av_client_id
  av_client_id=$(grep -oE '[0-9]+' vecgen-client.txt | head -1)
  cat > contract.env <<ENV
export PROVISIONER_TEST_URL=https://127.0.0.1:19446/admin/v2
export PROVISIONER_TEST_SERVER_CERT=$work/prov-tls/cert.pem
export PROVISIONER_TEST_CLIENT_CERT=$work/ossbss.pem
export PROVISIONER_TEST_AV_CLIENT_ID=${av_client_id:-1}
export PROVISIONER_TEST_SOURCE_ID=vecgen
ENV
  if [ -n "${CA_SRC:-}" ] && [ -n "${EDGE_SRC:-}" ]; then
    radsec_up "$bin" "$prov_pin"
  fi
  echo "起動しました。試験の環境変数: $work/contract.env"
}

down() {
  local work=$1
  radsec_down "$work"
  for p in aka-provisioner aka-radius aka-vecgen; do
    if [ -f "$work/$p.pid" ]; then
      kill "$(cat "$work/$p.pid")" 2>/dev/null || true
      rm -f "$work/$p.pid"
    fi
  done
  for c in "${containers[@]}"; do
    docker rm -f "$c" >/dev/null 2>&1 || true
  done
}

case "${1:-}" in
  up) up "$2" "$3" ;;
  down) down "$2" ;;
  *) echo "usage: $0 up <バイナリのディレクトリ> <作業ディレクトリ> | down <作業ディレクトリ>" >&2; exit 2 ;;
esac
