#!/bin/sh
set -eu
umask 077
root=$(cd "$(dirname "$0")/../.." && pwd)
directory=${1:-$root/.local/deployment}
if [ -e "$directory" ]; then
    echo 'Secret directory exists; preserve the existing database credentials.' >&2
    exit 1
fi
mkdir -p "$(dirname "$directory")"
mkdir "$directory"
chmod 700 "$directory"
openssl rand -hex 32 | tr -d '\r' > "$directory/postgres_password"
openssl rand -hex 32 | tr -d '\r' > "$directory/migrate_password"
openssl rand -hex 32 | tr -d '\r' > "$directory/runtime_password"
openssl rand -hex 32 | tr -d '\r' > "$directory/replication_password"
migrate_password=$(cat "$directory/migrate_password")
runtime_password=$(cat "$directory/runtime_password")
printf '%s' "postgres://fleet_migrator:${migrate_password}@postgres:5432/fleet?sslmode=disable" > "$directory/migrate_url"
printf '%s' "postgres://fleet_app:${runtime_password}@postgres:5432/fleet?sslmode=disable" > "$directory/runtime_url"
# compose.ha.yaml: try both servers and use whichever accepts writes.
ha_hosts='postgres:5432,postgres-standby:5432'
printf '%s' "postgres://fleet_migrator:${migrate_password}@${ha_hosts}/fleet?sslmode=disable&target_session_attrs=read-write" > "$directory/migrate_ha_url"
printf '%s' "postgres://fleet_app:${runtime_password}@${ha_hosts}/fleet?sslmode=disable&target_session_attrs=read-write" > "$directory/runtime_ha_url"
# Docker mounts these files individually. Private parent prevents host traversal;
# readable file mounts permit the unprivileged container to use its own secret.
chmod 444 "$directory/"*
echo 'Created deployment secrets in the private directory; preserve them with the database.'
