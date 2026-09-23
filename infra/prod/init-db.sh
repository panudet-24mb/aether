#!/bin/sh
# Runs once, on a NEW PostgreSQL volume, from the stock entrypoint. Secrets arrive through the
# environment and are never echoed. Mirrors infra/init-db.sh and additionally pre-creates the MQTT
# provisioner login: migration 00005 creates that role only if it does not exist, and a role created
# by a migration has no password.
set -eu

psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --set=ON_ERROR_STOP=1 \
     --set=app_password="$APP_DB_PASSWORD" --set=prov_password="$MQTT_PROVISION_DB_PASSWORD" <<'SQL'
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
CREATE ROLE aether_owner NOLOGIN NOSUPERUSER NOBYPASSRLS;
CREATE ROLE aether_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS PASSWORD :'app_password';
CREATE ROLE aether_mqtt_provisioner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS PASSWORD :'prov_password';
GRANT CONNECT ON DATABASE aether TO aether_app, aether_mqtt_provisioner;
SQL

psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --set=ON_ERROR_STOP=1 \
     -c 'GRANT CREATE ON DATABASE aether TO aether_owner;'

# Refuse unencrypted TCP for every role. The local socket stays peer-authenticated so that
# pg_isready, the entrypoint and `docker compose exec -u postgres` keep working.
cat > "$PGDATA/pg_hba.conf" <<'HBA'
local   all             all                                     peer
local   replication     all                                     peer
hostssl all             all             0.0.0.0/0               scram-sha-256
hostssl all             all             ::/0                    scram-sha-256
HBA
