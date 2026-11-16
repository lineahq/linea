#!/bin/bash
# Vets and tests every package whose test binary does not link the Collector
# registry: fleet, the plugin SDK, and the debugger packages that never run a
# pipeline. These fit a hosted CI runner; the rest stays in the local
# checklist ("Before a release" in docs/internals/development.md).
#
#   scripts/test-light.sh [--list]
#
#   --list   print the selected packages and exit
#
# Tests that need Docker or PostgreSQL skip without LINEA_DOCKER_TESTS or a
# fixture URL, as everywhere else.
set -euo pipefail
cd "$(dirname "$0")/.."
export GOMAXPROCS=${GOMAXPROCS:-2}

registry=github.com/lineahq/linea/internal/debugger/registry
# One listing: each package's test binary (pkg.test) with its dependencies;
# a package without test files has no binary and keeps its own dependencies.
packages=()
while read -r pkg links; do
    [ "$links" = false ] && packages+=("$pkg")
done < <(go list -test -f '{{if .ForTest}}{{else}}{{.ImportPath}} {{if eq .ImportPath "'"$registry"'"}}true{{end}}{{range .Deps}}{{if eq . "'"$registry"'"}}true{{end}}{{end}}{{end}}' ./... |
    awk 'NF { pkg = $1; sub(/\.test$/, "", pkg); if (!(pkg in seen)) order[++n] = pkg; seen[pkg] = 1; if ($2 == "true") linked[pkg] = 1 }
         END { for (i = 1; i <= n; i++) print order[i], (order[i] in linked) ? "true" : "false" }')

if [ "${1:-}" = --list ]; then
    printf '%s\n' "${packages[@]}"
    exit 0
fi
[ $# -eq 0 ] || { echo "unknown argument: $1" >&2; exit 2; }

echo "== ${#packages[@]} root-module packages without the Collector registry"
go vet "${packages[@]}"
go test -p 1 -count=1 "${packages[@]}"

echo '== pluginsdk'
go -C pluginsdk vet ./...
go -C pluginsdk test -count=1 ./...
