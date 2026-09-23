#!/bin/sh
# Restore one custom-format dump into a database, inside the `backup` container.
#   restore-inner.sh <dump-path> <database> <force:true|false>
# Refuses to touch a database that already holds tables unless force is true.
set -eu
umask 077
DUMP="$1"
TARGET="$2"
FORCE="${3:-false}"

[ -f "$DUMP" ] || { echo "dump not found: $DUMP" >&2; exit 1; }
pg_restore --list "$DUMP" > /dev/null || { echo "dump is unreadable: $DUMP" >&2; exit 1; }

q() { psql --dbname "$1" --no-align --tuples-only --set=ON_ERROR_STOP=1 -c "$2"; }

# Roles first, exactly as infra/prod/init-db.sh creates them: the dump carries ownership and grants
# for aether_owner / aether_app and restoring without them fails halfway through.
: "${APP_DB_PASSWORD:?APP_DB_PASSWORD required}"
: "${MQTT_PROVISION_DB_PASSWORD:?MQTT_PROVISION_DB_PASSWORD required}"
psql --dbname postgres --set=ON_ERROR_STOP=1 <<'SQL' > /dev/null
DO $$ BEGIN
  IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='aether_owner') THEN
    CREATE ROLE aether_owner NOLOGIN NOSUPERUSER NOBYPASSRLS;
  END IF;
  IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='aether_app') THEN
    CREATE ROLE aether_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
  END IF;
  IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='aether_mqtt_provisioner') THEN
    CREATE ROLE aether_mqtt_provisioner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
  END IF;
END $$;
SQL
# psql does not interpolate :'variables' inside -c, so this goes in through stdin. The passwords
# therefore never appear in the process arguments either.
psql --dbname postgres --set=ON_ERROR_STOP=1 \
     --set=app_password="$APP_DB_PASSWORD" --set=prov_password="$MQTT_PROVISION_DB_PASSWORD" <<'SQL' > /dev/null
ALTER ROLE aether_app PASSWORD :'app_password';
ALTER ROLE aether_mqtt_provisioner PASSWORD :'prov_password';
SQL

exists="$(q postgres "SELECT count(*) FROM pg_database WHERE datname='$TARGET'")"
if [ "$exists" = "0" ]; then
	psql --dbname postgres --set=ON_ERROR_STOP=1 -c "CREATE DATABASE \"$TARGET\"" > /dev/null
	echo "created database $TARGET"
else
	tables="$(q "$TARGET" "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind='r' AND n.nspname NOT IN ('pg_catalog','information_schema')")"
	if [ "$tables" != "0" ] && [ "$FORCE" != "true" ]; then
		echo "refusing to restore into $TARGET: it already holds $tables tables (pass --force to overwrite)" >&2
		exit 2
	fi
	if [ "$tables" != "0" ]; then
		echo "--force: dropping and recreating $TARGET"
		psql --dbname postgres --set=ON_ERROR_STOP=1 -c "DROP DATABASE \"$TARGET\" WITH (FORCE)" > /dev/null
		psql --dbname postgres --set=ON_ERROR_STOP=1 -c "CREATE DATABASE \"$TARGET\"" > /dev/null
	fi
fi

psql --dbname "$TARGET" --set=ON_ERROR_STOP=1 -c 'REVOKE CREATE ON SCHEMA public FROM PUBLIC' > /dev/null
psql --dbname "$TARGET" --set=ON_ERROR_STOP=1 -c "GRANT CONNECT ON DATABASE \"$TARGET\" TO aether_app, aether_mqtt_provisioner" > /dev/null
psql --dbname "$TARGET" --set=ON_ERROR_STOP=1 -c "GRANT CREATE ON DATABASE \"$TARGET\" TO aether_owner" > /dev/null

# Roles exist and this connection is superuser, so ownership and grants restore as dumped.
pg_restore --dbname "$TARGET" --exit-on-error "$DUMP"

echo "restored $DUMP into $TARGET"
q "$TARGET" "SELECT 'tables=' || count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind='r' AND n.nspname IN ('core','identity')"
