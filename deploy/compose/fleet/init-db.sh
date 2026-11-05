#!/bin/sh
set -eu

# Runs only when the official PostgreSQL image initializes an empty volume.
# psql reads passwords from its environment; they never become command arguments.
FLEET_MIGRATE_PASSWORD=$(cat /run/secrets/migrate_password)
FLEET_RUNTIME_PASSWORD=$(cat /run/secrets/runtime_password)
export FLEET_MIGRATE_PASSWORD FLEET_RUNTIME_PASSWORD
psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --set ON_ERROR_STOP=1 <<'SQL'
\getenv migrate_password FLEET_MIGRATE_PASSWORD
\getenv runtime_password FLEET_RUNTIME_PASSWORD
CREATE ROLE fleet_migrator LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD :'migrate_password';
CREATE ROLE fleet_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD :'runtime_password';
REVOKE ALL ON DATABASE fleet FROM PUBLIC;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT CONNECT,CREATE ON DATABASE fleet TO fleet_migrator;
GRANT CONNECT ON DATABASE fleet TO fleet_app;
SQL
unset FLEET_MIGRATE_PASSWORD FLEET_RUNTIME_PASSWORD
