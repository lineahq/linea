#!/bin/bash
# Docker capacity measurement harness with real Collectors, the shell twin of
# run.ps1. It provisions a unique disposable Compose project with its own
# private database, roles and deployment token, runs pinned official Collector
# Contrib 0.162.0 processes (not simulated connections), and measures
# registration, steady reporting and stable-identity reconnects. Needs Docker
# Compose v2 (Linux containers), curl and python3.
#
#   tools/fleet-capacity/run.sh (--functional-check | --dedicated-environment) \
#       [--with-capture-fault] [--capture-collector-image IMAGE] \
#       [--collectors N] [--duration-seconds N] [--image-tag TAG] \
#       [--network-octet N] [--build]
#
#   --functional-check        shared-host harness check: two Collectors (one,
#                             with --with-capture-fault) for 35 seconds
#   --dedicated-environment   capacity run on an agreed dedicated host
#                             (exactly one of the two modes is required; with
#                             neither the script refuses before Docker,
#                             filesystem writes or secret generation)
#   --with-capture-fault      also run the unchanged livecapture Collector, a
#                             capture whose channel is never acknowledged, and
#                             check failure isolation
#   --capture-collector-image IMAGE
#                             compatible livecapture Collector image; default
#                             linea-fleet-livecapture-compat:collector
#   --collectors N            Collectors, 1-1000; default 100 (ignored with
#                             --functional-check)
#   --duration-seconds N      steady reporting seconds, 35-1800; default 120
#                             (ignored with --functional-check)
#   --image-tag TAG           tag of the fleet, plugin and generator images;
#                             lowercase letters, digits, '.' and '-'; default
#                             local
#   --network-octet N         second octet of the 10.N.0.0 fixture subnets,
#                             1-254; default 245
#   --build                   build the fleet image (and, with
#                             --with-capture-fault, the plugin and generator
#                             images) first; never builds Collector source
#   -h, --help                print this text
#
# Results and bounded metric/resource snapshots are written to
# .local/fleet-capacity-<id>/ (result.json); the Compose project and its
# volumes are removed on exit.
set -u -o pipefail
export MSYS_NO_PATHCONV=1
winpath() { (cd "$1" && { pwd -W 2>/dev/null || pwd; }); }
root=$(winpath "$(dirname "$0")/../..")

usage() { awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; }
die() { echo "$*" >&2; exit 1; }
bad_usage() { echo "$*" >&2; usage >&2; exit 2; }
int_in_range() { # int_in_range VALUE MIN MAX
    case $1 in ''|*[!0-9]*) return 1;; esac
    [ "$1" -ge "$2" ] && [ "$1" -le "$3" ]
}

functional=0 dedicated=0 fault=0 build=0
capture_collector_image='linea-fleet-livecapture-compat:collector'
collectors=100 duration=120 image_tag='local' octet=245
while [ $# -gt 0 ]; do
    case $1 in
        --functional-check) functional=1;;
        --dedicated-environment) dedicated=1;;
        --with-capture-fault) fault=1;;
        --build) build=1;;
        --capture-collector-image) [ $# -ge 2 ] || bad_usage "$1 needs a value"; capture_collector_image=$2; shift;;
        --collectors) [ $# -ge 2 ] || bad_usage "$1 needs a value"; collectors=$2; shift;;
        --duration-seconds) [ $# -ge 2 ] || bad_usage "$1 needs a value"; duration=$2; shift;;
        --image-tag) [ $# -ge 2 ] || bad_usage "$1 needs a value"; image_tag=$2; shift;;
        --network-octet) [ $# -ge 2 ] || bad_usage "$1 needs a value"; octet=$2; shift;;
        -h|--help) usage; exit 0;;
        *) bad_usage "unknown argument: $1";;
    esac
    shift
done
int_in_range "$collectors" 1 1000 || bad_usage '--collectors must be an integer from 1 to 1000'
int_in_range "$duration" 35 1800 || bad_usage '--duration-seconds must be an integer from 35 to 1800'
int_in_range "$octet" 1 254 || bad_usage '--network-octet must be an integer from 1 to 254'
printf '%s' "$image_tag" | grep -Eq '^[a-z0-9][a-z0-9.-]{0,40}$' \
    || bad_usage '--image-tag must match ^[a-z0-9][a-z0-9.-]{0,40}$'

# This guard must precede Docker, credentials, filesystem writes and provisioning.
if [ "$functional" = 0 ] && [ "$dedicated" = 0 ]; then
    die 'Capacity runs require --dedicated-environment on an agreed dedicated host. Use --functional-check for at most two Collectors.'
fi
if [ "$functional" = 1 ] && [ "$dedicated" = 1 ]; then die 'Select one execution mode'; fi
if [ "$functional" = 1 ]; then
    if [ "$fault" = 1 ]; then collectors=1; else collectors=2; fi
    duration=35
fi
python=$(command -v python3 || command -v python || true)
[ -n "$python" ] || die 'python3 is required'

random_hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }
id=$(random_hex 16)
project="fleet-capacity-$id"
dir=$root/.local/$project
env_file=$dir/compose.env
override=$dir/collectors.json
collector_image='otel/opentelemetry-collector-contrib:0.162.0@sha256:39923a8e431bd1f57be82411999d389fcfe40857492e4365456d97a4c1f74be6'
tmp=$(mktemp -d "${TMPDIR:-/tmp}/linea-capacity-XXXXXX")
samples=$tmp/samples
: > "$samples"
started=0
admin=
main=$$
trap 'exit 1' USR1 INT TERM

compose_args=(--parallel 4 --project-name "$project" --env-file "$env_file" --file "$root/deploy/compose/fleet/compose.yaml")
if [ "$fault" = 1 ]; then compose_args+=(--file "$root/deploy/compose/fleet/compose.livecapture.yaml"); fi
compose_args+=(--file "$override")

if [ "$functional" = 1 ]; then mode=functional_two_collectors; else mode=dedicated_capacity; fi

fail() { echo "FAIL: $*" >&2; kill -USR1 "$main"; exit 1; }
json() { "$python" -c "import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1" 2>/dev/null; }
now_s() { "$python" -c 'import time; print(time.time())'; }
elapsed_since() { awk -v a="$1" -v b="$(now_s)" 'BEGIN { printf "%.3f", b - a }'; }
iso_in() { # iso_in SECONDS -> UTC timestamp that far in the future
    "$python" -c 'import datetime, sys; print((datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=float(sys.argv[1]))).strftime("%Y-%m-%dT%H:%M:%S.%fZ"))' "$1"
}

# record KEY VALUE-AS-JSON / record_str KEY STRING: set a (dotted) key in result.json.
record() {
    "$python" - "$dir/result.json" "$1" "$2" "${3:-json}" <<'PYEOF'
import json, os, sys
path, key, value, kind = sys.argv[1:5]
data = json.load(open(path)) if os.path.exists(path) else {}
node = data
parts = key.split(".")
for part in parts[:-1]:
    node = node.setdefault(part, {})
node[parts[-1]] = json.loads(value) if kind == "json" else value
with open(path, "w") as f:
    f.write(json.dumps(data, indent=2))
PYEOF
}
record_str() { record "$1" "$2" str; }

cleanup() {
    local rc=$?
    trap - EXIT USR1 INT TERM
    if [ -d "$dir" ]; then
        "$python" - "$dir/result.json" "$samples" <<'PYEOF' || true
import json, math, os, sys
path, samples = sys.argv[1:3]
data = json.load(open(path)) if os.path.exists(path) else {}
values = sorted(float(x) for x in open(samples).read().split())
if values:
    n = len(values)
    data["http_samples"] = {"count": n,
        "p50_ms": values[int(math.floor((n - 1) * 0.50))],
        "p95_ms": values[int(math.floor((n - 1) * 0.95))],
        "max_ms": values[-1]}
with open(path, "w") as f:
    f.write(json.dumps(data, indent=2))
PYEOF
        if [ "$started" = 1 ]; then
            if docker compose "${compose_args[@]}" down --volumes --remove-orphans --timeout 5; then
                record_str cleanup passed
            else
                record_str cleanup failed
                record_str status failed
                rc=1
            fi
        else
            record_str cleanup passed
        fi
        echo "Result and bounded metric/resource snapshots: $dir"
    fi
    rm -rf "$tmp"
    exit "$rc"
}
trap cleanup EXIT

compose() { docker compose "${compose_args[@]}" "$@" || fail 'Capacity fixture Compose operation failed; request/configuration omitted'; }

# request METHOD PATH TOKEN [BODY] [EXPECTED] -> response body. EXPECTED 0 admits
# 200 or 204. The token goes to curl on standard input, never its arguments.
request() {
    local method=$1 path=$2 token=$3 body=${4:-} expected=${5:-200} line code seconds
    local args=(-s -o "$tmp/resp" -w '%{http_code} %{time_total}' -X "$method" --max-time 8)
    if [ -n "$token" ]; then args+=(-K -); fi
    if [ -n "$body" ]; then args+=(-H 'Content-Type: application/json' --data "$body"); fi
    if [ -n "$token" ]; then
        line=$(printf 'header = "Authorization: Bearer %s"\n' "$token" | curl "${args[@]}" "$base$path")
    else
        line=$(curl "${args[@]}" "$base$path")
    fi
    code=${line%% *}
    seconds=${line#* }
    awk -v s="$seconds" 'BEGIN { printf "%.3f\n", s * 1000 }' >> "$samples"
    if [ "$expected" != 0 ] && [ "$code" != "$expected" ]; then
        echo "Capacity API request returned HTTP $code; body omitted" >&2
        return 1
    fi
    if [ "$expected" = 0 ] && [ "$code" != 200 ] && [ "$code" != 204 ]; then
        echo "Capacity API request returned HTTP $code; body omitted" >&2
        return 1
    fi
    if [ "$code" != 204 ]; then cat "$tmp/resp"; fi
}
req() { request "$@" || fail 'Capacity API request failed'; }
capture_api() { # capture_api METHOD PATH [BODY] [EXPECTED]
    request "$1" "/api/v1/plugins/livecapture/api/$2" "$admin" "${3:-}" "${4:-200}"
}
wait_for() { # wait_for SECONDS DESCRIPTION COMMAND...
    local deadline=$((SECONDS + $1)) what=$2
    shift 2
    while :; do
        if "$@"; then return 0; fi
        [ "$SECONDS" -lt "$deadline" ] || fail "$what"
        sleep 0.5
    done
}
metric_value() { # metric_value FILE NAME
    awk -v n="$2" '{ sub(/\r$/, "") } $1 == n && NF == 2 { print $2; found = 1; exit } END { exit !found }' "$1" \
        || fail 'Required operational metric absent'
}
snapshot() { # snapshot PHASE: metrics text and resource usage of the fixture
    request GET /metrics "$admin" > "$dir/$1.prom" || fail 'Capacity API request failed'
    docker stats --no-stream --format '{{json .}}' "$core" "$postgres_container" > "$dir/$1.resources.jsonl" \
        || fail 'Fixture resource snapshot failed'
}
connected_is() { [ "$(request GET /api/v1/summary "$admin" 2>/dev/null | json 'd["connected"]')" = "$1" ]; }
inventory() { # one JSON item per line, across every page
    local after='' path page
    while :; do
        path='/api/v1/agents?limit=500'
        if [ -n "$after" ]; then path="$path&after=$after"; fi
        page=$(request GET "$path" "$admin") || return 1
        printf '%s' "$page" | json 'chr(10).join(json.dumps(i) for i in d["items"])' | sed '/^$/d'
        after=$(printf '%s' "$page" | json 'd.get("next_after") or ""')
        if [ -z "$after" ]; then break; fi
    done
}
line_count() { local n; n=$(wc -l < "$1"); echo $((n + 0)); }

# Everything below runs against Docker; the guards above have already passed.
machine=$(docker info --format '{{.OSType}}|{{.Architecture}}|{{.NCPU}}|{{.MemTotal}}|{{.ServerVersion}}') || die 'Linux Docker daemon required'
IFS='|' read -r machine_os machine_arch machine_cpus machine_memory machine_docker <<< "$machine"
[ "$machine_os" = linux ] || die 'Linux Docker daemon required'
if [ "$fault" = 1 ]; then reserve=3072; else reserve=2048; fi
required=$(((collectors * 128 + reserve) * 1048576))
if [ "$functional" = 0 ] && [ "$machine_memory" -lt "$required" ]; then
    die 'Dedicated Docker memory budget is below the configured Collector limits plus 2 GiB reserve'
fi

sh "$root/scripts/fleet/new-deployment-secrets.sh" "$dir" >/dev/null || fail 'Deployment secrets could not be created'
if [ "$fault" = 1 ]; then
    sh "$root/scripts/fleet/livecapture/new-secrets.sh" "$dir/capture" >/dev/null || fail 'Capture secrets could not be created'
fi
record_str mode "$mode"
record_str status failed
record collectors "$collectors"
record steady_seconds "$duration"
record_str scope 'Official reporting-only Collectors; livecapture capacity is a separate profile'
record_str source "$(git -C "$root" rev-parse HEAD)"
record_str harness_sha256 "$("$python" -c 'import hashlib, sys; print(hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest().upper())' "$0")"
record images '{}'
record phases '{}'
record resource_limits '{"fleet_cpus":2,"fleet_memory_mib":512,"postgres_cpus":1,"postgres_memory_mib":512,"collector_cpus":0.25,"collector_memory_mib":128}'
record http_samples '[]'
if [ "$fault" = 1 ]; then record capture_fault true; else record capture_fault false; fi
record collector_processes_total "$((collectors + fault))"
if [ "$fault" = 1 ]; then
    record resource_limits.capture_plugin_cpus 2
    record resource_limits.capture_plugin_memory_mib 512
    record resource_limits.capture_postgres_cpus 1
    record resource_limits.capture_postgres_memory_mib 512
    record resource_limits.capture_collector_cpus 1
    record resource_limits.capture_collector_memory_mib 256
fi
record machine "$(printf '{"os":"%s","architecture":"%s","cpus":%s,"memory_bytes":%s,"docker":"%s"}' \
    "$machine_os" "$machine_arch" "$machine_cpus" "$machine_memory" "$machine_docker")"

{
    printf "FLEET_SECRET_DIR='%s'\nFLEET_SERVER_ID=%s\nFLEET_API_PORT=0\nFLEET_AGENT_PORT=0\nFLEET_VERSION=%s\n" "$dir" "$project" "$image_tag"
    if [ "$fault" = 1 ]; then
        printf "FLEET_LIVECAPTURE_SECRET_DIR='%s/capture'\nCAPTURE_VERSION=%s\nCAPTURE_CONTROL_PORT=0\nCAPTURE_DELIVERY_PORT=0\n" "$dir" "$image_tag"
    fi
} > "$env_file"
# Compose needs the variable defined to interpolate the Collector services; the
# real credential replaces it once the fleet server has issued it.
export LINEA_FLEET_AGENT_TOKEN='not-issued-yet'

# The Compose override: the fleet command, one service per Collector (each with
# a locally assigned UUIDv7 instance identity kept across restarts) and the
# fixture network subnets.
"$python" - "$root" "$dir" "$project" "$collectors" "$fault" "$octet" "$collector_image" "$capture_collector_image" "$override" <<'PYEOF' \
    || fail 'Compose override could not be generated'
import json, os, sys, time

root, d, project, count, fault, octet, collector_image, capture_image, override = sys.argv[1:10]
count, octet, fault = int(count), int(octet), fault == "1"


def uuid7():
    b = bytearray(os.urandom(16))
    millis = int(time.time() * 1000)
    for i in range(5, -1, -1):
        b[i] = millis & 255
        millis >>= 8
    b[6] = 0x70 | (b[6] & 15)
    b[8] = 0x80 | (b[8] & 63)
    h = b.hex()
    return "%s-%s-%s-%s-%s" % (h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])


command = ["--server-id", project, "--management-listen", "0.0.0.0:8080",
           "--agent-listen", "0.0.0.0:4320", "--allow-insecure-remote",
           "--max-agents", str(count * 2), "--max-connections", str(count + 16)]
services = {"fleet": {"command": command}}
if fault:
    command += ["--plugins-config", "/etc/linea-fleet/plugins.json", "--plugin-listen", "0.0.0.0:8081"]
    services["capture-probe"] = {
        "image": capture_image, "command": ["--config=/fixture.yaml"],
        "environment": {"GOMAXPROCS": "1", "LINEA_FLEET_AGENT_TOKEN": "${LINEA_FLEET_AGENT_TOKEN}", "POD_NAME": "capacity-probe"},
        "volumes": [root + "/examples/fleet-livecapture/collector.yaml:/fixture.yaml:ro",
                    d + "/capture/tls_cert.pem:/certs/ca.pem:ro"],
        "networks": ["capture-ingress"], "cpus": 1, "mem_limit": "256m", "pids_limit": 128,
        "read_only": True, "cap_drop": ["ALL"], "security_opt": ["no-new-privileges:true"], "restart": "no"}
with open(os.path.join(root, "examples", "fleet-collector.yaml"), newline="") as f:
    template = f.read()
for number in range(1, count + 1):
    name = "collector-%06d" % number
    config = template.replace("  opamp:", "  opamp:\n    instance_uid: " + uuid7())
    path = os.path.join(d, name + ".yaml")
    with open(path, "w", newline="") as f:
        f.write(config)
    os.chmod(path, 0o644)
    services[name] = {
        "image": collector_image, "command": ["--config=/etc/collector.yaml"],
        "environment": {"GOMAXPROCS": "1", "GOMEMLIMIT": "96MiB",
                        "LINEA_FLEET_AGENT_TOKEN": "${LINEA_FLEET_AGENT_TOKEN}",
                        "LINEA_FLEET_OPAMP_ENDPOINT": "ws://fleet:4320/v1/opamp"},
        "volumes": [d + "/" + name + ".yaml:/etc/collector.yaml:ro"], "networks": ["ingress"],
        "cpus": 0.25, "mem_limit": "128m", "pids_limit": 64, "read_only": True,
        "cap_drop": ["ALL"], "security_opt": ["no-new-privileges:true"], "restart": "no",
        "logging": {"driver": "local", "options": {"max-size": "1m", "max-file": "2"}}}
networks = {"database": {"ipam": {"config": [{"subnet": "10.%d.0.0/20" % octet}]}},
            "ingress": {"ipam": {"config": [{"subnet": "10.%d.16.0/20" % octet}]}}}
if fault:
    for index, network in enumerate(["plugins", "capture-database", "capture-ingress"]):
        networks[network] = {"ipam": {"config": [{"subnet": "10.%d.%d.0/24" % (octet, 32 + index)}]}}
with open(override, "w") as f:
    f.write(json.dumps({"services": services, "networks": networks}, indent=2))
PYEOF
names=()
number=1
while [ "$number" -le "$collectors" ]; do
    names+=("$(printf 'collector-%06d' "$number")")
    number=$((number + 1))
done

if [ "$build" = 1 ]; then
    compose build fleet
    if [ "$fault" = 1 ]; then
        compose build livecapture-plugin
        docker build --file "$root/deploy/docker/linea-fleet-livecapture.Dockerfile" --target fixture \
            --tag "linea-fleet-livecapture-generator:$image_tag" "$root" || fail 'Synthetic input image build failed'
    fi
fi
record_str images.collector "$collector_image"
fleet_id=$(docker image inspect "ghcr.io/lineahq/linea-fleet:$image_tag" --format '{{.Id}}') \
    || fail 'Build the selected fleet image before running the profile'
record_str images.fleet "$fleet_id"
if [ "$fault" = 1 ]; then
    for entry in "capture_collector=$capture_collector_image" \
        "capture_plugin=ghcr.io/lineahq/linea-fleet-livecapture:$image_tag" \
        "synthetic_input=linea-fleet-livecapture-generator:$image_tag"; do
        image_id=$(docker image inspect "${entry#*=}" --format '{{.Id}}') \
            || fail 'Optional capture fault profile image missing; see capacity README'
        record_str "images.${entry%%=*}" "$image_id"
    done
fi

started=1
compose up --detach --wait postgres
compose run --rm -T migrate >/dev/null
if [ "$fault" = 1 ]; then
    compose up --detach --wait capture-postgres
    compose run --rm -T capture-migrate >/dev/null
fi
admin=$(compose run --rm -T bootstrap | tr -d '\r' | json 'd["token"]')
[ -n "$admin" ] || fail 'Fleet bootstrap did not disclose a credential'
compose up --detach --wait fleet
if [ "$fault" = 1 ]; then compose up --detach --wait livecapture-plugin; fi
core=$(compose ps --quiet fleet)
postgres_container=$(compose ps --quiet postgres)
record_str images.postgres "$(docker inspect "$postgres_container" --format '{{.Image}}')"
port=$(docker port "$core" 8080/tcp | head -n 1 | sed 's/.*://' | tr -d '\r')
base=http://127.0.0.1:$port

# One fleet agent credential for all Collectors; the capture label admits the probe to livecapture.
labels=''
if [ "$fault" = 1 ]; then labels=',"labels":{"capture":"s4"}'; fi
credential=$(printf '{"id":"%s","name":"capacity-deployment","role":"agent","expires_at":"%s"%s}' \
    "$(random_hex 16)" "$(iso_in 3600)" "$labels")
issued=$(req POST /api/v1/credentials "$admin" "$credential" 201 | json 'd["token"]')
[ -n "$issued" ] || fail 'Agent credential was not issued'
export LINEA_FLEET_AGENT_TOKEN=$issued
if [ "$fault" = 1 ]; then compose up --detach --no-deps capture-probe; fi

phase=$(now_s)
compose up --detach --no-deps "${names[@]}"
wait_for "$((collectors > 120 ? collectors : 120))" 'Not all real Collectors registered' connected_is "$collectors"
record phases.registration_seconds "$(elapsed_since "$phase")"
inventory > "$tmp/before.jsonl" || fail 'Capacity API request failed'
[ "$(line_count "$tmp/before.jsonl")" = "$collectors" ] || fail 'Inventory pagination lost or duplicated identities'
snapshot registered

phase=$(now_s)
end=$((SECONDS + duration))
while [ "$SECONDS" -lt "$end" ]; do
    summary=$(req GET /api/v1/summary "$admin")
    [ "$(printf '%s' "$summary" | json 'd["connected"]')" = "$collectors" ] || fail 'Collector presence changed during steady reporting'
    req GET /readyz "$admin" >/dev/null
    sleep 1
done
snapshot steady
record phases.steady_elapsed_seconds "$(elapsed_since "$phase")"
steady_reports=$(awk -v a="$(metric_value "$dir/steady.prom" linea_fleet_report_duration_seconds_count)" \
    -v b="$(metric_value "$dir/registered.prom" linea_fleet_report_duration_seconds_count)" 'BEGIN { printf "%.10g", a - b }')
steady_errors=$(awk -v a="$(metric_value "$dir/steady.prom" linea_fleet_report_errors_total)" \
    -v b="$(metric_value "$dir/registered.prom" linea_fleet_report_errors_total)" 'BEGIN { printf "%.10g", a - b }')
record phases.steady_reports "$steady_reports"
record phases.steady_report_errors "$steady_errors"
awk -v r="$steady_reports" -v c="$collectors" -v e="$steady_errors" 'BEGIN { exit !(r >= c && e == 0) }' \
    || fail 'Steady report progress/error gate failed'

if [ "$fault" = 1 ]; then
    one_peer() { [ "$(capture_api GET v1/peers 2>/dev/null | json 'len(d["items"])')" = 1 ]; }
    wait_for 120 'Capture fault peer did not register' one_peer
    peers=$(capture_api GET v1/peers) || fail 'Capture API request failed'
    channel=$(capture_api POST v1/channels "{\"expires_at\":\"$(iso_in 60)\"}" 201) || fail 'Capture API request failed'
    channel_id=$(printf '%s' "$channel" | json 'd["id"]')
    capture_body=$(printf '%s' "$peers" | "$python" -c '
import datetime, json, sys, uuid
peer = json.load(sys.stdin)["items"][0]
expires = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=20)).strftime("%Y-%m-%dT%H:%M:%S.%fZ")
print(json.dumps({"authority_id": peer["authority_id"], "plugin_instance": peer["plugin_instance"],
    "id": uuid.uuid4().hex, "channel_id": sys.argv[1],
    "targets": [{"peer_id": peer["id"], "generation": peer["generation"]}],
    "capture_points": [{"pipeline": "traces", "point": "egress"}], "signals": ["traces"],
    "filter": {}, "expires_at": expires}))' "$channel_id")
    capture_op=$(capture_api POST v1/captures "$capture_body" 202) || fail 'Capture API request failed'
    capture_id=$(printf '%s' "$capture_op" | json 'd["id"]')
    capture_is() { # capture_is STATE...: the capture is in one of the states
        local state
        state=$(capture_api GET "v1/captures/$capture_id" 2>/dev/null | json 'd["state"]')
        for want in "$@"; do [ "$state" = "$want" ] && return 0; done
        return 1
    }
    wait_for 120 'Capture fault activation not acknowledged' capture_is active
    phase=$(now_s)
    docker run --rm --network "${project}_capture-ingress" --cpus 1 --memory 128m --pids-limit 64 --read-only --cap-drop ALL \
        "linea-fleet-livecapture-generator:$image_tag" --endpoint capture-probe:4317 || fail 'Capture fault synthetic input failed'
    fault_reads=0 pending_peak=0 pending_reads=0 uncertain=0
    limit=$((SECONDS + 12))
    while :; do
        capture_api GET "v1/channels/$channel_id/next" '' 0 >/dev/null || fail 'Capture API request failed'
        observed=$(capture_api GET v1/metrics) || fail 'Capture API request failed'
        read -r pending_bytes pending_ok uncertain <<< "$(printf '%s' "$observed" | "$python" -c '
import json, sys
d = json.load(sys.stdin)
g, c = d.get("gauges", {}), d.get("counters", {})
pending = g.get("pending_bytes", 0)
ok = pending > 0 and g.get("metadata_snapshot_available") == 1 and g.get("metadata_snapshot_stale") == 0
print(pending, 1 if ok else 0, c.get("receipt_uncertain_total", 0))')"
        pending_peak=$(awk -v a="$pending_peak" -v b="$pending_bytes" 'BEGIN { printf "%.10g", (b > a ? b : a) }')
        [ "$(req GET /api/v1/summary "$admin" | json 'd["connected"]')" = "$collectors" ] || fail 'Consumer fault disrupted fleet presence'
        [ "$(req GET /readyz "$admin" | json 'd["status"]')" = ready ] || fail 'Consumer fault disrupted core readiness'
        if [ "$pending_ok" = 1 ]; then pending_reads=$((pending_reads + 1)); fi
        fault_reads=$((fault_reads + 1))
        if awk -v u="$uncertain" 'BEGIN { exit !(u > 0) }'; then break; fi
        sleep 0.1
        [ "$SECONDS" -lt "$limit" ] || break
    done
    if ! awk -v u="$uncertain" -v p="$pending_peak" -v r="$pending_reads" 'BEGIN { exit !(u >= 1 && p > 0 && r >= 1) }'; then
        fail 'Unacknowledged real delivery was not observed as bounded uncertainty'
    fi
    wait_for 120 'Consumer loss did not withdraw capture' capture_is stopped expired
    record phases.capture_fault "$(printf '{"elapsed_seconds":%s,"core_read_pairs":%s,"core_read_pairs_while_pending":%s,"peak_observed_pending_bytes":%s,"receipt_uncertain":%s,"payloads_recorded":false}' \
        "$(elapsed_since "$phase")" "$fault_reads" "$pending_reads" "$pending_peak" "$uncertain")"
    printf '%s' "$observed" > "$dir/plugin-consumer-fault.json"
    snapshot consumer-fault
fi

restart_count=$(((collectors + 9) / 10))
if [ "$restart_count" -lt 1 ]; then restart_count=1; fi
phase=$(now_s)
compose restart --timeout 5 "${names[@]:0:$restart_count}"
reconnected() {
    local counts
    inventory > "$tmp/current.jsonl" 2>/dev/null || return 1
    counts=$("$python" - "$tmp/before.jsonl" "$tmp/current.jsonl" <<'PYEOF2'
import json, sys
before = {}
for line in open(sys.argv[1]):
    item = json.loads(line)
    before[item["id"]] = item["generation"]
current = [json.loads(line) for line in open(sys.argv[2])]
print(len(current), sum(1 for i in current if i.get("connected") and i["id"] in before and i["generation"] > before[i["id"]]))
PYEOF2
)
    [ "${counts% *}" = "$collectors" ] && [ "${counts#* }" -ge "$restart_count" ] && connected_is "$collectors"
}
wait_for "$((collectors > 120 ? collectors : 120))" 'Stable-identity reconnects did not advance generations' reconnected
record phases.reconnect_count "$restart_count"
record phases.reconnect_seconds "$(elapsed_since "$phase")"
snapshot reconnected
record_str status passed
echo "PASS $mode: $collectors real Collectors, registration/steady reporting/reconnect, metrics and resource snapshots."
