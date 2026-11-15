#!/bin/sh
# Valkey のスナップショットを、止めずに取って手元に写す（運用ガイド 8 章。aka-vecgen・aka-radius と同じ作り）。
# compose.yaml のあるディレクトリで実行する。
#
#   scripts/backup-valkey.sh backups/valkey-$(date +%Y%m%d).rdb
set -eu

out=${1:?usage: scripts/backup-valkey.sh <出力のファイル>}

# 動いている valkey がなければ何もしない（compose のプロジェクトを取り違えて、別のボリュームを触らないため）。
# 別のプロジェクト名や重ね合わせで起動しているときは、COMPOSE_PROJECT_NAME と COMPOSE_FILE を付けて動かす
if [ -z "$(docker compose ps -q valkey 2> /dev/null)" ]; then
	echo "backup-valkey: この compose のプロジェクトで動いている valkey が見つからない（COMPOSE_PROJECT_NAME・COMPOSE_FILE を確かめる）" >&2
	exit 1
fi

cli() {
	docker compose exec -T valkey sh -c 'valkey-cli -a "$VALKEY_PASSWORD" --no-auth-warning "$@"' valkey-cli "$@"
}
# 起動してからスナップショットを取り始めた回数。Valkey は BGSAVE を始めたとき（fork のとき）にこの数を増やすので、増えただけでは
# まだ書いている途中で、dump.rdb は前のスナップショットのまま。数が増え、かつ書いている途中でなくなるまで待つ（aka-mvpset の段階 6 の B6-1）
saves() {
	cli INFO persistence | sed -n 's/^rdb_saves:\([0-9]*\).*/\1/p'
}
saved() {
	p=$(cli INFO persistence)
	[ "$(printf '%s\n' "$p" | sed -n 's/^rdb_saves:\([0-9]*\).*/\1/p')" != "$before" ] &&
		printf '%s\n' "$p" | grep -q '^rdb_bgsave_in_progress:0'
}

before=$(saves)
# 自動のスナップショットや AOF の書き直しの途中なら、終わってから取る
cli BGSAVE SCHEDULE > /dev/null
i=0
until saved; do
	i=$((i + 1))
	if [ "$i" -gt 600 ]; then
		echo "backup-valkey: スナップショットが 10 分たっても終わらない" >&2
		exit 1
	fi
	sleep 1
done
if ! cli INFO persistence | grep -q '^rdb_last_bgsave_status:ok'; then
	echo "backup-valkey: スナップショットに失敗した（docker compose logs valkey を見る）" >&2
	exit 1
fi

docker compose cp valkey:/data/dump.rdb "$out"
echo "backup-valkey: $out"
