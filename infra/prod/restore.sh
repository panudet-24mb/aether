#!/bin/sh
# Restore a dump produced by backup.sh.
#   infra/prod/restore.sh <dump-file> [--database NAME] [--force] [--env-file FILE] [--project NAME]
#
# <dump-file> may be a path on this host inside the backup directory, or just the file name; it is
# resolved against /backups inside the container. The target database is created fresh; an existing
# non-empty database is refused unless --force is given (which DROPs it).
#
# Restoring into the live `aether` database requires every service that writes to it to be stopped first
# (api, mqtt-ingest, mqtt-provisioner, mqtt-commander, tuya-cloud); the script does that for you and starts them
# again afterwards.
# shellcheck source=_common.sh
. "$(cd "$(dirname "$0")" && pwd)/_common.sh"

DUMP=""
TARGET="aether_restore"
FORCE="false"
while [ $# -gt 0 ]; do
	case "$1" in
		--database) TARGET="$2"; shift 2 ;;
		--force) FORCE="true"; shift ;;
		--env-file) ENV_FILE="$2"; shift 2 ;;
		--project|-p) PROJECT="$2"; shift 2 ;;
		-h|--help) sed -n '2,11p' "$0"; exit 0 ;;
		*) DUMP="$1"; shift ;;
	esac
done
[ -n "$DUMP" ] || { echo "usage: restore.sh <dump-file> [--database NAME] [--force]" >&2; exit 1; }
require_env

# Accept an absolute host path, a daily/weekly relative path or a bare file name.
case "$DUMP" in
	/*) NAME="$(basename "$DUMP")" ;;
	*) NAME="$DUMP" ;;
esac
case "$NAME" in
	*/*) INNER="/backups/$NAME" ;;
	*) INNER="/backups/daily/$NAME" ;;
esac

# Every service that connects to the application database. mqtt-commander and tuya-cloud write too (command
# claims, Tuya ingest): leaving them running would race the restore.
WRITERS="api mqtt-ingest mqtt-provisioner mqtt-commander tuya-cloud"
STOPPED=""
if [ "$TARGET" = "aether" ]; then
	echo "target is the live database: stopping every service that writes to it"
	# shellcheck disable=SC2086 # a list of service names
	dc stop $WRITERS
	STOPPED="yes"
fi

# The restore recreates the application roles, so it needs their passwords. They are forwarded
# from the env file through the environment (-e NAME), never as command arguments.
APP_DB_PASSWORD="$(grep '^APP_DB_PASSWORD=' "$ENV_FILE" | cut -d= -f2-)"
MQTT_PROVISION_DB_PASSWORD="$(grep '^MQTT_PROVISION_DB_PASSWORD=' "$ENV_FILE" | cut -d= -f2-)"
export APP_DB_PASSWORD MQTT_PROVISION_DB_PASSWORD
# Forwarded only when the env file has one (from migration 00040 on): psql's \getenv then sees it unset.
AUTH_FORWARD=""
AUTH_DB_PASSWORD="$(grep '^AUTH_DB_PASSWORD=' "$ENV_FILE" | cut -d= -f2- || true)"
if [ -n "$AUTH_DB_PASSWORD" ]; then
	export AUTH_DB_PASSWORD
	AUTH_FORWARD="-e AUTH_DB_PASSWORD"
fi

set +e
# shellcheck disable=SC2086 # AUTH_FORWARD is empty or one option pair
dc exec -T -e APP_DB_PASSWORD -e MQTT_PROVISION_DB_PASSWORD $AUTH_FORWARD \
	backup /bin/sh /opt/aether/restore-inner.sh "$INNER" "$TARGET" "$FORCE"
STATUS=$?
set -e

if [ -n "$STOPPED" ]; then
	# shellcheck disable=SC2086
	dc start $WRITERS
fi
exit "$STATUS"
