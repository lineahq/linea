#!/bin/sh
set -eu

# Runs only when the primary of compose.ha.yaml initializes an empty volume.
# Creates the streaming-replication role and the standby's slot, and admits
# replication connections with SCRAM. The password comes from a secret and
# never becomes a command argument.
FLEET_REPLICATION_PASSWORD=$(cat /run/secrets/replication_password)
export FLEET_REPLICATION_PASSWORD
psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --set ON_ERROR_STOP=1 <<'SQL'
\getenv replication_password FLEET_REPLICATION_PASSWORD
CREATE ROLE fleet_replicator LOGIN REPLICATION NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD :'replication_password';
SELECT pg_create_physical_replication_slot('fleet_standby');
SQL
unset FLEET_REPLICATION_PASSWORD
echo 'host replication fleet_replicator all scram-sha-256' >> "$PGDATA/pg_hba.conf"
