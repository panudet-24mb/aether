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
}

dc() {
	docker compose --env-file "$ENV_FILE" -f "$HERE/compose.yaml" -p "$PROJECT" "$@"
}
