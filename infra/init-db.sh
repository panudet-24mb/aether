#!/bin/sh
set -eu
# Executed only on a new PostgreSQL volume. Secrets arrive through the environment.
psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --set=ON_ERROR_STOP=1 --set=app_password="$APP_DB_PASSWORD" <<'SQL'
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
CREATE ROLE aether_owner NOLOGIN NOSUPERUSER NOBYPASSRLS;
CREATE ROLE aether_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS PASSWORD :'app_password';
GRANT CONNECT ON DATABASE aether TO aether_app;
SQL
psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --set=ON_ERROR_STOP=1 -c 'GRANT CREATE ON DATABASE aether TO aether_owner;'
