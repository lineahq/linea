#!/bin/bash
# Livecapture plugin database checks against one disposable, resource-bounded
# PostgreSQL container (1 CPU, 256 MB, 128 PIDs, tmpfs data, random loopback
# port, random password), removed on exit. The shell twin of test-postgres.ps1.
#
#   scripts/fleet/livecapture/test-postgres.sh [--race]
#
#   --race       run the check under the race detector in a bounded golang
#                container (2 CPUs, 2 GB, read-only mount of the checkout)
#                that reaches the fixture over the Docker network
#   -h, --help   print this text
set -euo pipefail
export MSYS_NO_PATHCONV=1
winpath() { (cd "$1" && { pwd -W 2>/dev/null || pwd; }); }
root=$(winpath "$(dirname "$0")/../../..")

usage() { awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; }
die() { echo "$*" >&2; exit 1; }

race=0
while [ $# -gt 0 ]; do
    case $1 in
        --race) race=1;;
        -h|--help) usage; exit 0;;
        *) echo "unknown argument: $1" >&2; usage >&2; exit 2;;
    esac
    shift
done

random_hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }
fixture="capture-pg-test-$(random_hex 16)"
image='postgres:17@sha256:b8faeff7712663c2773a190181e1246e37e364b887611c04658c28b44e2408fe'
go_image='golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9'
created=0
cleanup() {
    if [ "$created" = 1 ]; then docker rm --force "$fixture" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT
trap 'exit 1' INT TERM

POSTGRES_PASSWORD=$(random_hex 32)
export POSTGRES_PASSWORD
docker run --detach --name "$fixture" --cpus 1 --memory 256m --pids-limit 128 \
    --env POSTGRES_PASSWORD --env POSTGRES_DB=capture_fixture \
    --publish '127.0.0.1::5432' --tmpfs /var/lib/postgresql/data "$image" >/dev/null \
    || die 'Disposable capture database startup failed'
created=1

deadline=$((SECONDS + 30))
until docker exec "$fixture" pg_isready -h 127.0.0.1 -U postgres -d capture_fixture >/dev/null 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || die 'Capture fixture readiness failed'
    sleep 0.2
done
port=$(docker port "$fixture" 5432/tcp | head -n 1 | sed 's/.*://' | tr -d '\r')

OTEL_CAPTURE_TEST_DATABASE_URL="postgres://postgres:$POSTGRES_PASSWORD@127.0.0.1:$port/capture_fixture?sslmode=disable"
export OTEL_CAPTURE_TEST_DATABASE_URL
GOMAXPROCS=2
export GOMAXPROCS
if [ "$race" = 1 ]; then
    ip=$(docker inspect "$fixture" --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' | tr -d '\r')
    OTEL_CAPTURE_TEST_DATABASE_URL="postgres://postgres:$POSTGRES_PASSWORD@$ip:5432/capture_fixture?sslmode=disable"
    export OTEL_CAPTURE_TEST_DATABASE_URL
    docker run --rm --cpus 2 --memory 2g --mount "type=bind,source=$root,target=/src,readonly" \
        --workdir /src --env GOMAXPROCS=2 --env OTEL_CAPTURE_TEST_DATABASE_URL "$go_image" \
        go test -mod=readonly -race -p 1 -count=1 -run '^TestPostgresReservationRecoveryAndGrants$' \
        ./internal/fleetplugins/livecapture \
        || die 'Capture database checks failed'
else
    (cd "$root" && go test -p 1 -count=1 -run '^TestPostgresReservationRecoveryAndGrants$' \
        ./internal/fleetplugins/livecapture) \
        || die 'Capture database checks failed'
fi
