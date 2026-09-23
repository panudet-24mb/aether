#!/bin/sh
# Restore a dump produced by backup.sh.
#   infra/prod/restore.sh <dump-file> [--database NAME] [--force] [--env-file FILE] [--project NAME]
#
# <dump-file> may be a path on this host inside the backup directory, or just the file name; it is
# resolved against /backups inside the container. The target database is created fresh; an existing
# non-empty database is refused unless --force is given (which DROPs it).
#
# Restoring into the live `aether` database requires the API and the collector to be stopped first;
# the script does that for you and starts them again afterwards.
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
		-h|--help) sed -n '2,12p' "$0"; exit 0 ;;
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

STOPPED=""
if [ "$TARGET" = "aether" ]; then
	echo "target is the live database: stopping api, mqtt-ingest and mqtt-provisioner"
	dc stop api mqtt-ingest mqtt-provisioner
	STOPPED="yes"
fi

# The restore recreates the application roles, so it needs their passwords. They are forwarded
# from the env file through the environment (-e NAME), never as command arguments.
APP_DB_PASSWORD="$(grep '^APP_DB_PASSWORD=' "$ENV_FILE" | cut -d= -f2-)"
MQTT_PROVISION_DB_PASSWORD="$(grep '^MQTT_PROVISION_DB_PASSWORD=' "$ENV_FILE" | cut -d= -f2-)"
export APP_DB_PASSWORD MQTT_PROVISION_DB_PASSWORD

set +e
dc exec -T -e APP_DB_PASSWORD -e MQTT_PROVISION_DB_PASSWORD \
	backup /bin/sh /opt/aether/restore-inner.sh "$INNER" "$TARGET" "$FORCE"
STATUS=$?
set -e

if [ -n "$STOPPED" ]; then
	dc start api mqtt-ingest mqtt-provisioner
fi
exit "$STATUS"
