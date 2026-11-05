#!/bin/sh
# Installs the pinned OpenTelemetry Collector builder (OCB) into .tools/bin of
# the repository, the shell twin of install-tools.ps1, and prints its version.
#
#   scripts/install-tools.sh
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
bin=$root/.tools/bin

case "${1:-}" in
    '') ;;
    -h|--help) echo 'usage: scripts/install-tools.sh'; exit 0;;
    *) echo "unknown argument: $1" >&2; exit 2;;
esac

mkdir -p "$bin"
GOBIN=$bin go install go.opentelemetry.io/collector/cmd/builder@v0.162.0
"$bin/builder" version
