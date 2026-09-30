# Shared front matter for the operator scripts in this directory. Sourced, not executed.
# Defines: HERE, ROOT, ENV_FILE, PROJECT, and dc() — the compose invocation for this deployment.
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
ENV_FILE="${AETHER_ENV_FILE:-$ROOT/.env.prod}"
PROJECT="${AETHER_PROJECT:-aether-prod}"

parse_common() {
	REST=""
	while [ $# -gt 0 ]; do
		case "$1" in
			--env-file) ENV_FILE="$2"; shift 2 ;;
			--project|-p) PROJECT="$2"; shift 2 ;;
			*) REST="$REST $1"; shift ;;
		esac
	done
}

require_env() {
	[ -f "$ENV_FILE" ] || { echo "env file not found: $ENV_FILE (run infra/prod/setup.py first)" >&2; exit 1; }
	warn_shell_overrides
}

# Compose gives an exported shell variable precedence over --env-file. For these, a stale export would make compose
# use another database volume, repository or image than the one the scripts read from the env file.
warn_shell_overrides() {
	for name in AETHER_PG_VOLUME AETHER_PITR_DIR AETHER_POSTGRES_IMAGE AETHER_SECRETS_DIR AETHER_BACKUP_DIR; do
		shell_value="$(printenv "$name" || true)"
		[ -n "$shell_value" ] || continue
		file_value="$(env_get "$name")"
		if [ "$shell_value" != "$file_value" ]; then
			echo "WARNING: $name is exported in this shell ($shell_value) and overrides $ENV_FILE ($file_value) for" \
				"docker compose; run 'unset $name' unless that is intended" >&2
		fi
	done
}

dc() {
	docker compose --env-file "$ENV_FILE" -f "$HERE/compose.yaml" -p "$PROJECT" "$@"
}

# env_get NAME — one value from the env file (never echoed by the callers when it is a secret).
env_get() {
	grep "^$1=" "$ENV_FILE" | tail -n 1 | cut -d= -f2-
}

# The data directory inside the postgres image (PGDATA of postgres:18); pg1-path in pgbackrest.conf.
# shellcheck disable=SC2034 # used by the scripts that source this file
PGDATA_DIR=/var/lib/postgresql/18/docker
