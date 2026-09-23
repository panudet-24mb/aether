#!/bin/sh
# Take a backup right now, using the running backup container.
#   infra/prod/backup-now.sh [--env-file FILE] [--project NAME]
# shellcheck source=_common.sh
. "$(cd "$(dirname "$0")" && pwd)/_common.sh"
parse_common "$@"
require_env
dc exec -T backup /bin/sh /opt/aether/backup.sh
