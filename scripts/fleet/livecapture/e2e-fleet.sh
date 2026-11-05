#!/usr/bin/env bash
# Fleet end-to-end: Compose fleet (PostgreSQL) + livecapture plugin, the
# unchanged livecapture Collector distribution and a contrib Collector that
# reports the same pod identity over OpAMP. Proves fleet agent credentials,
# peer linking, group targeting, label refusal and core revocation.
#
# COLLECTOR_IMAGE (required): image built from the unchanged livecapture
#   extension repository's Dockerfile.
# EXTENSION_SOURCE: that repository, to record its commit.
# COMPOSE_OVERRIDE: extra Compose file, e.g. fixed subnets on a crowded host.
# SKIP_BUILD=1 reuses images; KEEP=1 leaves the project running.
set -u
export MSYS_NO_PATHCONV=1
winpath() { (cd "$1" && { pwd -W 2>/dev/null || pwd; }); }
root="$(winpath "$(dirname "$0")/../../..")"
collector_image=${COLLECTOR_IMAGE:?set COLLECTOR_IMAGE to the unchanged livecapture distribution image}
python=$(command -v python || command -v python3)
id="$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
project="lc-e2e-$id"
run="$root/.local/$project"
runw="$run"
children=()
created=0
main=$$
trap 'exit 1' USR1
fail() { echo "FAIL: $*" >&2; kill -USR1 "$main"; exit 1; }
pass() { echo "PASS: $*"; }
compose() { docker compose --project-name "$project" --env-file "$run/compose.env" --file "$root/deploy/compose/fleet/compose.yaml" --file "$root/deploy/compose/fleet/compose.livecapture.yaml" ${COMPOSE_OVERRIDE:+--file "$COMPOSE_OVERRIDE"} "$@"; }
cleanup() {
  [ "${KEEP:-}" = 1 ] && { echo "kept $project"; return; }
  for c in "${children[@]}"; do docker logs "$c" > "$run/$c.log" 2>&1; docker rm --force "$c" >/dev/null 2>&1; done
  [ "$created" = 1 ] && compose logs --no-color > "$run/compose.log" 2>&1; [ "$created" = 1 ] && compose down --volumes --remove-orphans --timeout 5 >/dev/null 2>&1
}
trap cleanup EXIT
json() { "$python" -c "import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1"; }
# req METHOD PATH TOKEN [BODY] [EXPECTED] -> body on stdout
req() {
  local method=$1 path=$2 token=$3 body=${4:-} expected=${5:-200} code
  local args=(-s -o "$run/resp" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $token" --max-time 8)
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' --data "$body")
  code=$(curl "${args[@]}" "$base$path")
  [ "$code" = "$expected" ] || fail "$method $path returned $code, expected $expected"
  cat "$run/resp"
}
api() { req "$1" "/api/v1/plugins/livecapture/api/$2" "$admin" "${3:-}" "${4:-200}"; }
wait_for() { # wait_for SECONDS DESCRIPTION COMMAND...
  local deadline=$((SECONDS + $1)) what=$2; shift 2
  until "$@"; do [ $SECONDS -lt $deadline ] || fail "$what"; sleep 0.5; done
}

mkdir -p "$root/.local"
sh "$root/scripts/fleet/new-deployment-secrets.sh" "$run/fleet" >/dev/null || fail "fleet secrets"
sh "$root/scripts/fleet/new-plugin-secrets.sh" "$run/plugin" >/dev/null || fail "plugin secrets"
p="$run/plugin"
for name in postgres_password migrate_password runtime_password metrics_token; do openssl rand -hex 32 | tr -d '\r' > "$p/$name"; done
printf '%s' "postgres://capture_migrator:$(cat "$p/migrate_password")@capture-postgres:5432/livecapture?sslmode=disable" > "$p/migrate_url"
printf '%s' "postgres://capture_app:$(cat "$p/runtime_password")@capture-postgres:5432/livecapture?sslmode=disable" > "$p/runtime_url"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=localhost -keyout "$p/tls_key.pem" -out "$p/tls_cert.pem" \
  -addext "subjectAltName=DNS:localhost,DNS:livecapture-plugin,IP:127.0.0.1" -addext "basicConstraints=critical,CA:TRUE" >/dev/null 2>&1 || fail "tls"
cat > "$p/config.json" <<'EOF'
{"id":"livecapture","version":"0.4.0","host_endpoint":"http://fleet:8081","allow_insecure_host":true,
 "host_token_file":"/run/secrets/capture_host_token","service_token_file":"/run/secrets/capture_service_token",
 "http_listen":"0.0.0.0:8090","control_listen":"0.0.0.0:4340","delivery_listen":"0.0.0.0:4341",
 "tls_cert_file":"/run/secrets/capture_tls_cert","tls_key_file":"/run/secrets/capture_tls_key","test_insecure":false,
 "capture_label":"capture","raw_principals":[],"max_peers":256,"metrics_token_file":"/run/secrets/capture_metrics_token"}
EOF
chmod 444 "$p"/*
printf "FLEET_SECRET_DIR='%s/fleet'\nFLEET_LIVECAPTURE_SECRET_DIR='%s/plugin'\nFLEET_SERVER_ID=e2e-%s\nFLEET_API_PORT=0\nFLEET_AGENT_PORT=0\nCAPTURE_CONTROL_PORT=0\nCAPTURE_DELIVERY_PORT=0\nFLEET_VERSION=m4-e2e\nCAPTURE_VERSION=m4-e2e\n" "$runw" "$runw" "$id" > "$run/compose.env"

if [ "${SKIP_BUILD:-}" != 1 ]; then
  compose build fleet >"$run/build.log" 2>&1 || fail "fleet build"
  compose build livecapture-plugin >>"$run/build.log" 2>&1 || fail "plugin build"
  for target in client fixture; do
    docker build --file "$root/deploy/docker/linea-fleet-livecapture.Dockerfile" --target $target --tag "linea-fleet-livecapture-$target:m4-e2e" "$root" >>"$run/build.log" 2>&1 || fail "$target build"
  done
fi

created=1
compose up --detach --wait postgres capture-postgres >/dev/null 2>&1 || fail "databases"
compose run --rm -T migrate >"$run/migrate.log" 2>&1 || { cat "$run/migrate.log"; fail "fleet migrate"; }
compose run --rm -T capture-migrate >"$run/capture-migrate.log" 2>&1 || { cat "$run/capture-migrate.log"; fail "plugin migrate"; }
admin=$(compose run --rm -T bootstrap 2>/dev/null | json "d['token']") || fail "bootstrap"
printf '%s' "$admin" > "$run/admin_token"
compose up --detach --wait fleet >/dev/null 2>&1 || fail "fleet start"
base="http://127.0.0.1:$(docker port "$(compose ps --quiet fleet)" 8080/tcp | head -1 | awk -F: '{print $NF}')"
compose up --detach --wait livecapture-plugin >"$run/plugin-up.log" 2>&1 || { cat "$run/plugin-up.log"; fail "plugin start"; }
wait_for 20 "plugin not ready" sh -c "curl -s -H 'Authorization: Bearer $admin' $base/api/v1/plugins/livecapture | grep -q '\"state\":\"ready\"'"
status=$(req GET /api/v1/plugins/livecapture/status "$admin")
echo "$status" | json "d['data']['host_permissions']" | grep -q True || fail "plugin lacks host permissions: $status"
pass "plugin 0.4.0 ready with agents.read and credentials.introspect"

expires=$("$python" -c "import datetime;print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(days=1)).isoformat())")
unlabeled_id=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
labeled_id=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
req POST /api/v1/credentials "$admin" "{\"id\":\"$unlabeled_id\",\"name\":\"e2e unlabeled\",\"role\":\"agent\",\"expires_at\":\"$expires\"}" 201 | json "d['token']" > "$run/unlabeled_token"
req POST /api/v1/credentials "$admin" "{\"id\":\"$labeled_id\",\"name\":\"e2e collectors\",\"role\":\"agent\",\"expires_at\":\"$expires\",\"labels\":{\"capture\":\"s4\"}}" 201 | json "d['token']" > "$run/agent_token"
group=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
code=$(curl -s -o "$run/resp" -w '%{http_code}' -X PUT -H "Authorization: Bearer $admin" -H 'If-Match: "0"' -H 'Content-Type: application/json' \
  --data '{"name":"e2e cluster","selector":"k8s.cluster.name=s4"}' "$base/api/v1/groups/$group")
[ "$code" = 201 ] || fail "group create returned $code"

# OpAMP side: contrib Collector reporting the pod identity under the same credential.
cat > "$run/opamp.yaml" <<'EOF'
extensions:
  opamp:
    server:
      ws:
        endpoint: ws://fleet:4320/v1/opamp
        headers:
          Authorization: "Bearer ${env:LINEA_FLEET_AGENT_TOKEN}"
    agent_description:
      non_identifying_attributes:
        k8s.pod.name: ${env:POD_NAME}
        k8s.namespace.name: telemetry
        k8s.cluster.name: s4
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 127.0.0.1:4317
exporters:
  debug: {}
service:
  extensions: [opamp]
  telemetry:
    logs:
      level: warn
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [debug]
EOF
agent_token=$(cat "$run/agent_token")
c="$project-opamp-a"
docker run --detach --name "$c" --network "${project}_ingress" --cpus 1 --memory 256m --pids-limit 128 --read-only --cap-drop ALL --security-opt no-new-privileges \
  --mount "type=bind,source=$runw/opamp.yaml,target=/etc/opamp.yaml,readonly" --env "LINEA_FLEET_AGENT_TOKEN=$agent_token" --env POD_NAME=collector-a -e GOMAXPROCS=1 \
  otel/opentelemetry-collector-contrib:0.162.0@sha256:39923a8e431bd1f57be82411999d389fcfe40857492e4365456d97a4c1f74be6 --config /etc/opamp.yaml >/dev/null || fail "opamp collector"
children+=("$c")

# Capture side: the unchanged livecapture distribution with the repository example config.
start_capture() { # name pod tokenfile
  docker run --detach --init --name "$project-$1" --network "${project}_capture-ingress" --network-alias "$2" --cpus 1 --memory 512m --pids-limit 128 --read-only --cap-drop ALL --security-opt no-new-privileges \
    --mount "type=bind,source=$root/examples/fleet-livecapture/collector.yaml,target=/fixture.yaml,readonly" --mount "type=bind,source=$runw/plugin/tls_cert.pem,target=/certs/ca.pem,readonly" \
    --env "LINEA_FLEET_AGENT_TOKEN=$(cat "$3")" --env "POD_NAME=$2" -e GOMAXPROCS=1 "$collector_image" --config /fixture.yaml >/dev/null || fail "capture collector $1"
  children+=("$project-$1")
}
start_capture capture-a collector-a "$run/agent_token"
peer_count() { api GET v1/peers | json "len(d['items'])"; }
linked() { api GET v1/peers | json "[p.get('agent_id','') for p in d['items']]" | grep -q "'[0-9a-f]\{32\}'"; }
wait_for 40 "capture peer did not register" sh -c "[ \"\$(curl -s -H 'Authorization: Bearer $admin' $base/api/v1/plugins/livecapture/api/v1/peers | \"$python\" -c 'import json,sys;print(len(json.load(sys.stdin)[\"items\"]))')\" = 1 ]"
pass "unchanged extension authenticated with a labeled fleet agent credential"
agents=$(req GET "/api/v1/agents?connected=true" "$admin")
agent=$(echo "$agents" | json "[a['id'] for a in d['items'] if a.get('attributes',{}).get('k8s.pod.name')=='collector-a'][0]") || fail "OpAMP agent missing: $agents"
wait_for 30 "peer was not linked to the fleet agent" linked
peers=$(api GET v1/peers)
[ "$(echo "$peers" | json "d['items'][0]['agent_id']")" = "$agent" ] || fail "peer linked to wrong agent"
[ "$(echo "$peers" | json "d['items'][0]['credential_id']")" = "$labeled_id" ] || fail "peer credential mismatch"
pass "capture peer linked to OpAMP agent $agent by credential and pod identity"

# Unlabeled credential: refused on both the control stream and the delivery endpoint.
start_capture capture-b collector-b "$run/unlabeled_token"
docker run --rm --network "${project}_capture-ingress" --cpus 1 --memory 128m --read-only --cap-drop ALL \
  --mount "type=bind,source=$runw/plugin/tls_cert.pem,target=/certs/ca.pem,readonly" --mount "type=bind,source=$runw/unlabeled_token,target=/fixture_token,readonly" \
  linea-fleet-livecapture-fixture:m4-e2e --endpoint livecapture-plugin:4341 --tls-ca /certs/ca.pem --token-file /fixture_token --expect-code Unauthenticated >/dev/null || fail "unlabeled delivery not refused"
sleep 6
[ "$(peer_count)" = 1 ] || fail "unlabeled credential registered a peer"
pass "credential without capture label refused (control and delivery)"

# Capture by fleet group, delivered to an independent consumer.
peers=$(api GET v1/peers)
authority=$(echo "$peers" | json "d['items'][0]['authority_id']")
instance=$(echo "$peers" | json "d['items'][0]['plugin_instance']")
chexp=$("$python" -c "import datetime;print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=110)).isoformat())")
channel=$(api POST v1/channels "{\"expires_at\":\"$chexp\"}" 201 | json "d['id']")
c="$project-consumer"
docker run --detach --name "$c" --network "${project}_ingress" --cpus 1 --memory 128m --read-only --cap-drop ALL \
  --mount "type=bind,source=$runw/admin_token,target=/consumer_token,readonly" linea-fleet-livecapture-client:m4-e2e \
  --endpoint http://fleet:8080/api/v1/plugins/livecapture/api --allow-insecure-http --token-file /consumer_token --channel "$channel" --duration 100s --verify-redaction >/dev/null || fail "consumer"
children+=("$c")
wait_for 20 "consumer not ready" sh -c "docker logs $c 2>&1 | grep -q consumer_ready"
body=$("$python" - "$authority" "$instance" "$channel" "$group" <<'EOF'
import datetime, json, secrets, sys
a, i, ch, g = sys.argv[1:]
print(json.dumps({"authority_id": a, "plugin_instance": i, "id": secrets.token_hex(16), "channel_id": ch, "group_id": g,
  "capture_points": [{"pipeline": p, "point": "egress"} for p in ("traces", "logs", "metrics")], "signals": ["traces", "logs", "metrics"],
  "filter": {"traces": {"span": ["status.code == STATUS_CODE_ERROR"]}, "logs": {"log": ["severity_number >= SEVERITY_NUMBER_ERROR"]}, "metrics": {"datapoint": ["attributes[\"keep\"] == true"]}},
  "limits": {}, "expires_at": (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=40)).isoformat()}))
EOF
)
printf "%s" "$body" > "$run/capture.json"
op=$(api POST v1/captures "$body" 202)
opid=$(echo "$op" | json "d['id']")
[ "$(echo "$op" | json "d['targets'][0]['agent_id']")" = "$agent" ] || fail "group target missing agent_id: $op"
wait_for 20 "capture not active" sh -c "curl -s -H 'Authorization: Bearer $admin' $base/api/v1/plugins/livecapture/api/v1/captures/$opid | grep -q '\"state\":\"active\"'"
docker run --rm --network "${project}_capture-ingress" --cpus 1 --memory 128m --read-only --cap-drop ALL linea-fleet-livecapture-fixture:m4-e2e --endpoint collector-a:4317 >/dev/null || fail "generator"
wait_for 30 "consumer received no records" sh -c "docker logs $c 2>&1 | grep '\"records\"' | tail -1 | \"$python\" -c 'import json,sys;r=json.loads(sys.stdin.read())[\"records\"];sys.exit(0 if r.get(\"traces\",0)>=1 and r.get(\"logs\",0)>=1 and r.get(\"metrics\",0)>=1 else 1)'"
pass "capture targeted by fleet group $group delivered traces, logs and metrics"
rev=$(api GET "v1/captures/$opid" | json "d['revision']")
curl -s -o /dev/null -X POST -H "Authorization: Bearer $admin" -H "If-Match: \"$rev\"" "$base/api/v1/plugins/livecapture/api/v1/captures/$opid/stop"
wait_for 20 "capture did not stop" sh -c "curl -s -H 'Authorization: Bearer $admin' $base/api/v1/plugins/livecapture/api/v1/captures/$opid | grep -Eq '\"state\":\"(stopped|completed)\"'"

# The same peer by fleet selector, once the group capture released it.
sel=$(echo "$body" | "$python" -c 'import json,secrets,sys;d=json.load(sys.stdin);d.pop("group_id");d["selector"]="k8s.cluster.name=s4,k8s.pod.name in (collector-a)";d["id"]=secrets.token_hex(16);print(json.dumps(d))')
op=$(api POST v1/captures "$sel" 202)
opid=$(echo "$op" | json "d['id']")
[ "$(echo "$op" | json "d['targets'][0]['agent_id']")" = "$agent" ] || fail "selector target missing agent_id: $op"
wait_for 20 "selector capture not active" sh -c "curl -s -H 'Authorization: Bearer $admin' $base/api/v1/plugins/livecapture/api/v1/captures/$opid | grep -q '\"state\":\"active\"'"
rev=$(api GET "v1/captures/$opid" | json "d['revision']")
curl -s -o /dev/null -X POST -H "Authorization: Bearer $admin" -H "If-Match: \"$rev\"" "$base/api/v1/plugins/livecapture/api/v1/captures/$opid/stop"
wait_for 20 "selector capture did not stop" sh -c "curl -s -H 'Authorization: Bearer $admin' $base/api/v1/plugins/livecapture/api/v1/captures/$opid | grep -Eq '\"state\":\"(stopped|completed)\"'"
pass "capture targeted by fleet selector admitted and activated the linked peer"

# Core revocation closes the capture stream (and the OpAMP connection).
req DELETE "/api/v1/credentials/$labeled_id" "$admin" '' 204 >/dev/null
start=$SECONDS
wait_for 30 "revoked credential kept its capture stream" sh -c "[ \"\$(curl -s -H 'Authorization: Bearer $admin' $base/api/v1/plugins/livecapture/api/v1/peers | \"$python\" -c 'import json,sys;print(len(json.load(sys.stdin)[\"items\"]))')\" = 0 ]"
closed_after=$((SECONDS - start))
closed=$(api GET v1/metrics | json "d['counters'].get('peers_credential_closed_total',0)")
[ "$closed" -ge 1 ] || fail "credential close not counted"
[ "$(req GET "/api/v1/agents/$agent" "$admin" | json "d['connected']")" = "False" ] || fail "OpAMP agent still connected"
pass "core revocation closed the capture stream in ~${closed_after}s and disconnected OpAMP"
if [ -n "${EXTENSION_SOURCE:-}" ]; then
  echo "extension source: $(git -C "$EXTENSION_SOURCE" rev-parse HEAD) clean=$([ -z "$(git -C "$EXTENSION_SOURCE" status --porcelain)" ] && echo yes || echo no)"
fi
echo "ALL PASSED ($project)"
