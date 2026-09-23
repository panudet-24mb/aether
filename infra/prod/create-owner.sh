#!/bin/sh
# Create the first owner account. Public registration is disabled in production, so this is the
# only way in. The password is read from a 0600 file and piped into the container over stdin: it
# never appears in argv, in `ps`, in the shell history or in any log.
#
#   infra/prod/create-owner.sh --email owner@example.org --name "ชื่อผู้ดูแล" \
#       --tenant "โรงพยาบาลตัวอย่าง" [--password-file PATH] [--env-file FILE] [--project NAME]
#
# With no --password-file the script generates one at <secrets-dir>/owner-password.txt (0600) and
# tells you where it is, without printing it.
# shellcheck source=_common.sh
. "$(cd "$(dirname "$0")" && pwd)/_common.sh"

EMAIL=""; NAME=""; TENANT=""; PWFILE=""
while [ $# -gt 0 ]; do
	case "$1" in
		--email) EMAIL="$2"; shift 2 ;;
		--name) NAME="$2"; shift 2 ;;
		--tenant) TENANT="$2"; shift 2 ;;
		--password-file) PWFILE="$2"; shift 2 ;;
		--env-file) ENV_FILE="$2"; shift 2 ;;
		--project|-p) PROJECT="$2"; shift 2 ;;
		-h|--help) sed -n '2,12p' "$0"; exit 0 ;;
		*) echo "unknown argument: $1" >&2; exit 1 ;;
	esac
done
[ -n "$EMAIL" ] && [ -n "$NAME" ] && [ -n "$TENANT" ] || {
	echo "usage: create-owner.sh --email ADDRESS --name NAME --tenant WORKSPACE" >&2; exit 1; }
require_env

SECRETS="$(grep '^AETHER_SECRETS_DIR=' "$ENV_FILE" | cut -d= -f2-)"
[ -n "$PWFILE" ] || PWFILE="$SECRETS/owner-password.txt"

if [ ! -f "$PWFILE" ]; then
	(umask 077; python3 -c "import secrets,sys;sys.stdout.write(secrets.token_urlsafe(24))" > "$PWFILE")
	chmod 0600 "$PWFILE"
	echo "generated a random owner password in $PWFILE (not printed here)"
fi
[ "$(stat -c '%a' "$PWFILE" 2>/dev/null || stat -f '%Lp' "$PWFILE")" = "600" ] || {
	echo "password file must be mode 0600: $PWFILE" >&2; exit 1; }

# The password reaches the container only on stdin. Inside, umask 077 makes the temporary file
# 0600 and owned by uid 10001, which is what /app/admin requires before it will read it.
dc run --rm -T --no-deps \
	-e ADMIN_EMAIL="$EMAIL" -e ADMIN_NAME="$NAME" -e TENANT_NAME="$TENANT" \
	--entrypoint /bin/sh api -c \
	'umask 077; cat > /tmp/owner-password; ADMIN_PASSWORD_FILE=/tmp/owner-password /app/admin bootstrap; s=$?; rm -f /tmp/owner-password; exit $s' \
	< "$PWFILE"
