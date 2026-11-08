#!/bin/bash
# Fleet PostgreSQL tests against one disposable, resource-bounded container
# (1 CPU, 256 MB, tmpfs data, random loopback port, random password), removed
# on exit. The shell twin of test-postgres.ps1.
#
#   scripts/fleet/test-postgres.sh [--race] [--inventory-records N]
#
#   --race               run the lifecycle checks under the race detector in a
#                        bounded golang container (2 CPUs, 2 GB, read-only
#                        mount of the checkout) that reaches the fixture over
#                        the Docker network
#   --inventory-records N  run only the opt-in inventory latency measurement
#                        at N records (0-100000; 0, the default, runs the
#                        lifecycle checks)
#   -h, --help           print this text
set -euo pipefail
export MSYS_NO_PATHCONV=1
winpath() { (cd "$1" && { pwd -W 2>/dev/null || pwd; }); }
root=$(winpath "$(dirname "$0")/../..")

usage() { awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; }
die() { echo "$*" >&2; exit 1; }

race=0
inventory_records=0
while [ $# -gt 0 ]; do
    case $1 in
        --race) race=1;;
        --inventory-records) [ $# -ge 2 ] || { usage >&2; exit 2; }; inventory_records=$2; shift;;
        -h|--help) usage; exit 0;;
        *) echo "unknown argument: $1" >&2; usage >&2; exit 2;;
    esac
    shift
done
case $inventory_records in
    ''|*[!0-9]*) echo '--inventory-records must be an integer from 0 to 100000' >&2; exit 2;;
esac
[ "$inventory_records" -le 100000 ] || { echo '--inventory-records must be an integer from 0 to 100000' >&2; exit 2; }

random_hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }
name="linea-fleet-pg-test-$(random_hex 16)"
image='postgres:17@sha256:b8faeff7712663c2773a190181e1246e37e364b887611c04658c28b44e2408fe'
go_image='golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9'
created=0
cleanup() {
    if [ "$created" = 1 ]; then docker rm --force "$name" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT
trap 'exit 1' INT TERM

POSTGRES_PASSWORD=$(random_hex 32)
export POSTGRES_PASSWORD
docker run --detach --name "$name" --cpus 1 --memory 256m \
    --env POSTGRES_PASSWORD --env POSTGRES_DB=fleet \
    --publish '127.0.0.1::5432' --tmpfs /var/lib/postgresql/data "$image" >/dev/null \
    || die 'PostgreSQL fixture could not start'
created=1

deadline=$((SECONDS + 30))
until docker exec "$name" pg_isready -h 127.0.0.1 -U postgres -d fleet >/dev/null 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || die 'PostgreSQL readiness timed out'
    sleep 0.2
done
port=$(docker port "$name" 5432/tcp | head -n 1 | sed 's/.*://' | tr -d '\r')

cd "$root"
LINEA_FLEET_TEST_DATABASE_URL="postgres://postgres:$POSTGRES_PASSWORD@127.0.0.1:$port/fleet?sslmode=disable"
export LINEA_FLEET_TEST_DATABASE_URL
GOMAXPROCS=2
export GOMAXPROCS
if [ "$inventory_records" -gt 0 ]; then
    LINEA_FLEET_TEST_INVENTORY_RECORDS=$inventory_records
    export LINEA_FLEET_TEST_INVENTORY_RECORDS
    go test -p 1 -count 1 -v -run '^TestPostgresInventoryLatency$' ./internal/fleet/postgres \
        || die 'PostgreSQL lifecycle checks failed'
elif [ "$race" = 1 ]; then
    # The container reaches the host-bound fixture over the Docker network.
    ip=$(docker inspect "$name" --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' | tr -d '\r')
    LINEA_FLEET_TEST_DATABASE_URL="postgres://postgres:$POSTGRES_PASSWORD@$ip:5432/fleet?sslmode=disable"
    export LINEA_FLEET_TEST_DATABASE_URL
    docker run --rm --cpus 2 --memory 2g --mount "type=bind,source=$root,target=/src,readonly" \
        --workdir /src --env GOMAXPROCS=2 --env LINEA_FLEET_TEST_DATABASE_URL "$go_image" \
        go test -mod=readonly -race -p 1 -count 1 \
        -run '^TestPostgres(Lifecycle|Replicas|Inventory|ReapBudget|RemoteConfig|Configs|BootstrapToken)$' ./internal/fleet/postgres \
        || die 'PostgreSQL lifecycle checks failed'
else
    go test -p 1 -count 1 \
        -run '^TestPostgres(Lifecycle|Replicas|Inventory|ReapBudget|RemoteConfig|Configs|BootstrapToken)$' ./internal/fleet/postgres \
        || die 'PostgreSQL lifecycle checks failed'
fi
