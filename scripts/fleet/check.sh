#!/bin/sh
# Focused fleet checks, the shell twin of check.ps1: gofmt, package tests, vet,
# the fleet and simulator builds (bin/linea-fleet, bin/agent-simulator) and a
# whitespace check. Runs with GOMAXPROCS=2 and one package at a time.
#
#   scripts/fleet/check.sh
set -eu
root=$(cd "$(dirname "$0")/../.." && pwd)

case "${1:-}" in
    '') ;;
    -h|--help) echo 'usage: scripts/fleet/check.sh'; exit 0;;
    *) echo "unknown argument: $1" >&2; exit 2;;
esac

cd "$root"
GOMAXPROCS=2
export GOMAXPROCS
exe=$(go env GOEXE)

unformatted=$(gofmt -l cmd/linea-fleet cmd/linea-fleet-simulator internal/fleet pluginsdk) \
    || { echo 'gofmt failed' >&2; exit 1; }
if [ -n "$unformatted" ]; then
    echo "Run gofmt on: $unformatted" >&2
    exit 1
fi
packages='./internal/fleet/fleet ./internal/fleet/inventory ./internal/fleet/auth ./pluginsdk ./pluginsdk/metrics ./internal/fleet/plugins ./internal/fleet/api ./internal/fleet/gateway ./internal/fleet/app ./internal/fleet/postgres ./internal/fleet/agentsimulator'
# shellcheck disable=SC2086  # the package list is a fixed, space-free word list
go test -p 1 $packages || { echo 'Package tests failed' >&2; exit 1; }
# shellcheck disable=SC2086
go vet -p 1 ./cmd/linea-fleet $packages || { echo 'Go vet failed' >&2; exit 1; }
go build -p 1 -o "bin/linea-fleet$exe" ./cmd/linea-fleet || { echo 'Build failed' >&2; exit 1; }
go vet -p 1 ./cmd/linea-fleet-simulator || { echo 'Simulator vet failed' >&2; exit 1; }
go build -p 1 -o "bin/agent-simulator$exe" ./cmd/linea-fleet-simulator || { echo 'Simulator build failed' >&2; exit 1; }
git diff --check || { echo 'Whitespace check failed' >&2; exit 1; }
