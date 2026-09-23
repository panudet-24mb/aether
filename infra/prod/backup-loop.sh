#!/bin/sh
# Scheduler for the nightly dump. No cron daemon and no extra binary: the container wakes every
# 30 seconds, and runs exactly once on the first minute of each day that matches BACKUP_AT.
set -eu
AT="${BACKUP_AT:-02:30}"
MARKER=/tmp/last-backup-date
echo "backup scheduler started; nightly dump at $AT ($(date +%Z))"
while true; do
	today="$(date +%Y-%m-%d)"
	if [ "$(date +%H:%M)" = "$AT" ] && [ "$(cat "$MARKER" 2>/dev/null || true)" != "$today" ]; then
		echo "$today" > "$MARKER"
		/bin/sh /opt/aether/backup.sh || echo "nightly backup failed" >&2
	fi
	sleep 30
done
