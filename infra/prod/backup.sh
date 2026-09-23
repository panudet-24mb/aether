#!/bin/sh
# One backup run. Executed inside the `backup` container, which already carries PG* connection
# settings (TLS, verify-full) in its environment. Prints file names and sizes, never credentials.
set -eu
umask 077

DIR=/backups
KEEP_DAILY="${BACKUP_KEEP_DAILY:-14}"
KEEP_WEEKLY="${BACKUP_KEEP_WEEKLY:-8}"

mkdir -p "$DIR/daily" "$DIR/weekly"
stamp="$(date +%Y%m%d-%H%M%S)"
file="$DIR/daily/aether-$stamp.dump"

# --format=custom is what restore.sh and pg_restore --list expect; compression is built in.
pg_dump --format=custom --compress=6 --file="$file.part"
mv "$file.part" "$file"

# Integrity check: a dump whose table of contents cannot be read is not a backup.
if ! pg_restore --list "$file" > /dev/null; then
	echo "backup FAILED integrity check: $file" >&2
	mv "$file" "$file.corrupt"
	exit 1
fi

# One weekly snapshot, hard-linked so it costs no extra disk until the daily copy is pruned.
if [ "$(date +%u)" = "7" ]; then
	ln -f "$file" "$DIR/weekly/$(basename "$file")" 2>/dev/null || cp "$file" "$DIR/weekly/"
fi

prune() {
	target="$1"
	keep="$2"
	# shellcheck disable=SC2012
	ls -1t "$target"/aether-*.dump 2>/dev/null | tail -n "+$((keep + 1))" | while read -r old; do
		rm -f "$old"
		echo "pruned $(basename "$old")"
	done
}
prune "$DIR/daily" "$KEEP_DAILY"
prune "$DIR/weekly" "$KEEP_WEEKLY"

echo "backup ok $(basename "$file") $(wc -c < "$file") bytes, verified with pg_restore --list"
