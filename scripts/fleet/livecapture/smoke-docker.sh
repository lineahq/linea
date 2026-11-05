#!/bin/bash
# Docker smoke of the livecapture plugin with the real, unchanged Collector
# distribution, the shell twin of smoke-docker.ps1. It builds the Collector,
# fleet, plugin, consumer and generator images (unless --skip-build), starts a
# disposable Compose project (own PostgreSQL databases, private secrets,
# bounded containers, fixed fixture subnets) and checks: TLS/bearer peers and
# catalogs, the public API and its pagination, private metrics scopes, a
# generic consumer receiving filtered and redacted telemetry, ingress identity
# and bounds, idempotency, overlap and raw-capture denial, partial receipts,
# lost-consumer backpressure, absolute expiry, generic lifecycle drain/disable,
# competing-authority denial, narrow storage grants, API-key revocation, core
# and plugin crash recovery without replay, shared-credential revocation, a
# metadata restore into a fresh database and database-outage isolation.
# Needs Docker Compose v2 (Linux containers), curl and python3. It takes
# minutes and builds the Collector from source: do not run it on a shared or
# constrained machine.
#
#   scripts/fleet/livecapture/smoke-docker.sh [--skip-build] [--image-tag TAG] [--network-octet N]
#
#   --skip-build        reuse the images of a previous run
#   --image-tag TAG     tag of the fleet, plugin, consumer and generator
#                       images; lowercase letters, digits, '.' and '-';
#                       default local
#   --network-octet N   second octet of the 10.N.240.0 fixture subnets, 1-254;
#                       default 244
#   -h, --help          print this text
#
# Container logs are kept in .local/capture-s4-<id>/; the Compose project and
# its volumes are removed on exit.
set -u -o pipefail
export MSYS_NO_PATHCONV=1
winpath() { (cd "$1" && { pwd -W 2>/dev/null || pwd; }); }
root=$(winpath "$(dirname "$0")/../../..")

usage() { awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; }
bad_usage() { echo "$*" >&2; usage >&2; exit 2; }

skip_build=0
image_tag='local'
octet=244
while [ $# -gt 0 ]; do
    case $1 in
        --skip-build) skip_build=1;;
        --image-tag) [ $# -ge 2 ] || bad_usage "$1 needs a value"; image_tag=$2; shift;;
        --network-octet) [ $# -ge 2 ] || bad_usage "$1 needs a value"; octet=$2; shift;;
        -h|--help) usage; exit 0;;
        *) bad_usage "unknown argument: $1";;
    esac
    shift
done
printf '%s' "$image_tag" | grep -Eq '^[a-z0-9][a-z0-9.-]{0,40}$' \
    || bad_usage '--image-tag must match ^[a-z0-9][a-z0-9.-]{0,40}$'
case $octet in ''|*[!0-9]*) bad_usage '--network-octet must be an integer from 1 to 254';; esac
{ [ "$octet" -ge 1 ] && [ "$octet" -le 254 ]; } || bad_usage '--network-octet must be an integer from 1 to 254'
python=$(command -v python3 || command -v python || true)
[ -n "$python" ] || { echo 'python3 is required' >&2; exit 1; }

random_hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }
id=$(random_hex 16)
project="capture-s4-$id"
run=$root/.local/$project
env_file=$run/compose.env
postgres_image='postgres:17@sha256:b8faeff7712663c2773a190181e1246e37e364b887611c04658c28b44e2408fe'
consumer_image="linea-fleet-livecapture-consumer:$image_tag"
generator_image="linea-fleet-livecapture-generator:$image_tag"
collector_image='linea-fleet-livecapture-compat:collector'
created=0
children=()
consumer_sequence=0
consumer_name=
admin=
base=
tmp=$(mktemp -d "${TMPDIR:-/tmp}/linea-capture-smoke-XXXXXX")
main=$$
trap 'exit 1' USR1 INT TERM

compose_args=(--project-name "$project" --env-file "$env_file"
    --file "$root/deploy/compose/fleet/compose.yaml"
    --file "$root/deploy/compose/fleet/compose.livecapture.yaml"
    --file "$run/observe.yaml")

fail() { echo "FAIL: $*" >&2; kill -USR1 "$main"; exit 1; }
cleanup() {
    local rc=$? name
    trap - EXIT USR1 INT TERM
    if [ -d "$run" ]; then
        for name in ${children[@]+"${children[@]}"}; do
            docker logs "$name" > "$run/$name.log" 2>&1
            docker rm --force "$name" >/dev/null 2>&1
        done
    fi
    if [ "$created" = 1 ]; then
        docker compose "${compose_args[@]}" down --volumes --remove-orphans --timeout 5
    fi
    rm -rf "$tmp"
    exit "$rc"
}
trap cleanup EXIT

json() { "$python" -c "import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1" 2>/dev/null; }
iso_in() { # iso_in SECONDS -> UTC timestamp that far in the future
    "$python" -c 'import datetime, sys; print((datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=float(sys.argv[1]))).strftime("%Y-%m-%dT%H:%M:%S.%fZ"))' "$1"
}
compose() { docker compose "${compose_args[@]}" "$@" || fail 'S4 Compose operation failed (sensitive details omitted)'; }
wait_for() { # wait_for SECONDS DESCRIPTION COMMAND...
    local deadline=$((SECONDS + $1)) what=$2
    shift 2
    while :; do
        if "$@"; then return 0; fi
        [ "$SECONDS" -lt "$deadline" ] || fail "$what"
        sleep 0.25
    done
}
host_port() { docker port "$1" "$2/tcp" | head -n 1 | sed 's/.*://' | tr -d '\r'; }

# http_try METHOD PATH TOKEN [BODY] [EXPECTED] [IF-MATCH] -> body (nothing for 204).
# Returns 1, with a message, when the status differs. The token goes to curl
# on standard input, never its arguments.
http_try() {
    local method=$1 path=$2 token=$3 body=${4:-} expected=${5:-200} match=${6:-} code
    local args=(-s -o "$tmp/resp" -w '%{http_code}' -X "$method" --max-time 6)
    if [ -n "$token" ]; then args+=(-K -); fi
    if [ -n "$match" ]; then args+=(-H "If-Match: $match"); fi
    if [ -n "$body" ]; then args+=(-H 'Content-Type: application/json' --data "$body"); fi
    if [ -n "$token" ]; then
        code=$(printf 'header = "Authorization: Bearer %s"\n' "$token" | curl "${args[@]}" "$base$path")
    else
        code=$(curl "${args[@]}" "$base$path")
    fi
    if [ "$code" != "$expected" ]; then
        echo "S4 $method $path returned $code, expected $expected (body omitted)" >&2
        return 1
    fi
    if [ "$expected" != 204 ]; then cat "$tmp/resp"; fi
}
http() { http_try "$@" || fail 'request assertion failed'; }
# capi_try / capi METHOD PATH [BODY] [EXPECTED] [TOKEN] [IF-MATCH]: the plugin's public API.
capi_try() { http_try "$1" "/api/v1/plugins/livecapture/api/$2" "${5-$admin}" "${3:-}" "${4:-200}" "${6:-}"; }
capi() { capi_try "$@" || fail 'request assertion failed'; }
# capi_is PATH EXPR [TOKEN]: the API answers 200 and EXPR over the body d is True.
capi_is() { [ "$(capi_try GET "$1" '' 200 "${3-$admin}" 2>/dev/null | json "$2")" = True ]; }
core_is() { [ "$(http_try GET "$1" "$admin" 2>/dev/null | json "$2")" = True ]; }
psql_plugin() { docker exec "$plugin_pg" psql -U postgres -d livecapture -At -c "$1" | tr -d '\r'; }
op_state_is() { # op_state_is ID STATE...
    local state want
    state=$(psql_plugin "SELECT state FROM capture.operations WHERE id='$1'")
    shift
    for want in "$@"; do [ "$state" = "$want" ] && return 0; done
    return 1
}
no_live_leases() { [ "$(psql_plugin 'SELECT count(*) FROM capture.leases WHERE expires_at>now()')" = 0 ]; }

# body_with BODY 'PYTHON STATEMENTS ON b': the capture request with changes applied.
body_with() {
    "$python" -c 'import json, sys; b = json.loads(sys.argv[1]); exec(sys.argv[2]); print(json.dumps(b))' "$1" "$2"
}
# capture_body CHANNEL PEERS-JSON-ARRAY [TTL=25] [FILTERED=1]
capture_body() {
    "$python" -c '
import datetime, json, sys, uuid
channel, peers, ttl, filtered = sys.argv[1], json.loads(sys.argv[2]), float(sys.argv[3]), sys.argv[4] == "1"
flt = {}
if filtered:
    flt = {"traces": {"span": ["status.code == STATUS_CODE_ERROR"]},
           "logs": {"log": ["severity_number >= SEVERITY_NUMBER_ERROR"]},
           "metrics": {"datapoint": ["attributes[\"keep\"] == true"]}}
expires = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=ttl)).strftime("%Y-%m-%dT%H:%M:%S.%fZ")
print(json.dumps({"authority_id": peers[0]["authority_id"], "plugin_instance": peers[0]["plugin_instance"],
    "id": uuid.uuid4().hex, "channel_id": channel,
    "targets": [{"peer_id": p["id"], "generation": p["generation"]} for p in peers],
    "capture_points": [{"pipeline": "traces", "point": "egress"}, {"pipeline": "logs", "point": "egress"},
                       {"pipeline": "metrics", "point": "egress"}],
    "signals": ["traces", "logs", "metrics"], "filter": flt, "limits": {}, "expires_at": expires}))' \
        "$1" "$2" "${3:-25}" "${4:-1}"
}
first_peer() { printf '%s' "$1" | json 'json.dumps(d[:1])'; }
peers_array() { capi GET v1/peers | json 'json.dumps(d["items"])'; }

new_channel() { capi POST v1/channels "{\"expires_at\":\"$(iso_in 110)\"}" 201 "${1-$admin}"; }
channel_id_of() { printf '%s' "$1" | json 'd["id"]'; }
# start_consumer CHANNEL TOKEN-FILE [RATIO=1]: leaves the container name in $consumer_name.
start_consumer() {
    consumer_sequence=$((consumer_sequence + 1))
    consumer_name="$project-consumer-$consumer_sequence"
    docker run --detach --name "$consumer_name" --network "${project}_ingress" --cpus 1 --memory 128m --pids-limit 64 \
        --read-only --cap-drop ALL --security-opt no-new-privileges \
        --mount "type=bind,source=$2,target=/consumer_token,readonly" "$consumer_image" \
        --endpoint http://fleet:8080/api/v1/plugins/livecapture/api --allow-insecure-http --token-file /consumer_token \
        --channel "$1" --duration 100s --accept-ratio "${3:-1}" --verify-redaction >/dev/null \
        || fail 'Independent generic consumer startup failed'
    children+=("$consumer_name")
    consumer_ready() { docker logs "$consumer_name" 2>&1 | grep -q consumer_ready; }
    wait_for 40 'Generic consumer did not start' consumer_ready
}
# counts_match CONTAINER PY-EXPRESSION: the consumer's last counts object c satisfies the expression.
counts_match() {
    docker logs "$1" 2>&1 | "$python" -c '
import json, sys
last = None
for line in sys.stdin:
    try:
        v = json.loads(line)
    except Exception:
        continue
    if isinstance(v, dict) and v.get("records"):
        last = v
sys.exit(0 if last is not None and eval(sys.argv[1], {"c": last}) else 1)' "$2"
}
each_signal() { # each_signal N: the consumer received N records of every signal
    echo "c['records'].get('traces') == $1 and c['records'].get('logs') == $1 and c['records'].get('metrics') == $1"
}
capture_active() { # capture_active ID [TOKEN]
    wait_for 40 'Collector did not acknowledge activation' capi_is "v1/captures/$1" 'd["state"] == "active"' "${2-$admin}"
}
capture_stop() {
    local current revision
    current=$(capi GET "v1/captures/$1")
    revision=$(printf '%s' "$current" | json 'd["revision"]')
    capi POST "v1/captures/$1/stop" '' 202 "$admin" "\"$revision\"" >/dev/null
    wait_for 40 'Collector did not acknowledge stop' capi_is "v1/captures/$1" 'd["state"] in ("stopped", "completed")'
}
generate() {
    docker run --rm --network "${project}_capture-ingress" --cpus 1 --memory 128m --pids-limit 64 --read-only --cap-drop ALL \
        --security-opt no-new-privileges "$generator_image" --endpoint "$1:4317" || fail 'Synthetic OTLP input generation failed'
}
# ingress_refusal TOKEN-FILE CODE [SERVER-NAME] [OVERSIZE=0]
ingress_refusal() {
    local extra=()
    if [ -n "${3:-}" ]; then extra+=(--server-name "$3"); fi
    if [ "${4:-0}" = 1 ]; then extra+=(--oversize); fi
    docker run --rm --network "${project}_capture-ingress" --cpus 1 --memory 128m --pids-limit 64 --read-only --cap-drop ALL \
        --security-opt no-new-privileges --mount "type=bind,source=$run/plugin/tls_cert.pem,target=/certs/ca.pem,readonly" \
        --mount "type=bind,source=$1,target=/fixture_token,readonly" "$generator_image" \
        --endpoint livecapture-plugin:4341 --tls-ca /certs/ca.pem --token-file /fixture_token --expect-code "$2" \
        ${extra[@]+"${extra[@]}"} || fail 'Capture ingress identity/bounds check failed'
}
write_token_file() { # write_token_file PATH TOKEN
    printf '%s' "$2" > "$1"
    chmod 444 "$1"
}
start_collector() { # start_collector POD
    local name="$project-$1"
    docker run --detach --init --name "$name" --network "${project}_capture-ingress" --network-alias "$1" --cpus 2 --memory 512m \
        --pids-limit 128 --read-only --cap-drop ALL --security-opt no-new-privileges \
        --mount "type=bind,source=$root/examples/fleet-livecapture/collector.yaml,target=/fixture.yaml,readonly" \
        --mount "type=bind,source=$run/plugin/tls_cert.pem,target=/certs/ca.pem,readonly" \
        --env LINEA_FLEET_AGENT_TOKEN --env "POD_NAME=$1" -e GOMAXPROCS=2 "$collector_image" --config /fixture.yaml >/dev/null \
        || fail 'Real Collector startup failed'
    children+=("$name")
}
two_ready_peers() { capi_is v1/peers 'len(d["items"]) == 2 and all(len(i["identity"]["capture_points"]) == 6 for i in d["items"])'; }

sh "$root/scripts/fleet/new-plugin-secrets.sh" "$run" >/dev/null || fail 'Plugin secrets could not be created'
sh "$root/scripts/fleet/new-deployment-secrets.sh" "$run/fleet" >/dev/null || fail 'Deployment secrets could not be created'
sh "$root/scripts/fleet/livecapture/new-secrets.sh" "$run/plugin" >/dev/null || fail 'Capture secrets could not be created'
"$python" - "$run/observe.yaml" "$octet" <<'PYEOF' || fail 'Fixture network override could not be generated'
import json, sys
path, octet = sys.argv[1], int(sys.argv[2])
networks, subnet = {}, 0
for name in ["database", "ingress", "plugins", "capture-database", "capture-ingress"]:
    networks[name] = {"ipam": {"config": [{"subnet": "10.%d.240.%d/28" % (octet, subnet)}]}}
    subnet += 16
with open(path, "w") as f:
    f.write(json.dumps({"services": {"livecapture-plugin": {"ports": ["127.0.0.1::8090"]}}, "networks": networks}, indent=2))
PYEOF
printf "FLEET_SECRET_DIR='%s/fleet'\nFLEET_LIVECAPTURE_SECRET_DIR='%s/plugin'\nFLEET_SERVER_ID=s4-%s\nFLEET_API_PORT=0\nFLEET_AGENT_PORT=0\nCAPTURE_CONTROL_PORT=0\nCAPTURE_DELIVERY_PORT=0\nFLEET_VERSION=%s\nCAPTURE_VERSION=%s\n" \
    "$run" "$run" "$id" "$image_tag" "$image_tag" > "$env_file"

if [ "$skip_build" = 0 ]; then
    for target in "runtime linea-fleet-livecapture-compat:collector" "capture-smoke linea-fleet-livecapture-compat:capture-smoke"; do
        # shellcheck disable=SC2086  # "<target> <tag>" is two words by construction
        set -- $target
        docker build --file "$root/deploy/docker/linea-collector.Dockerfile" --target "$1" --tag "$2" "$root" \
            || fail 'Real unchanged Collector build failed'
    done
    compose build fleet
    compose build livecapture-plugin
    docker build --file "$root/deploy/docker/linea-fleet-livecapture.Dockerfile" --target client --tag "$consumer_image" "$root" \
        || fail 'Generic client or synthetic generator build failed'
    docker build --file "$root/deploy/docker/linea-fleet-livecapture.Dockerfile" --target fixture --tag "$generator_image" "$root" \
        || fail 'Generic client or synthetic generator build failed'
fi
baseline=$(docker run --rm --network none --cpus 2 --memory 512m --pids-limit 128 --read-only --cap-drop ALL \
    --security-opt no-new-privileges -e GOMAXPROCS=2 linea-fleet-livecapture-compat:capture-smoke 2>&1) \
    || fail 'Unchanged Collector baseline failed (logs omitted)'
printf '%s' "$baseline" | grep -q 'Capture smoke passed:' || fail 'Unchanged Collector baseline failed (logs omitted)'
created=1
compose up --detach --wait postgres capture-postgres
compose run --rm -T migrate >/dev/null
compose run --rm -T capture-migrate >/dev/null
admin=$(compose run --rm -T bootstrap | tr -d '\r' | json 'd["token"]')
[ -n "$admin" ] || fail 'Disposable fleet bootstrap failed'
admin_file=$run/admin_token
write_token_file "$admin_file" "$admin"
compose up --detach --wait fleet
core=$(compose ps --quiet fleet)
base=http://127.0.0.1:$(host_port "$core" 8080)
[ "$(http GET /readyz '' | json 'd["status"]')" = ready ] || fail 'Core failed without plugin'
[ "$(http GET /api/v1/plugins/livecapture "$admin" | json 'd["state"]')" = unavailable ] || fail 'Absent plugin advertised ready'
compose up --detach --wait livecapture-plugin
plugin=$(compose ps --quiet livecapture-plugin)
plugin_pg=$(compose ps --quiet capture-postgres)
fleet_pg=$(compose ps --quiet postgres)
capi GET v1/openapi.json '' 401 '' >/dev/null
[ "$(capi GET v1/openapi.json | json 'd["info"]["title"]')" = 'Optional livecapture plugin API' ] \
    || fail 'Plugin-owned schema was not routed opaquely'
# The unchanged extension presents a fleet agent credential whose capture
# label allows the s4 collector group; the plugin introspects it via core.
unlabeled=$(http POST /api/v1/credentials "$admin" \
    "{\"id\":\"$(random_hex 16)\",\"name\":\"s4 unlabeled\",\"role\":\"agent\",\"expires_at\":\"$(iso_in 86400)\"}" 201 | json 'd["token"]')
unlabeled_file=$run/unlabeled_token
write_token_file "$unlabeled_file" "$unlabeled"
credential=$(random_hex 16)
agent_token=$(http POST /api/v1/credentials "$admin" \
    "{\"id\":\"$credential\",\"name\":\"s4 collectors\",\"role\":\"agent\",\"expires_at\":\"$(iso_in 86400)\",\"labels\":{\"capture\":\"s4\"}}" 201 | json 'd["token"]')
agent_file=$run/agent_token
write_token_file "$agent_file" "$agent_token"
export LINEA_FLEET_AGENT_TOKEN=$agent_token
for pod in collector-a collector-b; do start_collector "$pod"; done
wait_for 40 'TLS/bearer capture peers or catalogs did not register' two_ready_peers
peers=$(peers_array)
page=$(capi GET 'v1/peers?limit=1')
cursor=$("$python" -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$(printf '%s' "$page" | json 'd["next_cursor"]')")
next_page=$(capi GET "v1/peers?limit=1&cursor=$cursor")
if ! { [ "$(printf '%s' "$page" | json 'len(d["items"])')" = 1 ] \
    && [ "$(printf '%s' "$next_page" | json 'len(d["items"])')" = 1 ] \
    && [ "$(printf '%s' "$page" | json 'd["items"][0]["id"]')" != "$(printf '%s' "$next_page" | json 'd["items"][0]["id"]')" ]; }; then
    fail 'Public peer catalog pagination failed'
fi
capi GET 'v1/peers?limit=1&limit=2' '' 400 >/dev/null
[ "$(capi GET v1/metrics | json 'd["gauges"]["peers"] == 2 and d["limits"]["control_connections"] == 272')" = True ] \
    || fail 'Public plugin metric limits/counts mismatch'
metrics_port=$(host_port "$plugin" 8090)
metrics_bearer=$(cat "$run/plugin/metrics_token")
scrape() { # scrape URL TOKEN -> body on a file, HTTP status on stdout
    printf 'header = "Authorization: Bearer %s"\n' "$2" | curl -s -K - -o "$tmp/scrape" -w '%{http_code}' "$1"
}
if ! { [ "$(scrape "http://127.0.0.1:$metrics_port/metrics" "$metrics_bearer")" = 200 ] \
    && grep -q 'linea_fleet_livecapture_peers 2' "$tmp/scrape"; }; then
    fail 'Private authenticated plugin scrape failed'
fi
[ "$(scrape "http://127.0.0.1:$metrics_port/metrics" "$admin")" = 401 ] || fail 'General API token crossed private metrics credential scope'
if ! { [ "$(scrape "$base/metrics" "$admin")" = 200 ] \
    && grep -q 'linea_fleet_database_owner_wait_seconds_count' "$tmp/scrape" \
    && grep -q 'linea_fleet_plugins{state="ready"} 1' "$tmp/scrape" \
    && ! grep -q 'linea_fleet_livecapture_' "$tmp/scrape"; }; then
    fail 'Core operational metrics missing or capture boundary crossed'
fi
echo 'PASS S6.1 public cursor pages, private/read-only scrape credentials, plugin effective limits and domain-free core metrics.'
[ "$(http GET /api/v1/agents "$admin" | json 'len(d["items"])')" = 0 ] || fail 'Capture peer identities were invented as OpAMP UIDs'
channel_id=$(channel_id_of "$(new_channel)")
start_consumer "$channel_id" "$admin_file"
consumer=$consumer_name
capture_request=$(capture_body "$channel_id" "$peers")
operation=$(capi POST v1/captures "$capture_request" 202)
operation_id=$(printf '%s' "$operation" | json 'd["id"]')
capture_active "$operation_id"
ingress_refusal "$run/plugin/host_token" Unauthenticated
ingress_refusal "$unlabeled_file" Unauthenticated
ingress_refusal "$agent_file" PermissionDenied
ingress_refusal "$agent_file" ResourceExhausted '' 1
ingress_refusal "$agent_file" Unavailable wrong-host
[ "$(capi POST v1/captures "$capture_request" 202 | json 'd["id"]')" = "$operation_id" ] || fail 'Identical create did not reconcile'
capi POST v1/captures "$(body_with "$capture_request" 'b["limits"] = {"total_records": 1}')" 409 >/dev/null
other_channel=$(channel_id_of "$(new_channel)")
capi POST v1/captures "$(capture_body "$other_channel" "$peers")" 409 >/dev/null
raw_request=$(body_with "$capture_request" 'import uuid; b["id"] = uuid.uuid4().hex; b["capture_points"] = [{"pipeline": "traces", "point": "ingress"}]; b["signals"] = ["traces"]')
# The same targets are busy above; raw authorization is tested after release.
generate collector-a
generate collector-b
wait_for 40 'Generic public consumer did not receive filtered redacted traces/logs/metrics from both Collectors' \
    counts_match "$consumer" "$(each_signal 2)"
capi POST "v1/captures/$operation_id/stop" '' 428 >/dev/null
capi POST "v1/captures/$operation_id/stop" '' 412 "$admin" '"999999"' >/dev/null
capture_stop "$operation_id"
docker stop --time 5 "$consumer" >/dev/null
capi POST v1/captures "$raw_request" 403 >/dev/null
echo 'PASS public API: real TLS/bearer peers, generic consumer receipts for all signals, OTTL filtering/redaction, fixed roster, overlap denial, raw denial, idempotency and acknowledged stop.'

# Restart the same real Collector: its capture identity remains, generation advances.
old_peer=$(printf '%s' "$peers" | json 'json.dumps([p for p in d if p["identity"]["pod"] == "collector-a"][:1])')
old_peer_id=$(printf '%s' "$old_peer" | json 'd[0]["id"]')
old_generation=$(printf '%s' "$old_peer" | json 'd[0]["generation"]')
docker restart "$project-collector-a" >/dev/null || fail 'Collector restart failed'
wait_for 40 'Real Collector generation did not advance' capi_is v1/peers \
    "len([p for p in d['items'] if p['id'] == '$old_peer_id' and p['generation'] > $old_generation and len(p['identity']['capture_points']) == 6]) == 1"
peers=$(peers_array)
fresh_channel=$(channel_id_of "$(new_channel)")
capi POST v1/captures "$(capture_body "$fresh_channel" "$old_peer")" 409 >/dev/null

# Partial receipts are returned as OTLP partial success, rather than full ACK.
start_consumer "$fresh_channel" "$admin_file" 0.5
partial_consumer=$consumer_name
first=$(first_peer "$peers")
first_pod=$(printf '%s' "$first" | json 'd[0]["identity"]["pod"]')
invalid_request=$(body_with "$(capture_body "$fresh_channel" "$first")" 'b["filter"]["traces"]["span"] = ["invalid OTTL expression ("]')
invalid_id=$(capi POST v1/captures "$invalid_request" 202 | json 'd["id"]')
wait_for 40 'Collector filter rejection was falsely reported active or lost' capi_is "v1/captures/$invalid_id" 'd["state"] == "failed"'
partial_id=$(capi POST v1/captures "$(capture_body "$fresh_channel" "$first" 25 0)" 202 | json 'd["id"]')
capture_active "$partial_id"
generate "$first_pod"
wait_for 40 'Consumer partial acceptance was not preserved' \
    counts_match "$partial_consumer" "c.get('accepted_records') == 3 and c.get('rejected_records') == 3"
capture_stop "$partial_id"
docker stop --time 5 "$partial_consumer" >/dev/null

# A channel without a polling/acknowledging consumer must never ACK OTLP.
lost_channel=$(channel_id_of "$(new_channel)")
lost_id=$(capi POST v1/captures "$(capture_body "$lost_channel" "$first" 12)" 202 | json 'd["id"]')
capture_active "$lost_id"
generate "$first_pod"
wait_for 40 'Lost consumer did not withdraw capture with visible uncertainty' capi_is "v1/captures/$lost_id" \
    'd["state"] in ("stopped", "expired") and d.get("reason") in ("receipt_uncertain", "consumer_unavailable")'

expiry_channel=$(channel_id_of "$(new_channel)")
start_consumer "$expiry_channel" "$admin_file"
expiry_consumer=$consumer_name
expiry_id=$(capi POST v1/captures "$(capture_body "$expiry_channel" "$first" 3)" 202 | json 'd["id"]')
capture_active "$expiry_id"
wait_for 40 'Absolute capture expiry was not enforced' capi_is "v1/captures/$expiry_id" 'd["state"] == "expired"'
docker stop --time 5 "$expiry_consumer" >/dev/null
# The service's expiry guard may conservatively retain a lease for five seconds.
wait_for 40 'Expiry guard did not elapse' no_live_leases
echo 'PASS reconnect generation, partial receipts, lost-consumer backpressure and absolute expiry.'
[ "$(capi GET v1/metrics | json 'd["counters"]["receipt_uncertain_total"] >= 1 and d["counters"]["consumer_rejected_traces_records_total"] >= 1')" = True ] \
    || fail 'Real delivery partial/uncertain outcomes not observable'

drain_channel=$(channel_id_of "$(new_channel)")
start_consumer "$drain_channel" "$admin_file"
drain_consumer=$consumer_name
drain_request=$(capture_body "$drain_channel" "$first" 20)
drain_id=$(capi POST v1/captures "$drain_request" 202 | json 'd["id"]')
capture_active "$drain_id"
plugin_revision() { http GET /api/v1/plugins/livecapture "$admin" | json 'd["revision"]'; }
http POST /api/v1/plugins/livecapture/lifecycle "$admin" '{"action":"drain"}' 200 "\"$(plugin_revision)\"" >/dev/null
capi GET v1/peers '' 503 >/dev/null
http POST /api/v1/plugins/livecapture/lifecycle "$admin" '{"action":"disable"}' 200 "\"$(plugin_revision)\"" >/dev/null
http GET /readyz '' >/dev/null
sleep 2
http POST /api/v1/plugins/livecapture/lifecycle "$admin" '{"action":"enable"}' 200 "\"$(plugin_revision)\"" >/dev/null
wait_for 40 'Plugin did not re-register after enable' core_is /api/v1/plugins/livecapture 'd["state"] == "ready"'
wait_for 40 'Plugin failed to withdraw capture during drain/disable' capi_is "v1/captures/$drain_id" 'd["state"] == "stopped"'
[ "$(capi POST v1/captures "$drain_request" 202 | json 'd["state"]')" = stopped ] || fail 'Lifecycle recovery replayed a capture'
docker stop --time 5 "$drain_consumer" >/dev/null
echo 'PASS generic lifecycle drain/disable withdraws real capture; re-enable does not replay it.'

# One plugin authority even if an operator accidentally starts another replica.
if docker compose "${compose_args[@]}" run --rm -T --no-deps livecapture-plugin >/dev/null 2>&1; then
    fail 'A competing plugin authority acquired the database'
fi
[ "$(psql_plugin "SELECT has_schema_privilege('capture_app','capture','CREATE'),has_table_privilege('capture_app','capture.schema_version','UPDATE'),has_table_privilege('capture_app','capture.operations','DELETE')")" = 'f|f|f' ] \
    || fail 'Plugin runtime grants are not restricted'
[ "$(docker exec "$fleet_pg" psql -U postgres -d fleet -At -c "SELECT to_regclass('capture.operations') IS NULL" | tr -d '\r')" = t ] \
    || fail 'Capture schema leaked into fleet database'

# A lost management credential cannot keep a consumer channel alive.
other_id=$(random_hex 16)
other_token=$(http POST /api/v1/credentials "$admin" \
    "{\"id\":\"$other_id\",\"name\":\"s4-other-client\",\"role\":\"admin\",\"expires_at\":\"$(iso_in 3600)\"}" 201 | json 'd["token"]')
other_file=$run/other_admin_token
write_token_file "$other_file" "$other_token"
other_channel=$(channel_id_of "$(new_channel "$other_token")")
start_consumer "$other_channel" "$other_file"
other_consumer=$consumer_name
owned_id=$(capi POST v1/captures "$(capture_body "$other_channel" "$first" 14)" 202 "$other_token" | json 'd["id"]')
capture_active "$owned_id" "$other_token"
capi GET "v1/captures/$owned_id" '' 403 >/dev/null
http DELETE "/api/v1/credentials/$other_id" "$admin" '' 204 >/dev/null
capi GET "v1/channels/$other_channel/next" '' 401 "$other_token" >/dev/null
wait_for 40 'Revoked client retained capture execution' op_state_is "$owned_id" stopped expired
docker stop --time 5 "$other_consumer" >/dev/null

# Core crash removes host authority and consumer access; plugin withdraws control.
core_channel=$(channel_id_of "$(new_channel)")
start_consumer "$core_channel" "$admin_file"
core_consumer=$consumer_name
core_request=$(capture_body "$core_channel" "$first" 20)
core_op=$(capi POST v1/captures "$core_request" 202 | json 'd["id"]')
capture_active "$core_op"
docker kill "$core" >/dev/null || fail 'Core crash injection failed'
wait_for 40 'Core loss did not withdraw capture' op_state_is "$core_op" stopped
compose up --detach --wait fleet
core=$(compose ps --quiet fleet)
base=http://127.0.0.1:$(host_port "$core" 8080)
wait_for 40 'Core recovery did not preserve stopped outcome' capi_is "v1/captures/$core_op" 'd["state"] == "stopped"'
[ "$(capi POST v1/captures "$core_request" 202 | json 'd["state"]')" = stopped ] || fail 'Core recovery replayed a capture'
docker stop --time 5 "$core_consumer" >/dev/null
echo 'PASS competing-authority denial, separate/narrow storage, owner isolation, API-key revocation and core crash withdrawal.'

# Plugin crash: no execution replay and no new lease while old work is uncertain.
crash_channel=$(channel_id_of "$(new_channel)")
start_consumer "$crash_channel" "$admin_file"
crash_consumer=$consumer_name
crash_request=$(capture_body "$crash_channel" "$first" 12)
crash_operation=$(capi POST v1/captures "$crash_request" 202)
crash_id=$(printf '%s' "$crash_operation" | json 'd["id"]')
reserved_peer_id=$(printf '%s' "$crash_operation" | json 'd["targets"][0]["peer_id"]')
capture_active "$crash_id"
before_crash=$(http GET /api/v1/plugins/livecapture "$admin")
docker kill "$plugin" >/dev/null || fail 'Plugin crash injection failed'
wait_for 40 'Crashed plugin stayed ready' core_is /api/v1/plugins/livecapture 'd["state"] == "unavailable"'
http GET /readyz '' >/dev/null
compose up --detach --wait livecapture-plugin
wait_for 40 'Plugin recovery resumed or lost uncertain execution' capi_is "v1/captures/$crash_id" 'd["state"] == "interrupted"'
wait_for 40 'Existing Collectors did not reconnect after plugin crash' two_ready_peers
after_crash=$(http GET /api/v1/plugins/livecapture "$admin")
if ! { [ "$(printf '%s' "$before_crash" | json 'd["instance_id"]')" != "$(printf '%s' "$after_crash" | json 'd["instance_id"]')" ] \
    && [ "$(printf '%s' "$before_crash" | json 'd["epoch"]')" != "$(printf '%s' "$after_crash" | json 'd["epoch"]')" ]; }; then
    fail 'Plugin authority was not fenced across crash'
fi
[ "$(capi POST v1/captures "$crash_request" 202 | json 'd["state"]')" = interrupted ] || fail 'Identical post-crash retry dispatched again'
recovered_peers=$(peers_array)
reserved_peer=$(printf '%s' "$recovered_peers" | json "json.dumps([p for p in d if p['id'] == '$reserved_peer_id'][:1])")
recovered_channel=$(channel_id_of "$(new_channel)")
start_consumer "$recovered_channel" "$admin_file"
recovered_consumer=$consumer_name
capi POST v1/captures "$(capture_body "$recovered_channel" "$reserved_peer" 20)" 409 >/dev/null
wait_for 40 'Uncertain lease expiry guard did not elapse' no_live_leases
after_guard=$(capi POST v1/captures "$(capture_body "$recovered_channel" "$reserved_peer" 12)" 202 | json 'd["id"]')
capture_active "$after_guard"
generate "$(printf '%s' "$reserved_peer" | json 'd[0]["identity"]["pod"]')"
wait_for 40 'Explicit new capture after recovery did not deliver' counts_match "$recovered_consumer" "$(each_signal 1)"
capture_stop "$after_guard"
docker stop --time 5 "$crash_consumer" >/dev/null
docker stop --time 5 "$recovered_consumer" >/dev/null
peers=$recovered_peers
echo 'PASS plugin crash: metadata and conservative leases survive, stale authority is fenced, no replay, explicit new capture works after expiry.'

# Revoking the shared fleet agent credential in core closes every member.
revocation_channel=$(channel_id_of "$(new_channel)")
start_consumer "$revocation_channel" "$admin_file"
revocation_consumer=$consumer_name
revocation=$(capi POST v1/captures "$(capture_body "$revocation_channel" "$peers" 6)" 202)
revocation_id=$(printf '%s' "$revocation" | json 'd["id"]')
capture_active "$revocation_id"
http DELETE "/api/v1/credentials/$credential" "$admin" '' 204 >/dev/null
wait_for 40 'Revoked fleet credential retained control streams' capi_is v1/peers 'len(d["items"]) == 0'
wait_for 40 'Revoked execution did not remain bounded by expiry' capi_is "v1/captures/$revocation_id" 'd["state"] == "expired"'
docker stop --time 5 "$revocation_consumer" >/dev/null
persisted_authority=$(printf '%s' "$revocation" | json 'd["authority_id"]')
compose stop --timeout 5 livecapture-plugin
dump="/tmp/$project.dump"
docker exec "$plugin_pg" pg_dump -U postgres -d livecapture -Fc -f "$dump" >/dev/null || fail 'Plugin fixture dump failed'
restore="$project-restore"
docker run --detach --name "$restore" --network "${project}_capture-database" --cpus 1 --memory 256m --pids-limit 128 \
    --env POSTGRES_PASSWORD_FILE=/run/secrets/postgres_password --env POSTGRES_DB=livecapture \
    --mount "type=bind,source=$run/plugin/postgres_password,target=/run/secrets/postgres_password,readonly" \
    --mount "type=bind,source=$run/plugin/migrate_password,target=/run/secrets/capture_migrate_password,readonly" \
    --mount "type=bind,source=$run/plugin/runtime_password,target=/run/secrets/capture_runtime_password,readonly" \
    --mount "type=bind,source=$root/deploy/compose/fleet/livecapture/init-db.sh,target=/docker-entrypoint-initdb.d/10-capture.sh,readonly" \
    --tmpfs /var/lib/postgresql/data "$postgres_image" >/dev/null || fail 'Fresh plugin restore database startup failed'
children+=("$restore")
restore_ready() { docker exec "$restore" pg_isready -h 127.0.0.1 -U postgres -d livecapture >/dev/null 2>&1; }
wait_for 40 'Fresh restore database did not become ready' restore_ready
dump_host=$run/plugin.dump
docker cp "$plugin_pg:$dump" "$dump_host" >/dev/null || fail 'Fixture backup copy failed'
docker cp "$dump_host" "$restore:/tmp/plugin.dump" >/dev/null || fail 'Restore archive copy failed'
docker exec "$restore" pg_restore -U postgres --exit-on-error --dbname livecapture /tmp/plugin.dump >/dev/null || fail 'Fresh plugin restore failed'
restored=$(docker exec "$restore" psql -U postgres -d livecapture -At \
    -c "SELECT authority_id FROM capture.metadata; SELECT count(*) FROM capture.schema_version; SELECT state FROM capture.operations WHERE id='$crash_id'" | tr -d '\r') \
    || fail 'Restored plugin lost authority, schema or interrupted metadata'
if ! { [ "$(printf '%s\n' "$restored" | sed -n 1p)" = "$persisted_authority" ] \
    && [ "$(printf '%s\n' "$restored" | sed -n 2p)" = 2 ] \
    && [ "$(printf '%s\n' "$restored" | sed -n 3p)" = interrupted ]; }; then
    fail 'Restored plugin lost authority, schema or interrupted metadata'
fi
runtime_file=$run/plugin/runtime_url
chmod 644 "$runtime_file"
sed "s#@capture-postgres:5432/#@$restore:5432/#" "$runtime_file" > "$tmp/runtime_url" && cat "$tmp/runtime_url" > "$runtime_file"
chmod 444 "$runtime_file"
compose up --detach --wait livecapture-plugin
wait_for 40 'Revoked fleet credential reconnected after plugin restart' capi_is v1/peers 'len(d["items"]) == 0'
[ "$(capi GET "v1/captures/$crash_id" | json 'd["state"]')" = interrupted ] || fail 'Runtime did not reopen restored operation metadata'
docker stop --time 5 "$restore" >/dev/null || fail 'Plugin database outage injection failed'
plugin_stopped() { [ "$(docker inspect "$plugin" --format '{{.State.Running}}')" = false ]; }
wait_for 40 'Plugin stayed active after losing its database authority' plugin_stopped
[ "$(http GET /readyz '' | json 'd["status"]')" = ready ] || fail 'Plugin database outage affected core reporting'
echo 'PASS core credential revocation, fresh-database metadata restore, and plugin database-outage failure isolation.'
