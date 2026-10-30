#!/bin/sh
# backup-valkey.sh で取ったスナップショットから Valkey を戻す（運用ガイド 8 章）。今のデータは消える。
# compose.yaml のあるディレクトリで実行する。終わったら docker compose up -d で起動する。
#
#   scripts/restore-valkey.sh backups/valkey-20261031.rdb
#
# Valkey は AOF が有効だと、AOF がないときにスナップショットを読まずに空で始まる。
# そこで、AOF を切った一時の Valkey でスナップショットを読み込み、CONFIG SET appendonly yes で
# 読み込んだ中身から AOF を作り直してから止める（aka-vecgen の scripts/restore-valkey.sh と同じ）。
set -eu

in=${1:?usage: scripts/restore-valkey.sh <スナップショットのファイル>}
if [ ! -f "$in" ]; then
	echo "restore-valkey: $in がない" >&2
	exit 1
fi
src=$(cd "$(dirname "$in")" && pwd)/$(basename "$in")

# 動いている valkey がなければ何もしない（compose のプロジェクトを取り違えて、別のボリュームを触らないため）。
# 別のプロジェクト名や重ね合わせで起動しているときは、COMPOSE_PROJECT_NAME と COMPOSE_FILE を付けて動かす
if [ -z "$(docker compose ps -q valkey 2> /dev/null)" ]; then
	echo "restore-valkey: この compose のプロジェクトで動いている valkey が見つからない（COMPOSE_PROJECT_NAME・COMPOSE_FILE を確かめる）" >&2
	exit 1
fi

docker compose stop aka-provisioner valkey

# compose の valkey のサービス（ボリューム valkey-data を /data に付けたもの）で、一時の Valkey を動かす。
# 一時の Valkey はパスワードなしでコンテナの中だけで待ち受ける
docker compose run --rm --no-deps -T -v "$src":/restore/dump.rdb:ro valkey sh -euc '
	rm -rf /data/* /data/.[!.]*
	cp /restore/dump.rdb /data/dump.rdb
	valkey-server --dir /data --appendonly no --daemonize yes --save "" --bind 127.0.0.1
	until valkey-cli PING 2> /dev/null | grep -q PONG; do sleep 1; done
	valkey-cli CONFIG SET appendonly yes > /dev/null
	until valkey-cli INFO persistence | grep -q "^aof_rewrite_in_progress:0" &&
		valkey-cli INFO persistence | grep -q "^aof_rewrite_scheduled:0"; do sleep 1; done
	valkey-cli INFO persistence | grep -q "^aof_last_bgrewrite_status:ok"
	echo "restore-valkey: keys=$(valkey-cli DBSIZE)"
	valkey-cli SHUTDOWN NOSAVE > /dev/null 2>&1 || true
	while valkey-cli PING > /dev/null 2>&1; do sleep 1; done
	chown -R valkey:valkey /data
'
echo "restore-valkey: 戻した。docker compose up -d で起動する"
