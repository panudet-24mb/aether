#!/bin/sh
# Take a pgBackRest backup right now, in the running pitr service.
#   infra/prod/pitr-backup-now.sh [full|diff|incr] [--env-file FILE] [--project NAME]
# Default: full. Take one before an upgrade and right after a point-in-time restore (new timeline).
# shellcheck source=_common.sh
. "$(cd "$(dirname "$0")" && pwd)/_common.sh"
TYPE=full
while [ $# -gt 0 ]; do
	case "$1" in
		full|diff|incr) TYPE="$1"; shift ;;
		--env-file) ENV_FILE="$2"; shift 2 ;;
		--project|-p) PROJECT="$2"; shift 2 ;;
		*) echo "usage: pitr-backup-now.sh [full|diff|incr]" >&2; exit 1 ;;
	esac
done
require_env
dc exec -T pitr pgbackrest --stanza=aether --log-level-console=info backup --type="$TYPE"
