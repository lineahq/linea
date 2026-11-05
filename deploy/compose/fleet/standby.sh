#!/bin/sh
set -eu

# Hot standby for compose.ha.yaml. On an empty volume it clones the primary
# once through the replication slot, then runs PostgreSQL as a streaming
# standby that an operator promotes with pg_promote(). The replication password
# comes from a secret and never becomes a command argument.
data=${PGDATA:-/var/lib/postgresql/data}
if [ ! -s "$data/PG_VERSION" ]; then
    mkdir -p "$data"
    chown postgres:postgres "$data"
    chmod 700 "$data"
    PGPASSWORD=$(cat /run/secrets/replication_password) gosu postgres pg_basebackup \
        --host postgres --username fleet_replicator --pgdata "$data" \
        --wal-method stream --slot fleet_standby --write-recovery-conf --checkpoint fast
fi
exec docker-entrypoint.sh "$@"
