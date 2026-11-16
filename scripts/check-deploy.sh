#!/bin/bash
# Lints the Helm chart and renders it with fleet off and on, and resolves
# every Compose file with each of its overlays. Needs helm and docker compose;
# no secrets, images or cluster. CI runs it; run it after changing deploy/.
#
#   scripts/check-deploy.sh
set -euo pipefail
cd "$(dirname "$0")/.."

# The chart refuses to render without a store; the bundled PostgreSQL and
# SeaweedFS are the smallest complete values.
chart=deploy/charts/linea
bundled=(--set postgresql.enabled=true --set seaweedfs.enabled=true --set networkPolicy.enabled=false)
echo "== helm lint $chart"
helm lint --strict "$chart" "${bundled[@]}"
echo "== helm template $chart"
helm template linea "$chart" "${bundled[@]}" >/dev/null
helm template linea "$chart" "${bundled[@]}" --set fleet.deploy.enabled=true >/dev/null

# Missing secret files do not fail `config`; only the YAML and its
# interpolation are checked.
compose() {
    local args=()
    for f in "$@"; do args+=(-f "$f"); done
    echo "== docker compose ${args[*]} config"
    docker compose "${args[@]}" config -q
}
compose deploy/compose/linea/compose.yaml
compose deploy/compose/linea/compose.yaml deploy/compose/linea/compose.fleet.yaml
compose deploy/compose/fleet/compose.yaml
compose deploy/compose/fleet/compose.yaml deploy/compose/fleet/compose.ha.yaml
compose deploy/compose/fleet/compose.yaml deploy/compose/fleet/compose.livecapture.yaml
compose deploy/compose/fleet/compose.yaml deploy/compose/fleet/compose.supervised.yaml
compose deploy/compose/collector/compose.yaml
compose deploy/compose/collector/compose.yaml deploy/compose/collector/compose.lab.yaml
compose deploy/compose/collector/compose.yaml deploy/compose/collector/compose.perf.yaml
