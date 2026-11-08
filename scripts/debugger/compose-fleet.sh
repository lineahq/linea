#!/usr/bin/env bash
# Starts the debugger Compose stack with fleet and the live-capture plugin on
# its PostgreSQL (deploy/compose/linea/compose.fleet.yaml). Run it the first
# time and after every upgrade; it is safe to repeat:
#   1. creates the fleet and livecapture databases and roles (idempotent);
#   2. stops fleet and the plugin and migrates both schemas (migrations stay
#      explicit, and the plugin refuses to migrate while it runs);
#   3. installs linea-server's fleet token, generated once and never printed,
#      as the first fleet admin (no change once it is);
#   4. starts the whole stack.
# --build builds the fleet and plugin images first. Set LINEA_VERSION as for
# the stack itself (dev with --build to build from source).
set -euo pipefail
export MSYS_NO_PATHCONV=1
dir="$(cd "$(dirname "$0")/../../deploy/compose/linea" && { pwd -W 2>/dev/null || pwd; })"
compose() { docker compose --project-directory "$dir" -f "$dir/compose.yaml" -f "$dir/compose.fleet.yaml" "$@"; }
case "${1:-}" in
  --build) compose build fleet livecapture-plugin ;;
  '') ;;
  *) echo "usage: $0 [--build]" >&2; exit 2 ;;
esac

echo '== databases and roles'
compose run --rm -T fleet-db

echo '== migrations'
compose stop livecapture-plugin fleet >/dev/null 2>&1 || true
compose run --rm -T fleet-migrate
compose run --rm -T capture-migrate

echo '== fleet admin'
compose run --rm -T fleet-bootstrap

echo '== stack'
compose up -d
echo "Debugger: http://127.0.0.1:${LINEA_PORT:-7070}  Fleet API: http://127.0.0.1:${LINEA_FLEET_PORT:-8080}/api/v1"
