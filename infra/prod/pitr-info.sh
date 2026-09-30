#!/bin/sh
# What point-in-time recovery can do right now: backups, the WAL archive range, and the pitr service's checks.
#   infra/prod/pitr-info.sh [--env-file FILE] [--project NAME]
# The recovery window starts at the oldest backup's stop time and ends at the newest archived WAL (at most
# archive_timeout = 5 minutes behind now while archiving is healthy).
# shellcheck source=_common.sh
. "$(cd "$(dirname "$0")" && pwd)/_common.sh"
parse_common "$@"
require_env
dc exec -T pitr pgbackrest --stanza=aether info
echo
echo "pitr service status (/run/aether-ops/status.json):"
dc exec -T pitr cat /run/aether-ops/status.json 2>/dev/null || echo "  not written yet (first backup still running?)"
echo
