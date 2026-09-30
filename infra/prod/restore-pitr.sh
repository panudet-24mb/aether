#!/bin/sh
# Point-in-time restore from the pgBackRest repository into a NEW Docker volume. The running database is not touched
# until you ask for the cutover.
#
#   infra/prod/restore-pitr.sh --target "2026-09-30 14:05:00+07" [--cutover] [--keep-running] [--timeline N]
#   infra/prod/restore-pitr.sh --use-volume NAME        # switch the database to an existing volume (cutover/rollback)
#   infra/prod/restore-pitr.sh --drill [--minutes N]    # rehearsal, see pitr-drill.sh
#   common: [--env-file FILE] [--project NAME]
#
# Steps: checks the target and the repository (a backup that ended before the target, WAL archived up to now, disk
# room) -> creates <project>_postgres-data-r<UTC stamp> -> pgbackrest restore --type=time --target-action=promote ->
# starts the copy in an isolated container (no network, archive_mode=off, so it never writes to the repository) and
# waits for it to promote -> prints what it holds next to the live database.
#
# --cutover then: takes a pg_dump of the current database (forensics), stops the stack, points AETHER_PG_VOLUME in
# the env file at the restored volume, starts the stack and takes a full backup (the restore starts a new timeline).
# The previous volume is left untouched: `--use-volume <previous>` goes back to it. Without --cutover the restored
# volume stays and the same --use-volume command switches to it later.
#
# Timeline: the history the live server is on now (after a cutover and a rollback the repository holds branches that
# were abandoned); without a live server, the timeline of the newest backup before the target. --timeline overrides.
# The live database is optional: with it down (disk failure, corrupt cluster) the restore reads only the repository.
#
# --drill restores to now - N minutes (default 10), compares rows of DRILL_TABLE (default core.sensor_samples) stamped
# between target - 1 h and target - 1 min in both copies, appends PASS/FAIL to <backup-dir>/drills.log and removes the
# restored volume. Exit status 0 only on PASS.
# shellcheck source=_common.sh
. "$(cd "$(dirname "$0")" && pwd)/_common.sh"

TARGET=""
CUTOVER=false
KEEP=false
DRILL=false
MINUTES=10
USE_VOLUME=""
TIMELINE=""
DRILL_LOGGED=""
while [ $# -gt 0 ]; do
	case "$1" in
		--target) TARGET="$2"; shift 2 ;;
		--cutover) CUTOVER=true; shift ;;
		--keep-running) KEEP=true; shift ;;
		--drill) DRILL=true; shift ;;
		--minutes) MINUTES="$2"; shift 2 ;;
		--use-volume) USE_VOLUME="$2"; shift 2 ;;
		--timeline) TIMELINE="$2"; shift 2 ;;
		--env-file) ENV_FILE="$2"; shift 2 ;;
		--project|-p) PROJECT="$2"; shift 2 ;;
		-h|--help) sed -n '2,26p' "$0"; exit 0 ;;
		*) echo "unknown argument: $1 (see --help)" >&2; exit 1 ;;
	esac
done
require_env
case "$MINUTES" in *[!0-9]*|"") echo "--minutes must be a number" >&2; exit 1 ;; esac

IMAGE="$(env_get AETHER_POSTGRES_IMAGE)"
PITR_DIR="$(env_get AETHER_PITR_DIR)"
SECRETS="$(env_get AETHER_SECRETS_DIR)"
BACKUP_DIR="$(env_get AETHER_BACKUP_DIR)"
CURRENT_VOLUME="$(env_get AETHER_PG_VOLUME)"
CONF="$SECRETS/pgbackrest/pgbackrest.conf"
DRILL_TABLE="${DRILL_TABLE:-core.sensor_samples}"
DRILL_COLUMN="${DRILL_COLUMN:-received_at}"
# Interpolated into SQL: plain schema.table and column identifiers only.
printf '%s' "$DRILL_TABLE" | grep -Eqx '[a-z_][a-z0-9_]{0,62}\.[a-z_][a-z0-9_]{0,62}' \
	|| { echo "DRILL_TABLE must be schema.table (lowercase identifiers)" >&2; exit 1; }
printf '%s' "$DRILL_COLUMN" | grep -Eqx '[a-z_][a-z0-9_]{0,62}' \
	|| { echo "DRILL_COLUMN must be a lowercase column name" >&2; exit 1; }
VERIFY="$PROJECT-pitr-verify"
for v in IMAGE PITR_DIR SECRETS CURRENT_VOLUME; do
	eval "[ -n \"\$$v\" ]" || { echo "$v missing from $ENV_FILE: re-run infra/prod/setup.py" >&2; exit 1; }
done

say() { echo "restore-pitr: $*"; }
DIE_REASON=""
die() { echo "restore-pitr: $*" >&2; DIE_REASON="$*"; exit 1; }

# Query the live database through the pitr service (peer auth on the shared socket). Variables: -v name=value.
live() { dc exec -T pitr psql -h /var/run/postgresql -U postgres -d aether -X -A -t -q -v ON_ERROR_STOP=1 "$@"; }
# Query the restored copy.
restored() { docker exec -i -u postgres "$VERIFY" psql -d aether -X -A -t -q -v ON_ERROR_STOP=1 "$@"; }

set_env() { # NAME VALUE — rewrite one line of the env file in place, keeping its mode
	tmp="$ENV_FILE.tmp.$$"
	(umask 077; awk -v k="$1" -v v="$2" 'BEGIN { done = 0 }
		index($0, k "=") == 1 { print k "=" v; done = 1; next } { print }
		END { if (!done) print k "=" v }' "$ENV_FILE" > "$tmp")
	mv "$tmp" "$ENV_FILE"
}

wait_healthy() { # service
	deadline=$(($(date +%s) + ${WAIT_HEALTHY_SECONDS:-600}))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		id="$(dc ps -q "$1")"
		[ -n "$id" ] && [ "$(docker inspect -f '{{.State.Health.Status}}' "$id" 2>/dev/null)" = "healthy" ] && return 0
		sleep 5
	done
	return 1
}

# Start a copy of VOLUME with no network and archive_mode=off: it must never push anything into the repository
# while the live server is still archiving. restore_command (archive-get) reads the repository read-only.
# Extra arguments are passed to postgres.
start_copy() { # VOLUME [postgres args...]
	vol="$1"
	shift
	docker run -d --init --name "$VERIFY" --network none --cap-drop ALL \
		--cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER --cap-add SETGID --cap-add SETUID \
		--security-opt no-new-privileges:true --shm-size 256m \
		--tmpfs /tmp --tmpfs /var/run/postgresql --tmpfs /var/spool/pgbackrest:mode=1777 \
		-v "$vol":/var/lib/postgresql -v "$PITR_DIR":/var/lib/pgbackrest:ro -v "$CONF":/etc/pgbackrest/pgbackrest.conf:ro \
		"$IMAGE" postgres -c archive_mode=off -c listen_addresses= -c shared_buffers=256MB -c max_wal_size=4GB "$@" > /dev/null
}

# Wait until the copy has promoted; sets $waited. Returns 1 (after printing its log) when it stopped or timed out.
wait_promoted() {
	waited=0
	while :; do
		if [ "$(docker inspect -f '{{.State.Running}}' "$VERIFY" 2>/dev/null)" != "true" ]; then
			docker logs --tail 30 "$VERIFY" >&2
			return 1
		fi
		[ "$(restored -c 'SELECT pg_is_in_recovery()' 2>/dev/null)" = "f" ] && return 0
		sleep 3
		waited=$((waited + 3))
		if [ $waited -gt "${RESTORE_WAIT_SECONDS:-3600}" ]; then
			docker logs --tail 30 "$VERIFY" >&2
			return 1
		fi
	done
}

# Promotion writes <new timeline>.history, but with archive_mode=off nothing marks it for archiving. Without it in the
# repository a later restore could pick the same timeline number again and collide with this one's WAL. Marking it
# .ready makes the production server archive it as soon as it runs on this volume.
mark_history_ready() { # VOLUME (the copy must be stopped)
	docker run --rm --network none --user 70:70 --cap-drop ALL --security-opt no-new-privileges:true \
		-v "$1":/var/lib/postgresql --entrypoint /bin/sh "$IMAGE" -c '
		cd "$0/pg_wal" && for h in *.history; do
			[ -f "$h" ] || continue
			[ -f "archive_status/$h.done" ] || touch "archive_status/$h.ready"
		done' "$PGDATA_DIR"
}

# Timeline of a (stopped) data directory, from pg_control.
volume_timeline() { # VOLUME
	docker run --rm --network none --user 70:70 --cap-drop ALL -v "$1":/var/lib/postgresql --entrypoint pg_controldata \
		"$IMAGE" "$PGDATA_DIR" | awk -F: '/Latest checkpoint.s TimeLineID/ { gsub(/ /, "", $2); print $2 }'
}

# Newest timeline in the repository (archived WAL and history files).
repo_timeline() {
	docker run --rm --network none --user 70:70 --cap-drop ALL --security-opt no-new-privileges:true --read-only \
		--tmpfs /tmp -v "$PITR_DIR":/var/lib/pgbackrest:ro -v "$CONF":/etc/pgbackrest/pgbackrest.conf:ro "$IMAGE" \
		pgbackrest --stanza=aether repo-ls --recurse archive/aether | python3 -c '
import re, sys
print(max([int(m.group(1), 16) for m in re.finditer(r"(?m)(?:^|/)([0-9A-F]{8})(?:[0-9A-F]{16}|\.history)", sys.stdin.read())] or [1]))'
}

# A volume that is behind the repository's newest timeline (going back to the volume a cutover left) would continue
# an abandoned timeline: its WAL names then sort below the newer timeline's, and pgBackRest's expire removes them even
# while a backup needs them. Give it a fresh timeline first: archive recovery to the end of its own timeline (no
# target: recovery_target=immediate never triggers on a cleanly stopped server), which then promotes.
bump_timeline() { # VOLUME — returns 1 on failure, with the volume left as it was
	docker run --rm --network none --user 70:70 --cap-drop ALL -v "$1":/var/lib/postgresql --entrypoint touch \
		"$IMAGE" "$PGDATA_DIR/recovery.signal"
	# A volume made by an earlier restore may still carry that restore's recovery_target_* in postgresql.auto.conf;
	# with recovery.signal present they would apply again. Neutralise every target on the command line (it wins over
	# the file), then remove them from the file once the copy has promoted.
	start_copy "$1" -c 'restore_command=pgbackrest --stanza=aether archive-get %f "%p"' \
		-c recovery_target= -c recovery_target_time= -c recovery_target_name= -c recovery_target_xid= \
		-c recovery_target_lsn= -c recovery_target_action=promote -c recovery_target_timeline=current
	if ! wait_promoted || ! reset_recovery_targets; then
		docker rm -f "$VERIFY" > /dev/null 2>&1
		docker run --rm --network none --user 70:70 --cap-drop ALL -v "$1":/var/lib/postgresql --entrypoint rm \
			"$IMAGE" -f "$PGDATA_DIR/recovery.signal"
		return 1
	fi
	docker stop -t 120 "$VERIFY" > /dev/null
	docker rm "$VERIFY" > /dev/null
}

# pgBackRest writes the restore's target into postgresql.auto.conf. After promotion it has done its job, and left in
# place it would apply again to any later archive recovery of this data directory (a rollback onto this volume).
# restore_command stays: it is harmless without a signal file and useful for exactly such a recovery.
reset_recovery_targets() { # on the running copy
	restored -c 'ALTER SYSTEM RESET recovery_target_time' -c 'ALTER SYSTEM RESET recovery_target_action' \
		-c 'ALTER SYSTEM RESET recovery_target_timeline' > /dev/null
}

# If a cutover fails half-way, put the stack back on the volume it ran on (EXIT trap, so it also covers `die` and
# set -e): SWITCH_PHASE says how far it got.
SWITCH_PHASE=""
switch_exit() {
	case "$SWITCH_PHASE" in
		stopped)
			say "cutover failed: starting the stack again on $CURRENT_VOLUME"
			# shellcheck disable=SC2086
			dc up -d postgres pitr $running || true
			;;
		switched)
			say "cutover failed: switching back to $CURRENT_VOLUME"
			set_env AETHER_PG_VOLUME "$CURRENT_VOLUME" || true
			set_env AETHER_PG_VOLUME_PREVIOUS "$PREVIOUS_BEFORE" || true
			dc stop || true
			# shellcheck disable=SC2086
			dc up -d postgres pitr $running || true
			;;
	esac
}

switch_volume() { # NAME
	docker volume inspect "$1" > /dev/null 2>&1 || die "volume $1 does not exist"
	[ "$1" != "$CURRENT_VOLUME" ] || die "the database already runs on $1"
	if [ -n "$(printenv AETHER_PG_VOLUME || true)" ] && [ "$(printenv AETHER_PG_VOLUME)" != "$CURRENT_VOLUME" ]; then
		die "AETHER_PG_VOLUME is exported in this shell and differs from $ENV_FILE: unset it first, a switch would not take effect"
	fi
	docker run --rm --network none --user 70:70 --cap-drop ALL -v "$1":/var/lib/postgresql --entrypoint test \
		"$IMAGE" -f "$PGDATA_DIR/PG_VERSION" || die "$1 does not hold a PostgreSQL data directory"
	# Two servers on one data directory corrupt it: the inspection copy must be gone (stopped cleanly) first.
	if docker ps -a --format '{{.Names}}' | grep -qx "$VERIFY"; then
		docker stop -t 120 "$VERIFY" > /dev/null
		docker rm "$VERIFY" > /dev/null
	fi
	say "taking a pg_dump of the current database first (kept in $BACKUP_DIR/daily)"
	sh "$HERE/backup-now.sh" --env-file "$ENV_FILE" --project "$PROJECT" \
		|| say "WARNING: the pre-cutover dump failed; continuing (the old volume is kept anyway)"
	# Bring back exactly what ran before (plus postgres and pitr); a stack stopped on purpose stays stopped.
	running="$(dc ps --status running --services | grep -Evx 'postgres|pitr' | tr '\n' ' ' || true)"
	if dc exec -T pitr pg_isready -q -h /var/run/postgresql -U postgres -d aether > /dev/null 2>&1; then
		say "archiving the current WAL segment of the database being replaced"
		dc exec -T pitr pgbackrest --stanza=aether check > /dev/null || say "WARNING: pgbackrest check failed"
	fi
	PREVIOUS_BEFORE="$(env_get AETHER_PG_VOLUME_PREVIOUS)"
	trap switch_exit EXIT
	say "stopping the stack"
	SWITCH_PHASE=stopped
	dc stop
	vol_tli="$(volume_timeline "$1")"
	repo_tli="$(repo_timeline)"
	[ -n "$vol_tli" ] || die "cannot read the timeline of $1 (pg_controldata failed)"
	if [ "$vol_tli" -lt "${repo_tli:-0}" ]; then
		say "$1 is on timeline $vol_tli but the repository already has timeline $repo_tli: moving it to a new timeline"
		bump_timeline "$1" || die "could not move $1 to a new timeline (log above); nothing was switched"
	fi
	mark_history_ready "$1"
	set_env AETHER_PG_VOLUME_PREVIOUS "$CURRENT_VOLUME"
	set_env AETHER_PG_VOLUME "$1"
	SWITCH_PHASE=switched
	say "AETHER_PG_VOLUME: $CURRENT_VOLUME -> $1 (rollback: $0 --use-volume $CURRENT_VOLUME)"
	# shellcheck disable=SC2086
	dc up -d postgres pitr $running || die "the stack did not start on $1 (see 'logs postgres')"
	wait_healthy postgres || die "postgres did not become healthy on $1 (see 'logs postgres')"
	SWITCH_PHASE=complete
	say "postgres is healthy on $1; taking a full backup of the new timeline"
	i=0
	until dc exec -T pitr pgbackrest --stanza=aether --log-level-console=info backup --type=full; do
		i=$((i + 1))
		[ $i -lt 10 ] || die "full backup failed; run infra/prod/pitr-backup-now.sh full once the pitr service is up"
		sleep 30
	done
	say "cutover done"
}

if [ -n "$USE_VOLUME" ]; then
	switch_volume "$USE_VOLUME"
	exit 0
fi

# ------------------------------------------------------------------------------------------------ checks
NEW=""
cleanup_new() { # only what this run created
	[ -n "$NEW" ] || return 0
	docker rm -f "$VERIFY" > /dev/null 2>&1 || true
	docker volume rm "$NEW" > /dev/null 2>&1 || true
}
# A drill never leaves anything behind, whatever happens, and a drill that could not finish is logged as FAIL too.
drill_exit() {
	cleanup_new
	if [ -z "$DRILL_LOGGED" ] && [ -n "$BACKUP_DIR" ] && [ -d "$BACKUP_DIR" ]; then
		echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) drill target=$TARGET FAIL ${DIE_REASON:-aborted}" >> "$BACKUP_DIR/drills.log"
	fi
}
if $DRILL; then trap drill_exit EXIT; fi
# Works with the live database down (the usual reason to restore): the repository is read by a one-off container
# and the target is parsed here. The live database is only used when it answers: to archive the current WAL
# segment, to size the copy, and to compare the result.
LIVE=false
if dc ps --status running --services 2>/dev/null | grep -qx pitr \
	&& dc exec -T pitr pg_isready -q -h /var/run/postgresql -U postgres -d aether > /dev/null 2>&1; then
	LIVE=true
fi
if $DRILL; then
	$LIVE || die "a drill needs the live database and the pitr service running"
	TARGET="$(python3 -c 'import datetime, sys, time; print(datetime.datetime.fromtimestamp(time.time() - int(sys.argv[1]) * 60, datetime.timezone.utc).strftime("%Y-%m-%d %H:%M:%S+00"))' "$MINUTES")"
	CUTOVER=false
	KEEP=false
fi
[ -n "$TARGET" ] || die "--target \"YYYY-MM-DD HH:MM:SS+07\" is required (see --help)"

if $LIVE; then
	# Recovery to a time stops at the first commit after it; on a quiet database there may be none, and recovery
	# then fails with "recovery ended before configured recovery target was reached". A transaction id assigned now
	# writes such a commit, and check archives it.
	live -c 'SELECT pg_current_xact_id()' > /dev/null || true
	say "archiving the current WAL segment (pgbackrest check)"
	dc exec -T pitr pgbackrest --stanza=aether check > /dev/null \
		|| say "WARNING: check failed: WAL after the last archived segment may be missing; recovery stops where the archive ends"
	[ -n "$TIMELINE" ] || TIMELINE="$(live -c "SELECT ('x' || substr(pg_walfile_name(pg_current_wal_lsn()), 1, 8))::bit(32)::int")"
else
	say "WARNING: the live database does not answer; restoring from the repository alone"
fi

INFO_FILE="$(mktemp)"
docker run --rm --network none --user 70:70 --cap-drop ALL --security-opt no-new-privileges:true --read-only --tmpfs /tmp \
	-v "$PITR_DIR":/var/lib/pgbackrest:ro -v "$CONF":/etc/pgbackrest/pgbackrest.conf:ro "$IMAGE" \
	pgbackrest --stanza=aether --output=json info > "$INFO_FILE" || { rm -f "$INFO_FILE"; die "cannot read the pgBackRest repository"; }
# -> "target_epoch eligible_backups size_bytes timeline_of_newest_eligible_backup", or an error message.
plan="$(python3 - "$TARGET" "$INFO_FILE" 2>&1 <<'PY'
import datetime, json, re, sys, time
m = re.fullmatch(r"(\d{4}-\d\d-\d\d)[ T](\d\d:\d\d(?::\d\d(?:\.\d{1,6})?)?)\s*(Z|UTC|[+-]\d\d(?::?\d\d)?)", sys.argv[1].strip())
if not m:
    sys.exit('the target must look like "2026-09-30 14:05:00+07" (a time zone is required)')
tz = m.group(3)
tz = "+00:00" if tz in ("Z", "UTC") else (tz + ":00" if len(tz) == 3 else tz[:3] + ":" + tz[-2:])
t = datetime.datetime.fromisoformat(m.group(1) + " " + m.group(2) + tz).timestamp()
if t >= time.time():
    sys.exit("the target is in the future")
stanza = (json.load(open(sys.argv[2])) or [{}])[0]
ok = [b for b in stanza.get("backup", []) if not b.get("error") and b["timestamp"]["stop"] < t]
if not ok:
    sys.exit("no backup finished before the target: the recovery window starts later (infra/prod/pitr-info.sh)")
newest = max(ok, key=lambda b: b["timestamp"]["stop"])
print(int(t), len(ok), newest["info"]["size"], int(newest["archive"]["stop"][:8], 16))
PY
)" || { rm -f "$INFO_FILE"; die "$plan"; }
rm -f "$INFO_FILE"
# shellcheck disable=SC2086 # four words, split on purpose
set -- $plan
target_epoch="$1"
db_bytes="$3"
[ -n "$TIMELINE" ] || TIMELINE="$4"
case "$TIMELINE" in *[!0-9]*|"") die "--timeline must be a number" ;; esac
if $LIVE; then
	db_bytes="$(live -c 'SELECT sum(pg_database_size(datname))::bigint FROM pg_database')"
fi
if $DRILL && [ -n "$DRILL_TABLE" ]; then
	has_table="$(live -v t="$DRILL_TABLE" <<'SQL'
SELECT to_regclass(:'t') IS NOT NULL;
SQL
)"
	if [ "$has_table" = "t" ]; then
		live_window="$(live -v t="$target_epoch" <<SQL
SELECT count(*) FROM $DRILL_TABLE
WHERE $DRILL_COLUMN > to_timestamp(:'t'::bigint) - interval '1 hour' AND $DRILL_COLUMN <= to_timestamp(:'t'::bigint) - interval '1 minute';
SQL
)"
	else
		DRILL_TABLE=""
	fi
fi

# ------------------------------------------------------------------------------------------------ restore
if docker ps -a --format '{{.Names}}' | grep -qx "$VERIFY"; then
	die "a previous $VERIFY container exists (an inspection copy?): docker rm -f $VERIFY"
fi
STAMP="$(date -u +%Y%m%d%H%M%S)"
NEW="${PROJECT}_postgres-data-r$STAMP"
docker volume create --label "com.docker.compose.project=$PROJECT" --label com.docker.compose.volume=postgres-data \
	--label "aether.pitr.target=$TARGET" "$NEW" > /dev/null
say "restoring to $TARGET (timeline $TIMELINE) into volume $NEW"

free_kb="$(docker run --rm --network none -v "$NEW":/x --entrypoint df "$IMAGE" -Pk /x | awk 'NR == 2 { print $4 }')"
if [ $((free_kb * 1024)) -lt $((db_bytes + db_bytes / 5)) ]; then
	cleanup_new
	die "not enough disk for the restored copy: $((free_kb / 1024)) MB free, database is $((db_bytes / 1048576)) MB (+20%)"
fi

docker run --rm --network none --user 0:0 --cap-drop ALL --cap-add CHOWN --cap-add FOWNER --entrypoint /bin/sh \
	-v "$NEW":/var/lib/postgresql "$IMAGE" -c "install -d -o postgres -g postgres -m 0700 '$PGDATA_DIR' && chown postgres:postgres /var/lib/postgresql"
if ! docker run --rm --network none --user 70:70 --cap-drop ALL --security-opt no-new-privileges:true --read-only \
	--tmpfs /tmp -v "$NEW":/var/lib/postgresql -v "$PITR_DIR":/var/lib/pgbackrest:ro \
	-v "$CONF":/etc/pgbackrest/pgbackrest.conf:ro "$IMAGE" \
	pgbackrest --stanza=aether --log-level-console=info restore --type=time "--target=$TARGET" --target-action=promote \
	"--target-timeline=$TIMELINE"; then
	cleanup_new
	die "pgbackrest restore failed (volume removed)"
fi

start_copy "$NEW"
say "replaying WAL up to the target (container $VERIFY)"
wait_promoted || { cleanup_new; die "recovery did not complete (log above; volume removed). If it says the target was not reached, the archive ends before the target: choose a target before the 'last completed transaction' time in the log"; }
reset_recovery_targets || { cleanup_new; die "could not reset the recovery target on the restored copy"; }
say "the restored copy promoted after ${waited}s"

# ------------------------------------------------------------------------------------------------ verification
COUNTS='SELECT c.oid::regclass::text || '"'"' '"'"' || (xpath('"'"'/row/c/text()'"'"', query_to_xml(format('"'"'SELECT count(*) AS c FROM %s'"'"', c.oid::regclass), false, true, '"'"''"'"')))[1]::text
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname IN ('"'"'core'"'"', '"'"'identity'"'"') AND c.relkind IN ('"'"'r'"'"', '"'"'p'"'"') AND NOT c.relispartition ORDER BY 1'
FACTS="SELECT 'goose_version ' || CASE WHEN to_regclass('public.goose_db_version') IS NULL THEN 'none' ELSE
  (xpath('/row/v/text()', query_to_xml('SELECT max(version_id) AS v FROM public.goose_db_version WHERE is_applied', false, true, '')))[1]::text END
UNION ALL SELECT 'core_tables_without_forced_rls ' || count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
  WHERE n.nspname = 'core' AND c.relkind IN ('r', 'p') AND NOT (c.relrowsecurity AND c.relforcerowsecurity)"
restored_facts="$(restored -c "$FACTS")"
live_facts=""
if $LIVE; then live_facts="$(live -c "$FACTS")"; fi
last_replay="$(restored -c "SELECT coalesce(to_char(pg_last_xact_replay_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS') || '+00', 'none')")"
newest_sample=""
if [ -n "$DRILL_TABLE" ] && [ "$(restored -c "SELECT to_regclass('$DRILL_TABLE') IS NOT NULL" 2>/dev/null)" = "t" ]; then
	newest_sample="$(restored -c "SELECT coalesce(extract(epoch FROM max($DRILL_COLUMN))::bigint, 0) FROM $DRILL_TABLE")"
fi

echo
echo "  target                          $TARGET"
echo "  last transaction replayed       $last_replay"
printf '%s\n' "$restored_facts" | while read -r k v; do
	l="$(printf '%s\n' "$live_facts" | awk -v k="$k" '$1 == k { print $2 }')"
	printf '  %-31s restored %-10s live %s\n' "$k" "$v" "${l:-n/a}"
done
echo "  rows per table (restored / live):"
counts_restored="$(mktemp)"
counts_live="$(mktemp)"
restored -c "$COUNTS" > "$counts_restored"
if $LIVE; then live -c "$COUNTS" > "$counts_live"; fi
awk 'FILENAME == ARGV[1] { live[$1] = $2; next } { printf "    %-40s %12s %12s\n", $1, $2, ($1 in live ? live[$1] : "-") }' \
	"$counts_live" "$counts_restored"
rm -f "$counts_restored" "$counts_live"
echo

problems=""
# Row-level security must be as strict as on the live database (0 unforced core tables in production; 0 required
# when the live database does not answer).
restored_unforced="$(printf '%s\n' "$restored_facts" | awk '$1 == "core_tables_without_forced_rls" { print $2 }')"
live_unforced="$(printf '%s\n' "$live_facts" | awk '$1 == "core_tables_without_forced_rls" { print $2 }')"
[ "$restored_unforced" -le "${live_unforced:-0}" ] || problems="$problems; core tables without forced RLS ($restored_unforced, live ${live_unforced:-n/a})"
if [ -n "$newest_sample" ] && [ "$newest_sample" -gt "$target_epoch" ]; then
	problems="$problems; $DRILL_TABLE holds rows newer than the target"
fi

if $DRILL; then
	restored_version="$(printf '%s\n' "$restored_facts" | awk '$1 == "goose_version" { print $2 }')"
	live_version="$(printf '%s\n' "$live_facts" | awk '$1 == "goose_version" { print $2 }')"
	[ "$restored_version" = "$live_version" ] || problems="$problems; migration version $restored_version != live $live_version"
	window=""
	if [ -n "$DRILL_TABLE" ]; then
		restored_window="$(restored -v t="$target_epoch" <<SQL
SELECT count(*) FROM $DRILL_TABLE
WHERE $DRILL_COLUMN > to_timestamp(:'t'::bigint) - interval '1 hour' AND $DRILL_COLUMN <= to_timestamp(:'t'::bigint) - interval '1 minute';
SQL
)"
		window=" window_rows restored=$restored_window live=$live_window"
		[ "$restored_window" = "$live_window" ] || problems="$problems; $DRILL_TABLE rows in the hour before the target differ"
	fi
	verdict=PASS
	[ -z "$problems" ] || verdict="FAIL${problems}"
	line="$(date -u +%Y-%m-%dT%H:%M:%SZ) drill target=$TARGET replayed_to=$last_replay recovery_seconds=$waited$window $verdict"
	echo "$line"
	if [ -n "$BACKUP_DIR" ] && [ -d "$BACKUP_DIR" ]; then echo "$line" >> "$BACKUP_DIR/drills.log"; fi
	DRILL_LOGGED=yes
	cleanup_new
	say "drill copy removed"
	[ "$verdict" = PASS ]
	exit $?
fi

[ -z "$problems" ] || say "WARNING:${problems#;}"
if $CUTOVER; then
	[ -z "$problems" ] || die "not cutting over because of the warning above; inspect, then: $0 --use-volume $NEW"
	switch_volume "$NEW"
	exit 0
fi
if $KEEP; then
	say "the restored copy keeps running for inspection: docker exec -it -u postgres $VERIFY psql -d aether"
	say "stop it before any cutover: docker rm -f $VERIFY"
else
	docker stop -t 120 "$VERIFY" > /dev/null
	docker rm "$VERIFY" > /dev/null
fi
say "restored volume: $NEW (live database unchanged, still on $CURRENT_VOLUME)"
say "switch to it:    $0 --use-volume $NEW"
say "or discard it:   docker volume rm $NEW"
