#!/bin/sh
# Scheduler for the nightly dump. No cron daemon and no extra binary: the container wakes every
# 30 seconds, and runs exactly once on the first minute of each day that matches BACKUP_AT.
# It also takes a dump whenever the newest one is older than a day (new install, server that was off at BACKUP_AT,
# a failed run), checked at start and hourly, so the healthcheck and the pitr checks see something recent.
# Each outcome is recorded in /run/aether-ops (dump-last-ok: epoch of the newest good dump; dump-failed: the last
# failure, removed by the next good run), which the pitr service reports on.
set -eu
AT="${BACKUP_AT:-02:30}"
MARKER=/tmp/last-backup-date
echo "backup scheduler started; nightly dump at $AT ($(date +%Z))"

OPS=/run/aether-ops

run() {
	if /bin/sh /opt/aether/backup.sh; then
		date +%s > "$OPS/dump-last-ok" || true
		rm -f "$OPS/dump-failed"
	else
		echo "nightly backup failed" >&2
		echo "pg_dump backup failed at $(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$OPS/dump-failed" || true
	fi
}

# pg_isready over TCP: on a new volume the entrypoint's temporary init server is socket-only, and the healthcheck
# (socket) can pass while it runs.
until pg_isready -q; do sleep 5; done

# Newest dump younger than a day? If not (new install, server off at BACKUP_AT, failed run), take one now; checked
# at start and then hourly, so a failed dump is retried.
catch_up() {
	recent="$(find /backups/daily -name 'aether-*.dump' -mmin -1440 2>/dev/null | head -n 1)"
	if [ -z "$recent" ]; then
		echo "no dump from the last 24 h: taking one now"
		run
	elif [ ! -f "$OPS/dump-last-ok" ]; then
		stat -c %Y "$recent" > "$OPS/dump-last-ok" || true
	fi
}
catch_up
hour="$(date +%H)"
while true; do
	today="$(date +%Y-%m-%d)"
	if [ "$(date +%H:%M)" = "$AT" ] && [ "$(cat "$MARKER" 2>/dev/null || true)" != "$today" ]; then
		echo "$today" > "$MARKER"
		run
	fi
	if [ "$(date +%H)" != "$hour" ]; then
		hour="$(date +%H)"
		catch_up
	fi
	sleep 30
done
