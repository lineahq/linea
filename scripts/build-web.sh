#!/bin/sh
# Installs the locked web dependencies, regenerates the API types, runs the web
# tests and builds the web client, the shell twin of build-web.ps1. With
# --binary it also builds the linea-server binary with the client embedded
# (dist/linea-server, dist/linea-server.exe under Git Bash on Windows).
#
#   scripts/build-web.sh [--binary]
#
#   --binary      also build the linea-server binary with the web client embedded
#   -h, --help    print this text
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)

usage() { awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; }
binary=0
while [ $# -gt 0 ]; do
    case $1 in
        --binary) binary=1;;
        -h|--help) usage; exit 0;;
        *) echo "unknown argument: $1" >&2; usage >&2; exit 2;;
    esac
    shift
done

(
    cd "$root/web"
    # The lockfile pins every package; never resolve new versions here.
    pnpm install --frozen-lockfile
    # Regenerate the TypeScript API types from the committed OpenAPI contract.
    pnpm gen:api
    pnpm lint
    pnpm test
    # Type-checks, then writes internal/debugger/webui/static/app (ignored by git).
    pnpm build
)

if [ "$binary" = 1 ]; then
    cd "$root"
    go build -o "$root/dist/linea-server$(go env GOEXE)" ./cmd/linea-server
fi
