#!/bin/sh
# The `pitr` service: pgBackRest backups on a schedule, plus the operations checks that say when point-in-time
# recovery (or the nightly dump) is no longer protecting the data. No cron daemon: one loop that wakes every 30 s.
#
#   * at start: stanza-create (idempotent), `check`, and a first full backup when the repository has none;
#   * every day at PITR_BACKUP_AT: a full backup on PITR_FULL_DAY (date +%u, 7 = Sunday), a differential otherwise;
#     pgBackRest expires old backups and their WAL itself after each backup (repo1-retention-full);
#   * every PITR_CHECK_SECONDS: archiver health, WAL backlog, pg_wal size, backup age, repository disk, dump age.
#
# Problems are printed as "PITR ALERT <key>: <text>" lines, POSTed to the webhook in /run/secrets/ops/webhook-url when
# there is one (again every OPS_REALERT_HOURS while they last, and once more when they clear), and written to
# /run/aether-ops/status.json,
# which the service healthcheck reads. Runs as the postgres uid; peer authentication on the shared socket.
set -u

AT="${PITR_BACKUP_AT:-03:30}"
FULL_DAY="${PITR_FULL_DAY:-7}"
CHECK_SECONDS="${PITR_CHECK_SECONDS:-300}"
MAX_BACKUP_AGE_H="${PITR_MAX_BACKUP_AGE_HOURS:-36}"
ARCHIVE_LAG_MIN="${PITR_ARCHIVE_LAG_MINUTES:-15}"
REPO_MIN_FREE="${PITR_REPO_MIN_FREE_PERCENT:-15}"
DUMP_MAX_AGE_H="${DUMP_MAX_AGE_HOURS:-30}"
REALERT_H="${OPS_REALERT_HOURS:-6}"
WEBHOOK_FILE=/run/secrets/ops/webhook-url

OPS=/run/aether-ops
STATE=/tmp/pitr-alerts
DROP_FLAG="$OPS/wal-dropped"
BACKUP_FAILED=/tmp/pitr-backup-failed
mkdir -p "$STATE"
STARTED="$(date +%s)"

pgbr() { pgbackrest --stanza=aether "$@"; }
sql() { psql -h /var/run/postgresql -U postgres -d aether -X -A -t -q -F '|' -v ON_ERROR_STOP=1 "$@"; }
log() { echo "pitr: $*"; }

json_escape() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | tr '\n\t' '  '; }

# The URL (usually a credential) reaches curl through a config on stdin and the body through a file: neither is in
# a process argument list that `ps` on the host could show.
notify() { # severity key text
	url="$(cat "$WEBHOOK_FILE" 2>/dev/null || true)"
	[ -n "$url" ] || return 0
	text="[aether ${OPS_HOST:-$(hostname)}] $1 $2: $3"
	printf '{"source":"aether-pitr","severity":"%s","key":"%s","text":"%s","content":"%s"}' \
		"$1" "$2" "$(json_escape "$text")" "$(json_escape "$text")" > /tmp/pitr-webhook.json
	printf 'url = "%s"\n' "$url" | curl -sS -m 10 -o /dev/null --fail -K - \
		-H 'Content-Type: application/json' --data-binary @/tmp/pitr-webhook.json \
		|| echo "pitr: webhook delivery failed for $2" >&2
}

# Ready means the real server: on a new volume the image's entrypoint first runs a temporary socket-only server
# (listen_addresses='') for its init scripts, which answers pg_isready and then goes away.
wait_for_postgres() {
	until [ "$(sql -c "SELECT current_setting('listen_addresses') <> ''" 2>/dev/null)" = "t" ]; do sleep 5; done
}

# run_backup full|diff — also clears the WAL-drop flag when the backup started after the (last) drop.
run_backup() {
	type="$1"
	started="$(date +%s)"
	log "starting $type backup"
	if pgbr --log-level-console=info backup --type="$type"; then
		rm -f "$BACKUP_FAILED"
		if [ -f "$DROP_FLAG" ] && [ "$(stat -c %Y "$DROP_FLAG")" -lt "$started" ]; then
			rm -f "$DROP_FLAG"
			log "full backup after a WAL drop completed; recovery is continuous again from this backup"
		fi
		log "$type backup ok"
		return 0
	fi
	echo "$type backup failed at $(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$BACKUP_FAILED"
	echo "pitr: $type backup FAILED" >&2
	return 1
}

# pgbackrest info as "status_code|newest_stop|oldest_start|count|status_message" (epochs; 0 when none).
repo_info() {
	# Through a file and psql's \set with backticks, not -v on the command line: the JSON grows with every backup.
	pgbr --output=json info > /tmp/pitr-info.json 2>/dev/null || echo '[]' > /tmp/pitr-info.json
	[ -s /tmp/pitr-info.json ] || echo '[]' > /tmp/pitr-info.json
	sql <<'SQL'
\set info `cat /tmp/pitr-info.json`
WITH s AS (SELECT coalesce((:'info'::json)->0, '{}'::json) AS j)
SELECT coalesce(j->'status'->>'code', '99'),
       coalesce((SELECT max((b->'timestamp'->>'stop')::bigint) FROM json_array_elements(coalesce(j->'backup', '[]')) b), 0),
       coalesce((SELECT min((b->'timestamp'->>'start')::bigint) FROM json_array_elements(coalesce(j->'backup', '[]')) b), 0),
       (SELECT count(*) FROM json_array_elements(coalesce(j->'backup', '[]'))),
       replace(coalesce(j->'status'->>'message', 'no repository information'), '|', '/')
FROM s;
SQL
}

# Every check; prints "key|text" per problem and sets globals used by the status file.
collect() {
	now="$(date +%s)"
	pg="$(sql <<'SQL'
SELECT a.failed_count,
       coalesce(extract(epoch FROM a.last_failed_time)::bigint, 0),
       coalesce(extract(epoch FROM a.last_archived_time)::bigint, 0),
       coalesce(a.last_archived_wal, ''),
       (SELECT count(*) FROM pg_ls_archive_statusdir() WHERE name LIKE '%.ready'),
       coalesce((SELECT extract(epoch FROM now() - min(modification))::bigint
                 FROM pg_ls_archive_statusdir() WHERE name LIKE '%.ready'), 0),
       (SELECT coalesce(sum(size), 0) FROM pg_ls_waldir()),
       pg_size_bytes(current_setting('max_wal_size')),
       current_setting('archive_mode')
FROM pg_stat_archiver a;
SQL
)" || { echo "postgres|cannot query the database for archiver status"; return; }
	IFS='|' read -r failed_count last_failed last_archived LAST_WAL ready_count ready_age wal_bytes max_wal archive_mode <<EOF
$pg
EOF
	LAST_ARCHIVED="$last_archived"
	WAL_BYTES="$wal_bytes"
	READY_COUNT="$ready_count"
	[ "$archive_mode" = "on" ] || echo "archive_off|archive_mode is $archive_mode: no WAL is archived, point-in-time recovery is off"
	if [ "$last_failed" -gt 0 ] && [ "$last_failed" -ge "$last_archived" ]; then
		echo "archive_failing|WAL archiving is failing (failures so far: $failed_count; last success: $(fmt "$last_archived")); see the postgres log for pgbackrest errors"
	fi
	if [ "$ready_age" -gt $((ARCHIVE_LAG_MIN * 60)) ]; then
		echo "archive_lagging|$ready_count WAL segment(s) waiting to be archived, oldest for $((ready_age / 60)) min"
	fi
	if [ "$wal_bytes" -gt $((max_wal + 2147483648)) ]; then
		echo "wal_growing|pg_wal holds $((wal_bytes / 1048576)) MB (max_wal_size $((max_wal / 1048576)) MB): WAL is not being recycled, usually because archiving fails"
	fi
	if [ -f "$DROP_FLAG" ]; then
		echo "wal_dropped|archive queue exceeded, WAL was dropped ($(head -n 1 "$DROP_FLAG")): no point-in-time recovery across that gap until a new full backup completes"
	fi

	IFS='|' read -r repo_code NEWEST OLDEST BACKUPS repo_message <<EOF
$(repo_info)
EOF
	if [ "$repo_code" != "0" ]; then
		echo "repo_status|pgBackRest repository: $repo_message (code $repo_code)"
	elif [ "$NEWEST" -gt 0 ] && [ $((now - NEWEST)) -gt $((MAX_BACKUP_AGE_H * 3600)) ]; then
		echo "backup_stale|newest pgBackRest backup finished $(((now - NEWEST) / 3600)) h ago (limit ${MAX_BACKUP_AGE_H} h)"
	fi
	[ -f "$BACKUP_FAILED" ] && echo "backup_failed|$(cat "$BACKUP_FAILED")"

	df_line="$(df -P /var/lib/pgbackrest | awk 'NR == 2 { print $2, $4 }')"
	total="${df_line% *}"
	avail="${df_line#* }"
	REPO_FREE=$((avail * 100 / (total > 0 ? total : 1)))
	[ "$REPO_FREE" -lt "$REPO_MIN_FREE" ] && echo "repo_disk|backup disk has ${REPO_FREE}% free (limit ${REPO_MIN_FREE}%)"

	# The nightly dump (backup service records each outcome in $OPS). Skipped for the first half hour so a fresh
	# stack is not reported before its first dump.
	DUMP_NEWEST="$(cat "$OPS/dump-last-ok" 2>/dev/null || echo 0)"
	if [ $((now - STARTED)) -gt 1800 ] && [ $((now - DUMP_NEWEST)) -gt $((DUMP_MAX_AGE_H * 3600)) ]; then
		echo "dump_stale|newest nightly dump is older than ${DUMP_MAX_AGE_H} h (backup service)"
	fi
	[ -f "$OPS/dump-failed" ] && echo "dump_failed|$(head -n 1 "$OPS/dump-failed")"
}

fmt() { [ "${1:-0}" -gt 0 ] && date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ || echo never; }

check_and_report() {
	LAST_ARCHIVED=0 LAST_WAL="" WAL_BYTES=0 READY_COUNT=0 NEWEST=0 OLDEST=0 BACKUPS=0 REPO_FREE=0 DUMP_NEWEST=0
	collect > /tmp/pitr-problems 2>/dev/null
	now="$(date +%s)"
	alerts=""
	# New or still-active problems.
	while IFS='|' read -r key text; do
		[ -n "$key" ] || continue
		if [ ! -f "$STATE/$key" ]; then
			echo "PITR ALERT $key: $text" >&2
			notify ALERT "$key" "$text"
			echo "$now $now" > "$STATE/$key"
		else
			read -r since notified < "$STATE/$key"
			if [ $((now - notified)) -ge $((REALERT_H * 3600)) ]; then
				echo "PITR ALERT $key (still): $text" >&2
				notify ALERT "$key" "$text"
				echo "$since $now" > "$STATE/$key"
			fi
		fi
		read -r since _ < "$STATE/$key"
		alerts="$alerts${alerts:+,}{\"key\":\"$key\",\"since\":\"$(fmt "$since")\",\"message\":\"$(json_escape "$text")\"}"
	done < /tmp/pitr-problems
	# Cleared problems.
	for f in "$STATE"/*; do
		[ -f "$f" ] || continue
		key="$(basename "$f")"
		if ! grep -q "^$key|" /tmp/pitr-problems; then
			echo "PITR RESOLVED $key" >&2
			notify RESOLVED "$key" "cleared"
			rm -f "$f"
		fi
	done
	ok=true
	[ -n "$alerts" ] && ok=false
	window=null
	[ "$OLDEST" -gt 0 ] && window="\"$(fmt "$OLDEST")\""
	umask 022
	cat > "$OPS/status.json.tmp" <<EOF
{"ok":$ok,"checked_at":"$(fmt "$now")","alerts":[$alerts],
 "pitr":{"restore_from":$window,"newest_backup":"$(fmt "$NEWEST")","backups":$BACKUPS,"last_archived_at":"$(fmt "$LAST_ARCHIVED")","last_archived_wal":"$LAST_WAL","wal_waiting":$READY_COUNT,"pg_wal_mb":$((WAL_BYTES / 1048576)),"repo_free_percent":$REPO_FREE},
 "dump":{"newest":"$(fmt "$DUMP_NEWEST")"}}
EOF
	mv "$OPS/status.json.tmp" "$OPS/status.json"
	umask 077
	# A WAL drop is only healed by a new full backup; take it as soon as archiving works again.
	if [ -f "$DROP_FLAG" ] && ! grep -Eq '^(archive_failing|archive_lagging|archive_off)\|' /tmp/pitr-problems; then
		run_backup full && check_and_report
	fi
}

umask 077
log "waiting for postgres"
wait_for_postgres
if ! pgbr --log-level-console=info stanza-create; then
	echo "PITR ALERT stanza: stanza-create failed (see above); restarting in 60 s" >&2
	notify ALERT stanza "pgBackRest stanza-create failed: no backups or WAL archiving until this is fixed (see the pitr log)"
	sleep 60
	exit 1
fi
pgbr --log-level-console=info check || echo "pitr: check failed; archiving is not working yet" >&2
IFS='|' read -r _ _ _ count _ <<EOF
$(repo_info)
EOF
if [ "${count:-0}" = "0" ]; then
	run_backup full
fi
log "scheduled: daily backup at $AT ($(date +%Z)), full on day $FULL_DAY of the week, checks every ${CHECK_SECONDS}s"
check_and_report
last_check="$(date +%s)"
last_retry="$(date +%s)"
while true; do
	sleep 30
	today="$(date +%Y-%m-%d)"
	if [ "$(date +%H:%M)" = "$AT" ] && [ "$(cat /tmp/pitr-last-backup 2>/dev/null || true)" != "$today" ]; then
		echo "$today" > /tmp/pitr-last-backup
		if [ "$(date +%u)" = "$FULL_DAY" ]; then run_backup full; else run_backup diff; fi
		last_check=0
	fi
	if [ $(($(date +%s) - last_check)) -ge "$CHECK_SECONDS" ]; then
		check_and_report
		last_check="$(date +%s)"
		# No backup at all (the first one failed): nothing can be restored, so retry every 15 minutes.
		if [ "$BACKUPS" = "0" ] && [ $((last_check - last_retry)) -ge 900 ]; then
			last_retry="$last_check"
			run_backup full
			last_check=0
		fi
	fi
done
