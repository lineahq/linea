#!/usr/bin/env bash
# Docker deployment smoke with the real Collector distribution: narrow database
# grants, two Collectors sharing one bearer token/config with automatic UIDs,
# a third Collector with a failing component (unhealthy health in PostgreSQL
# inventory), HTTP administration, crash recovery/reconnect with a surviving
# event cursor, shared-token revocation, dump/restore into a fresh database
# and shutdown on database loss.
#
# SKIP_BUILD=1 reuses the fleet image; KEEP=1 leaves everything for inspection. SUBNET (default 10.249) selects fixed
# /24 subnets (.1 database, .2 ingress) for hosts with exhausted address pools.
set -u
export MSYS_NO_PATHCONV=1
winpath() { (cd "$1" && { pwd -W 2>/dev/null || pwd; }); }
root="$(winpath "$(dirname "$0")/../..")"
python=$(command -v python || command -v python3)
subnet=${SUBNET:-10.249}
id="$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
project="linea-fleet-smoke-$id"
run="$root/.local/docker-$id"
collectors=("linea-fleet-collector-$id" "linea-fleet-replica-$id" "linea-fleet-unhealthy-$id")
collector_image='otel/opentelemetry-collector-contrib:0.162.0@sha256:39923a8e431bd1f57be82411999d389fcfe40857492e4365456d97a4c1f74be6'
created=0
main=$$
trap 'exit 1' USR1
fail() { echo "FAIL: $*" >&2; kill -USR1 "$main"; exit 1; }
compose() { docker compose --project-name "$project" --env-file "$run/compose.env" --file "$root/deploy/compose/fleet/compose.yaml" --file "$run/override.json" "$@"; }
cleanup() {
  [ "${KEEP:-}" = 1 ] && { echo "kept $project ($run)"; return; }
  for c in "${collectors[@]}"; do docker rm --force "$c" >/dev/null 2>&1; done
  [ "$created" = 1 ] && compose down --volumes --remove-orphans --timeout 5 >/dev/null 2>&1
  case "$run" in "$root/.local/docker-"*) rm -rf "$run";; esac
}
trap cleanup EXIT
json() { "$python" -c "import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1"; }
# req METHOD PATH ROLE [BODY] [EXPECTED] [IF-MATCH] -> body; ROLE names a header file or "none".
req() {
  local method=$1 path=$2 role=$3 body=${4:-} expected=${5:-200} match=${6:-} code
  local args=(-s -o "$run/resp" -w '%{http_code}' -X "$method" --max-time 8)
  [ "$role" != none ] && args+=(-H "@$run/$role.header")
  [ -n "$match" ] && args+=(-H "If-Match: $match")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' --data "$body")
  code=$(curl "${args[@]}" "$base$path")
  [ "$code" = "$expected" ] || fail "$method $path returned $code, expected $expected (body omitted)"
  cat "$run/resp"
}
wait_for() { # wait_for SECONDS DESCRIPTION COMMAND...
  local deadline=$((SECONDS + $1)) what=$2; shift 2
  until "$@"; do [ $SECONDS -lt $deadline ] || fail "$what"; sleep 0.5; done
}
locate() {
  fleet=$(compose ps --quiet fleet)
  base="http://127.0.0.1:$(docker port "$fleet" 8080/tcp | head -n 1 | sed 's/.*://' | tr -d '\r')"
}
header() { printf 'Authorization: Bearer %s' "$2" > "$run/$1.header"; }
secret() { sed 's/^Authorization: Bearer //' "$run/$1.header"; }
collector() { # collector NAME CONFIG
  docker run --detach --name "$1" --cpus 1 --memory 256m --read-only --network "${project}_ingress" \
    --env "LINEA_FLEET_AGENT_TOKEN=$(secret agent)" --env LINEA_FLEET_OPAMP_ENDPOINT=ws://fleet:4320/v1/opamp \
    --mount "type=bind,source=$2,target=/etc/otelcol-contrib/config.yaml,readonly" "$collector_image" >/dev/null || fail "Collector $1 failed to start"
}
connected_count() { req GET '/api/v1/agents?connected=true&limit=10' reader | json "len(d['items'])"; }

mkdir -p "$root/.local"
sh "$root/scripts/fleet/new-deployment-secrets.sh" "$run" >/dev/null || fail "secrets"
printf "FLEET_SECRET_DIR=%s\nFLEET_SERVER_ID=smoke-%s\nFLEET_API_PORT=0\nFLEET_AGENT_PORT=0\n" "$run" "$id" > "$run/compose.env"
cat > "$run/override.json" <<EOF
{"networks": {"database": {"ipam": {"config": [{"subnet": "$subnet.1.0/24"}]}}, "ingress": {"ipam": {"config": [{"subnet": "$subnet.2.0/24"}]}}}}
EOF
[ "${SKIP_BUILD:-}" = 1 ] || compose build fleet >/dev/null 2>&1 || fail "build"
created=1
compose up --detach --wait postgres >/dev/null 2>&1 || fail "postgres start"
compose run --rm -T migrate >/dev/null 2>&1 || fail "migrate"
admin=$(compose run --rm -T bootstrap 2>/dev/null | json 'd["token"]' | tr -d '\r')
[ -n "$admin" ] || fail "bootstrap did not disclose a credential"
header admin "$admin"
compose up --detach --wait fleet >/dev/null 2>&1 || fail "fleet start"
locate
[ "$(docker inspect "$fleet" --format '{{.Config.User}}|{{.HostConfig.ReadonlyRootfs}}')" = '65532:65532|true' ] || fail "fleet container hardening was not applied"
[ "$(req GET /api/v1/server admin | json 'd["storage"] == "postgres" and len(d["plugins"]) == 0')" = True ] || fail "standalone discovery"
[ "$(req GET /api/v1/plugins admin | json 'len(d["items"])')" = 0 ] || fail "plugin discovery count"
req GET /api/v1/agents none '' 401 >/dev/null
expires=$("$python" -c "import datetime; print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(hours=1)).isoformat())")
reader_id=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
agent_id=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
header reader "$(req POST /api/v1/credentials admin "{\"id\":\"$reader_id\",\"name\":\"docker-reader\",\"role\":\"reader\",\"expires_at\":\"$expires\"}" 201 | json 'd["token"]')"
agent_body="{\"id\":\"$agent_id\",\"name\":\"docker-deployment\",\"role\":\"agent\",\"expires_at\":\"$expires\"}"
header agent "$(req POST /api/v1/credentials admin "$agent_body" 201 | json 'd["token"]')"
req GET /api/v1/credentials reader '' 403 >/dev/null
req GET /api/v1/agents agent '' 401 >/dev/null
req POST /api/v1/credentials admin "$agent_body" 409 >/dev/null
flags=$(compose exec -T postgres sh -c 'PGPASSWORD="$(cat /run/secrets/runtime_password)" psql -h127.0.0.1 -U fleet_app -d fleet -Atc "select has_schema_privilege(current_user, '"'fleet'"', '"'CREATE'"'), has_table_privilege(current_user, '"'fleet.schema_version'"', '"'UPDATE'"'), has_table_privilege(current_user, '"'fleet.agents'"', '"'INSERT'"')"' | tr -d '\r')
[ "$flags" = 'f|f|t' ] || fail "runtime database permissions were broader than expected ($flags)"

# Same deployment token and config; neither Collector is given an instance UID.
collector "${collectors[0]}" "$root/examples/fleet-collector.yaml"
collector "${collectors[1]}" "$root/examples/fleet-collector.yaml"
two_healthy() { [ "$(req GET '/api/v1/agents?connected=true&limit=10' reader | json "len(d['items']) == 2 and all(a.get('health', {}).get('healthy') for a in d['items'])")" = True ]; }
wait_for 40 "two real Collectors did not register with the shared deployment token" two_healthy
uids=$(req GET '/api/v1/agents?connected=true&limit=10' reader | json "' '.join(a['id'] for a in d['items'])")
set -- $uids
[ "$1" != "$2" ] || fail "Collectors did not generate independent instance UIDs"
uid=$1 peer=$2
first_generation=$(req GET "/api/v1/agents/$uid" reader | json 'd["generation"]')

# M2: a real Collector whose exporter cannot deliver reports an unhealthy
# component through OpAMP health, persisted in PostgreSQL inventory.
collector "${collectors[2]}" "$root/scripts/fleet/fixtures/collector-unhealthy.yaml"
unhealthy_uid=""
find_unhealthy() {
  unhealthy_uid=$(req GET '/api/v1/agents?connected=true&limit=10' reader | json "' '.join(a['id'] for a in d['items'] if a['id'] not in ('$uid', '$peer'))")
  [ -n "$unhealthy_uid" ] || return 1
  [ "$(req GET "/api/v1/agents/$unhealthy_uid" reader | json "any(c.get('healthy') is False for c in d.get('components', []))")" = True ]
}
wait_for 90 "the failing component was not reported unhealthy" find_unhealthy
req GET "/api/v1/agents/$unhealthy_uid" reader | json "json.dumps({'healthy': d.get('health', {}).get('healthy'), 'unhealthy': [(c['id'], c.get('status')) for c in d['components'] if c.get('healthy') is False]})" | sed 's/^/unhealthy collector: /'
docker rm --force "${collectors[2]}" >/dev/null

group_id=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
group_body="{\"name\":\"docker-group\",\"agent_ids\":[\"$uid\"]}"
req PUT "/api/v1/groups/$group_id" admin "$group_body" 201 '"0"' >/dev/null
req PUT "/api/v1/groups/$group_id" admin "$group_body" 412 '"0"' >/dev/null
cursor=$(req GET /api/v1/events reader | json 'd["next_cursor"]')

# Pause both processes to keep their automatic UIDs across the server crash.
docker pause "${collectors[0]}" "${collectors[1]}" >/dev/null || fail "Collector pause"
compose kill --signal SIGKILL fleet >/dev/null 2>&1 || fail "kill"
compose up --detach --wait fleet >/dev/null 2>&1 || fail "restart"
locate
crashed_disconnected() { [ "$(req GET "/api/v1/agents/$uid" reader | json 'd["presence"] == "disconnected" and not d["connected"]')" = True ]; }
wait_for 40 "crashed replica agents did not become disconnected" crashed_disconnected
[ "$(req GET "/api/v1/agents/$uid" reader | json 'd.get("health", {}).get("healthy")')" = True ] || fail "crash recovery lost historical health"
req GET "/api/v1/events?cursor=$cursor" reader >/dev/null
[ "$(req GET "/api/v1/groups/$group_id" reader | json 'd["member_count"]')" = 1 ] || fail "group did not survive restart"
docker unpause "${collectors[0]}" "${collectors[1]}" >/dev/null || fail "Collector resume"
reconnected() { [ "$(connected_count)" = 2 ] && [ "$(req GET "/api/v1/agents/$uid" reader | json "d['connected'] and d['generation'] > $first_generation")" = True ]; }
wait_for 60 "both Collectors did not reconnect after server recovery" reconnected
req DELETE "/api/v1/credentials/$agent_id" admin '' 204 >/dev/null
none_connected() { [ "$(connected_count)" = 0 ]; }
wait_for 40 "deployment token revocation did not disconnect both Collectors" none_connected

# Restore a custom-format dump into a fresh database, preserving owners and
# grants. Never restore into or remove an existing user database.
compose stop --timeout 10 fleet >/dev/null 2>&1 || fail "stop fleet"
compose exec -T postgres pg_dump -U postgres -d fleet -Fc -f /tmp/fleet-backup.dump || fail "dump"
compose exec -T postgres createdb -U postgres -O fleet_migrator restored || fail "createdb"
compose exec -T postgres pg_restore -U postgres --exit-on-error --dbname restored /tmp/fleet-backup.dump || fail "restore"
compose exec -T postgres psql -U postgres -d restored -v ON_ERROR_STOP=1 -c 'REVOKE ALL ON DATABASE restored FROM PUBLIC; GRANT CONNECT ON DATABASE restored TO fleet_app,fleet_migrator;' >/dev/null || fail "grants"
chmod 644 "$run/runtime_url"
runtime_url=$(sed 's#/fleet?#/restored?#' "$run/runtime_url")
printf '%s' "$runtime_url" > "$run/runtime_url"
chmod 444 "$run/runtime_url"
compose up --detach --wait --force-recreate fleet >/dev/null 2>&1 || fail "restart on restored database"
locate
[ "$(req GET "/api/v1/groups/$group_id" reader | json 'd["member_count"]')" = 1 ] || fail "restore lost group membership"
[ "$(req GET "/api/v1/credentials/$agent_id" admin | json 'bool(d.get("revoked_at"))')" = True ] || fail "restore lost credential revocation"
logs=$(docker logs "$fleet" 2>&1)
for value in "$admin" "$(secret reader)" "$(secret agent)" "$runtime_url"; do
  case "$logs" in *"$value"*) fail "secret material appeared in fleet logs";; esac
done
compose stop --timeout 5 postgres >/dev/null 2>&1 || fail "stop postgres"
exited() { [ "$(docker inspect "$fleet" --format '{{.State.Status}}')" = exited ]; }
wait_for 40 "fleet did not stop after database availability loss" exited
echo 'PASS: Docker deployment, narrow database grants, two real Collectors sharing one bearer token/config with automatic UIDs, an unhealthy component from a real Collector in PostgreSQL inventory, HTTP administration, crash recovery/reconnect with a surviving cursor, shared-token revocation, dump/restore and database-outage shutdown.'
