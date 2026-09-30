#!/bin/sh
# archive_command for the production cluster:  aether-pgbackrest-push %p
# (pgBackRest refuses to back up unless archive_command contains "pgbackrest", hence the name.)
#
# Runs pgbackrest archive-push and passes its output on to the server log. With archive-push-queue-max set, pgBackRest
# reports success but DROPS the segment when the queue is full (it keeps the database up instead of filling pg_wal):
# that is a hole in point-in-time recovery until the next full backup. The drop is recorded in a flag file that the
# pitr service watches; it raises an alert and takes a new full backup as soon as archiving works again.
out="$(pgbackrest --stanza=aether archive-push "$1" 2>&1)"
status=$?
[ -n "$out" ] && printf '%s\n' "$out" >&2
case "$out" in
	*"dropped WAL file"*)
		{ date -u +%Y-%m-%dT%H:%M:%SZ; printf '%s\n' "$out" | grep 'dropped WAL file' | head -n 1; } \
			> /run/aether-ops/wal-dropped.tmp 2>/dev/null \
			&& mv /run/aether-ops/wal-dropped.tmp /run/aether-ops/wal-dropped 2>/dev/null
		;;
esac
exit "$status"
