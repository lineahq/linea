#!/bin/bash
# Smoke test of the standalone fleet server with one real Collector: starts a
# built linea-fleet binary with in-memory storage, connects an unmodified
# Collector Contrib container over OpAMP and checks reporting, health, HTTP
# discovery/detail/filtering, role isolation and disconnect. The shell twin of
# smoke-collector.ps1. Needs Docker (Linux containers), curl and python3.
#
#   scripts/fleet/smoke-collector.sh [--binary PATH]
#
#   --binary PATH   the linea-fleet binary to run; default bin/linea-fleet
#                   (bin/linea-fleet.exe under Git Bash), built by
#                   scripts/fleet/check.sh
#   -h, --help      print this text
#
# The container reaches the fleet process, which listens for agents on all
# interfaces with --allow-insecure-remote, through host.docker.internal (mapped
# to the host gateway for Docker Engine on Linux). Management stays on
# loopback. This local smoke is not a deployment configuration.
set -u
export MSYS_NO_PATHCONV=1
winpath() { (cd "$1" && { pwd -W 2>/dev/null || pwd; }); }
root=$(winpath "$(dirname "$0")/../..")

usage() { awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; }

binary=
while [ $# -gt 0 ]; do
    case $1 in
        --binary) [ $# -ge 2 ] || { usage >&2; exit 2; }; binary=$2; shift;;
        -h|--help) usage; exit 0;;
        *) echo "unknown argument: $1" >&2; usage >&2; exit 2;;
    esac
    shift
done
if [ -z "$binary" ]; then
    binary=$root/bin/linea-fleet
    [ -e "$binary" ] || binary=$root/bin/linea-fleet.exe
fi
[ -f "$binary" ] || { echo "linea-fleet binary not found: $binary (build it with scripts/fleet/check.sh)" >&2; exit 1; }
python=$(command -v python3 || command -v python || true)
[ -n "$python" ] || { echo 'python3 is required' >&2; exit 1; }

run_id=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
container="linea-fleet-smoke-$run_id"
log_dir=$root/.local/$run_id
mkdir -p "$log_dir"
pid=
container_created=0
main=$$
trap 'exit 1' USR1
fail() { echo "FAIL: $*" >&2; kill -USR1 "$main"; exit 1; }
cleanup() {
    if [ "$container_created" = 1 ]; then docker rm --force "$container" >/dev/null 2>&1; fi
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
        kill "$pid" 2>/dev/null
        wait "$pid" 2>/dev/null
    fi
    rm -f "$log_dir"/*.header
}
trap cleanup EXIT
trap 'exit 1' INT TERM

json() { "$python" -c "import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1"; }
new_port() {
    "$python" -c "import socket; s = socket.socket(); s.bind(('127.0.0.1', 0)); print(s.getsockname()[1]); s.close()"
}
wait_for() { # wait_for DESCRIPTION COMMAND... (30 seconds, as the fleet process may exit early)
    local what=$1 deadline=$((SECONDS + 30))
    shift
    while [ "$SECONDS" -lt "$deadline" ]; do
        kill -0 "$pid" 2>/dev/null || fail "Fleet process exited; inspect $log_dir"
        if "$@"; then return 0; fi
        sleep 0.2
    done
    fail "$what"
}
status() { # status URL [HEADER-FILE] -> HTTP status code
    if [ -n "${2:-}" ]; then
        curl -s -o /dev/null -w '%{http_code}' --max-time 6 -H "@$2" "$1"
    else
        curl -s -o /dev/null -w '%{http_code}' --max-time 6 "$1"
    fi
}
get() { curl -s --max-time 6 -H "@$admin_header" "$1"; }

LINEA_FLEET_API_TOKEN=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
LINEA_FLEET_AGENT_TOKEN=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
export LINEA_FLEET_API_TOKEN LINEA_FLEET_AGENT_TOKEN
management_port=$(new_port)
agent_port=$(new_port)
while [ "$agent_port" = "$management_port" ]; do agent_port=$(new_port); done
base_url=http://127.0.0.1:$management_port
LINEA_FLEET_OPAMP_ENDPOINT=ws://host.docker.internal:$agent_port/v1/opamp
export LINEA_FLEET_OPAMP_ENDPOINT
admin_header=$log_dir/admin.header
agent_header=$log_dir/agent.header
printf 'Authorization: Bearer %s' "$LINEA_FLEET_API_TOKEN" > "$admin_header"
printf 'Authorization: Bearer %s' "$LINEA_FLEET_AGENT_TOKEN" > "$agent_header"

"$binary" --storage memory --server-id collector-smoke --management-listen "127.0.0.1:$management_port" \
    --agent-listen "0.0.0.0:$agent_port" --allow-insecure-remote \
    > "$log_dir/stdout.log" 2> "$log_dir/stderr.log" &
pid=$!
ready() { [ "$(status "$base_url/readyz")" = 200 ]; }
wait_for 'Fleet readiness timed out' ready

[ "$(get "$base_url/api/v1/server" | json 'd["storage"] == "memory" and len(d["plugins"]) == 0')" = True ] \
    || fail 'Unexpected server discovery'
anonymous=$(status "$base_url/api/v1/agents")
wrong_role=$(status "$base_url/api/v1/agents" "$agent_header")
{ [ "$anonymous" = 401 ] && [ "$wrong_role" = 401 ]; } || fail 'Management auth isolation failed'
wrong_agent=$(status "http://127.0.0.1:$agent_port/v1/opamp" "$admin_header")
[ "$wrong_agent" = 401 ] || fail 'Agent auth isolation failed'

config=$root/examples/fleet-collector.yaml
image='otel/opentelemetry-collector-contrib:0.162.0'
docker run --detach --name "$container" --add-host host.docker.internal:host-gateway --cpus 1 --memory 256m \
    --env LINEA_FLEET_AGENT_TOKEN --env LINEA_FLEET_OPAMP_ENDPOINT \
    --mount "type=bind,source=$config,target=/etc/otelcol-contrib/config.yaml,readonly" "$image" >/dev/null \
    || fail 'Collector container could not start'
container_created=1
reporting() {
    [ "$(get "$base_url/api/v1/agents?connected=true&limit=1" | json "len(d['items']) == 1 and d['items'][0].get('health') is not None")" = True ]
}
wait_for 'Collector reporting timed out' reporting
expected_id=$(get "$base_url/api/v1/agents?connected=true&limit=1" | json "d['items'][0]['id']")
[ "$(get "$base_url/api/v1/agents/$expected_id" | json "bool(d['connected']) and bool(d['health']['healthy'])")" = True ] \
    || fail 'Collector did not report healthy'
docker stop --time 5 "$container" >/dev/null || fail 'Collector stop failed'
disconnected() {
    [ "$(get "$base_url/api/v1/agents/$expected_id" | json "not d['connected']")" = True ]
}
wait_for 'Collector disconnect was not observed' disconnected
digest=$(docker image inspect "$image" --format '{{index .RepoDigests 0}}')
echo 'PASS: standalone Collector reporting, health, HTTP discovery/detail/filtering, role isolation, and disconnect.'
echo "Collector: $digest"
