#!/usr/bin/env bash
# Config management smoke with the official OpAMP supervisor and the official
# contrib Collector (M6 steps 6a and 6b gates). A fleet with
# --config-management sends layered config through rollouts and checks, with
# one supervised Collector:
#   - a good config is applied and appears in the inventory topology;
#   - the build's component list is stored, and a config using a type the
#     build lacks is withheld by the rollout instead of sent to fail;
#   - an invalid config is FAILED, held, and the old config keeps running;
#   - a config that crashes the Collector is rolled back locally and held,
#     with no resend loop;
#   - a retry after the rollback sends exactly once and is held again;
#   - a supervisor restart causes no resend and keeps the hold; the restarted
#     supervisor reports FAILED for the held hash, so a retry sends nothing;
#   - removing the assignment releases the agent to its local config;
# and with a second supervised Collector whose local config conflicts with
# the next version:
#   - a two-wave rollout releases the canary, bakes it, and pauses when the
#     second wave's Collector fails;
#   - rolling back restores the previous version on both.
# Each single-agent change tolerates one failure, so it completes and the
# held config stays desired.
#
# The first Collector runs from compose.supervised.yaml, the deployment
# overlay, with the fixture supervisor config (shorter apply timeout).
#
# SKIP_BUILD=1 reuses the images; KEEP=1 leaves everything for inspection.
# SUBNET (default 10.247) selects fixed /24 subnets (.205 database, .206
# ingress) so no other project's networks are touched.
set -u
export MSYS_NO_PATHCONV=1
winpath() { (cd "$1" && { pwd -W 2>/dev/null || pwd; }); }
root="$(winpath "$(dirname "$0")/../..")"
python=$(command -v python || command -v python3)
subnet=${SUBNET:-10.247}
id="$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
project="linea-fleet-supervisor-$id"
run="$root/.local/supervisor-$id"
supervisor=""
second="linea-fleet-supervisor-$id-b"
supervisor_image='ghcr.io/lineahq/linea-fleet-supervised-collector:0.163.2'
fixtures="$root/scripts/fleet/fixtures/supervisor"
created=0
main=$$
trap 'exit 1' USR1
fail() { echo "FAIL: $*" >&2; kill -USR1 "$main"; exit 1; }
compose() { docker compose --project-name "$project" --env-file "$run/compose.env" --file "$root/deploy/compose/fleet/compose.yaml" --file "$root/deploy/compose/fleet/compose.supervised.yaml" --file "$run/override.json" "$@"; }
cleanup() {
  [ "${KEEP:-}" = 1 ] && { echo "kept $project ($run)"; return; }
  docker rm --force "$second" >/dev/null 2>&1
  docker volume rm "$second-storage" >/dev/null 2>&1
  [ "$created" = 1 ] && compose down --volumes --remove-orphans --timeout 5 >/dev/null 2>&1
  case "$run" in "$root/.local/supervisor-"*) rm -rf "$run";; esac
}
trap cleanup EXIT
json() { "$python" -c "import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1"; }
quote() { "$python" -c "import json,sys; print(json.dumps(open(sys.argv[1]).read()))" "$1"; }
# req METHOD PATH [BODY] [EXPECTED] [IF-MATCH] -> body, as admin.
req() {
  local method=$1 path=$2 body=${3:-} expected=${4:-200} match=${5:-} code
  local args=(-s -o "$run/resp" -D "$run/headers" -w '%{http_code}' -X "$method" --max-time 8 -H "@$run/admin.header")
  [ -n "$match" ] && args+=(-H "If-Match: $match")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' --data "$body")
  code=$(curl "${args[@]}" "$base$path")
  [ "$code" = "$expected" ] || fail "$method $path returned $code, expected $expected: $(head -c 300 "$run/resp")"
  cat "$run/resp"
}
etag() { sed -n 's/^[Ee][Tt][Aa][Gg]: *//p' "$run/headers" | tr -d '\r'; }
wait_for() { # wait_for SECONDS DESCRIPTION COMMAND...
  local deadline=$((SECONDS + $1)) what=$2; shift 2
  until "$@"; do [ $SECONDS -lt $deadline ] || fail "$what"; sleep 1; done
}
random_id() { od -An -N16 -tx1 /dev/urandom | tr -d ' \n'; }
sends() { req GET /metrics | sed -n 's/^linea_fleet_config_sends_total{path="[a-z]*"} //p' | awk '{s+=$1} END {print s+0}'; }
view() { req GET "/api/v1/agents/$uid/config"; }
check() { [ "$(view | json "$1")" = True ]; }
agent() { [ "$(req GET "/api/v1/agents/$uid" | json "$1")" = True ]; }
# version CONFIG_ID BODY_FILE MESSAGE -> version number
version() { req POST "/api/v1/configs/$1/versions" "{\"body\":$(quote "$2"),\"message\":\"$3\"}" 201 | json 'd["version"]'; }
# Single-agent changes complete even when the agent fails them.
single='"waves":[{"percent":100}],"bake_seconds":0,"max_failures":1'
rollout_is() { [ "$(req GET "/api/v1/rollouts/$1" | json 'd["state"]')" = "$2" ]; }
# rollout ASSIGNMENT CHANGE [POLICY]: start a rollout and wait for it to
# complete. CHANGE is "to_version":N or "delete":true.
rollout() {
  local r
  r=$(req POST /api/v1/rollouts "{\"assignment_id\":\"$1\",$2,${3:-$single}}" 201 | json 'd["id"]')
  wait_for 180 "rollout $r did not complete" rollout_is "$r" completed
}
set_version() { rollout "$assignment" "\"to_version\":$1"; }
desired() { view | json 'd["desired"]["hash"]'; }
# quiet SECONDS DESCRIPTION: no config is sent for that long.
quiet() {
  local before after
  before=$(sends); sleep "$1"; after=$(sends)
  [ "$before" = "$after" ] || fail "$2 ($before -> $after sends)"
}

mkdir -p "$root/.local"
sh "$root/scripts/fleet/new-deployment-secrets.sh" "$run" >/dev/null || fail "secrets"
printf "FLEET_SECRET_DIR=%s\nFLEET_SERVER_ID=supervisor-%s\nFLEET_API_PORT=0\nFLEET_AGENT_PORT=0\nFLEET_SUPERVISOR_CONFIG=%s\nFLEET_COLLECTOR_LOCAL_CONFIG=%s\n" \
  "$run" "$id" "$fixtures/supervisor.yaml" "$fixtures/local.yaml" > "$run/compose.env"
printf '%s\n' "{\"services\": {\"fleet\": {\"command\": [\"--server-id\", \"supervisor-$id\", \"--management-listen\", \"0.0.0.0:8080\", \"--agent-listen\", \"0.0.0.0:4320\", \"--allow-insecure-remote\", \"--config-management\"]}}," \
  " \"networks\": {\"database\": {\"ipam\": {\"config\": [{\"subnet\": \"$subnet.205.0/24\"}]}}, \"ingress\": {\"ipam\": {\"config\": [{\"subnet\": \"$subnet.206.0/24\"}]}}}}" > "$run/override.json"
if [ "${SKIP_BUILD:-}" != 1 ]; then
  compose build fleet collector >/dev/null 2>&1 || fail "build"
fi
created=1
compose up --detach --wait postgres >/dev/null 2>&1 || fail "postgres start"
compose run --rm -T migrate >/dev/null 2>&1 || fail "migrate"
admin=$(compose run --rm -T bootstrap 2>/dev/null | json 'd["token"]' | tr -d '\r')
[ -n "$admin" ] || fail "bootstrap did not disclose a credential"
printf 'Authorization: Bearer %s' "$admin" > "$run/admin.header"
compose up --detach --wait fleet >/dev/null 2>&1 || fail "fleet start"
fleet=$(compose ps --quiet fleet)
base="http://127.0.0.1:$(docker port "$fleet" 8080/tcp | head -n 1 | sed 's/.*://' | tr -d '\r')"
[ "$(req GET /api/v1/server | json 'd["config_management"]')" = True ] || fail "config management not enabled"

expires=$("$python" -c "import datetime; print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(hours=1)).isoformat())")
token=$(req POST /api/v1/credentials "{\"id\":\"$(random_id)\",\"name\":\"supervised\",\"role\":\"agent\",\"remote_config\":true,\"expires_at\":\"$expires\"}" 201 | json 'd["token"]')
LINEA_FLEET_AGENT_TOKEN=$token compose up --detach collector >/dev/null 2>&1 || fail "supervisor start"
supervisor=$(compose ps --quiet collector)
uid=""
registered() {
  uid=$(req GET '/api/v1/agents?connected=true&limit=10' | json "' '.join(a['id'] for a in d['items'] if a.get('health', {}).get('healthy'))")
  [ -n "$uid" ]
}
wait_for 60 "the supervised Collector did not register healthy" registered
check 'd["accepts_remote_config"] and d["credential_remote_config"] and d["desired"] is None' || fail "agent view before assignment"

# 1. A good config is applied and changes the inventory topology.
config=$(random_id) group=$(random_id) assignment=$(random_id)
req POST /api/v1/configs "{\"id\":\"$config\",\"name\":\"smoke\",\"description\":\"supervisor smoke\"}" 201 >/dev/null
good=$(version "$config" "$fixtures/good.yaml" good)
req PUT "/api/v1/groups/$group" "{\"name\":\"supervised\",\"agent_ids\":[\"$uid\"]}" 201 '"0"' >/dev/null
creating=$(req POST /api/v1/config-assignments "{\"id\":\"$assignment\",\"name\":\"base\",\"config_id\":\"$config\",\"version\":$good,\"layer\":10,\"group_id\":\"$group\",\"rollout\":{$single}}" 201 | json 'd["rollout_id"]')
wait_for 180 "the creating rollout did not complete" rollout_is "$creating" completed
good_hash=$(desired)
wait_for 90 "the good config was not applied" check 'd["in_sync"]'
wait_for 30 "the managed pipeline did not appear in the topology" agent "'nop/managed' in json.dumps(d.get('components', [])) and 'traces/local' in json.dumps(d)"
echo "applied: good config $good_hash"

# 1b. The Collector's component list is stored once per build. A version
# using a type the build lacks is not released to it and not sent.
agent "len(d.get('available_components_hash', '')) > 0" || fail "the build hash was not reported"
sets_stored() { [ "$(req GET /metrics | sed -n 's/^linea_fleet_config_component_sets_stored_total //p')" = 1 ]; }
wait_for 30 "the component list was not stored" sets_stored
printf 'receivers:\n  smokemissing: {}\n' > "$run/missing.yaml"
missing=$(version "$config" "$run/missing.yaml" missing)
before=$(sends)
r=$(req POST /api/v1/rollouts "{\"assignment_id\":\"$assignment\",\"to_version\":$missing,$single}" 201 | json 'd["id"]')
wait_for 180 "the incompatible rollout did not complete" rollout_is "$r" completed
[ "$(req GET "/api/v1/rollouts/$r/targets" | json "d['items'][0]['outcome']")" = incompatible ] || fail "the target was not marked incompatible"
check "d['missing_components'] == ['receiver:smokemissing'] and d['reported']['hash'] == '$good_hash' and d['reported']['status'] == 'applied'" ||
  fail "the agent view does not report the missing type: $(view)"
quiet 10 "an incompatible config was sent"
[ "$(sends)" = "$before" ] || fail "an incompatible config was sent"
echo "withheld: version $missing needs receiver:smokemissing"

# 2. An invalid config is FAILED and held; the old config keeps running.
bad=$(version "$config" "$fixtures/invalid.yaml" invalid)
set_version "$bad"
bad_hash=$(desired)
wait_for 60 "the invalid config was not reported failed and held" check "d['reported']['status'] == 'failed' and d['reported']['hash'] == '$bad_hash' and d['reported'].get('held_hash') == '$bad_hash'"
agent "d.get('health', {}).get('healthy') and 'nop/managed' in json.dumps(d.get('components', []))" || fail "the previous config stopped after an invalid config"
quiet 20 "an invalid config was sent again"
echo "held: invalid config failed, previous config still running"

# 3. A config that crashes the Collector is rolled back locally and held.
crash=$(version "$config" "$fixtures/crash.yaml" crash)
set_version "$crash"
crash_hash=$(desired)
rolled_back() { check "d['reported'].get('held_hash') == '$crash_hash' and d['reported']['hash'] != '$crash_hash'"; }
wait_for 120 "the crashing config was not rolled back and held" rolled_back
quiet 30 "the crashing config was sent again after its rollback"
agent "d.get('health', {}).get('healthy')" || fail "the Collector is not healthy after the rollback"
echo "held: crashing config rolled back to $(view | json 'd["reported"]["hash"]')"

# 4. A retry while the agent reports the rolled-back hash sends exactly once,
# and the crash is held again.
before=$(sends)
req POST "/api/v1/agents/$uid/config:retry" '' 204 >/dev/null
retried_and_held() { [ "$(( $(sends) - before ))" -ge 1 ] && rolled_back; }
wait_for 120 "the retried crash was not held again" retried_and_held
quiet 20 "a retried crash was sent again"
[ "$(( $(sends) - before ))" = 1 ] || fail "a retry sent more than once"
echo "held: retried crash sent once and held again"

# 5. A supervisor restart causes no resend and keeps the hold. The restarted
# supervisor reports its persisted FAILED status for the crash hash, so the
# agent already has the desired config and a retry sends nothing.
generation=$(req GET "/api/v1/agents/$uid" | json 'd["generation"]')
docker restart --time 10 "$supervisor" >/dev/null || fail "supervisor restart"
wait_for 60 "the supervisor did not reconnect" agent "d['connected'] and d['generation'] > $generation"
quiet 20 "a supervisor restart caused a resend"
check "d['reported'].get('held_hash') == '$crash_hash'" || fail "the hold did not survive the restart"
check "d['reported']['status'] == 'failed' and d['reported']['hash'] == '$crash_hash'" || fail "the restarted supervisor did not report FAILED for the crash"
req POST "/api/v1/agents/$uid/config:retry" '' 204 >/dev/null
quiet 20 "a retry for a hash reported FAILED sent it again"
agent "d.get('health', {}).get('healthy')" || fail "the Collector is not healthy after the restart"
echo "restarted: no resend, hold kept, retry of a reported FAILED hash sends nothing"

# 6. Removing the assignment releases the agent to its local config.
rollout "$assignment" '"delete":true'
req GET "/api/v1/config-assignments/$assignment" '' 404 >/dev/null
wait_for 90 "the agent was not released" check "d['desired'] is not None and d['desired'].get('unmanaged') and d['in_sync']"
wait_for 30 "the managed pipeline stayed after release" agent "'nop/managed' not in json.dumps(d.get('components', [])) and 'traces/local' in json.dumps(d) and d.get('health', {}).get('healthy')"
echo "released: local config running"

# 7. A two-wave rollout over two Collectors: the first is the canary; the
# second's local config already binds the port the new version binds.
docker run --detach --name "$second" --network "${project}_ingress" --cpus 1 --memory 512m --pids-limit 256 \
  --env "LINEA_FLEET_AGENT_TOKEN=$token" \
  --mount "type=bind,source=$fixtures/supervisor.yaml,target=/etc/otelcol/supervisor.yaml,readonly" \
  --mount "type=bind,source=$fixtures/local-port.yaml,target=/etc/otelcol/local.yaml,readonly" \
  --mount "type=volume,source=$second-storage,target=/home/opampsupervisor" \
  "$supervisor_image" --config /etc/otelcol/supervisor.yaml >/dev/null || fail "second supervisor start"
first=$uid second_uid=""
second_registered() {
  second_uid=$(req GET '/api/v1/agents?connected=true&limit=10' | json "' '.join(a['id'] for a in d['items'] if a.get('health', {}).get('healthy') and a['id'] != '$first')")
  [ -n "$second_uid" ]
}
wait_for 60 "the second supervised Collector did not register healthy" second_registered
both=$(random_id) canary=$(random_id) fleetwide=$(random_id)
req PUT "/api/v1/groups/$both" "{\"name\":\"both\",\"agent_ids\":[\"$first\",\"$second_uid\"]}" 201 '"0"' >/dev/null
req PUT "/api/v1/groups/$canary" "{\"name\":\"canary\",\"agent_ids\":[\"$first\"]}" 201 '"0"' >/dev/null
waves="\"canary_group_id\":\"$canary\",\"waves\":[{\"percent\":100}],\"bake_seconds\":10"
creating=$(req POST /api/v1/config-assignments "{\"id\":\"$fleetwide\",\"name\":\"fleetwide\",\"config_id\":\"$config\",\"version\":$good,\"layer\":10,\"group_id\":\"$both\",\"rollout\":{$waves}}" 201 | json 'd["rollout_id"]')
wait_for 180 "the two-agent creating rollout did not complete" rollout_is "$creating" completed
for uid in "$first" "$second_uid"; do check 'd["in_sync"]' || fail "$uid is not in sync with the good config"; done
fleet_hash=$(desired)
port=$(version "$config" "$fixtures/port.yaml" port)
upgrade=$(req POST /api/v1/rollouts "{\"assignment_id\":\"$fleetwide\",\"to_version\":$port,$waves}" 201 | json 'd["id"]')
paused() { [ "$(req GET "/api/v1/rollouts/$upgrade" | json 'd["state"] + "/" + str(d.get("reason"))')" = paused/threshold ]; }
wait_for 240 "the rollout did not pause when the second wave failed" paused
progress=$(req GET "/api/v1/rollouts/$upgrade")
[ "$(echo "$progress" | json 'd["waves"][0]["outcomes"].get("succeeded", 0)')" = 1 ] || fail "the canary did not succeed: $progress"
[ "$(echo "$progress" | json 'd["waves"][1]["outcomes"].get("failed", 0)')" = 1 ] || fail "the second wave did not fail: $progress"
[ "$(req GET "/api/v1/rollouts/$upgrade/targets?outcome=failed" | json 'd["items"][0]["agent_id"]')" = "$second_uid" ] || fail "the wrong agent failed"
uid=$first
agent "d.get('health', {}).get('healthy') and 'otlp/managed' in json.dumps(d.get('components', []))" || fail "the canary is not running the new version"
echo "paused: canary applied the new version, the second wave failed"
req POST "/api/v1/rollouts/$upgrade:rollback" '' 200 >/dev/null
wait_for 180 "the rollback did not finish" rollout_is "$upgrade" rolled_back
[ "$(req GET "/api/v1/config-assignments/$fleetwide" | json 'd["version"]')" = "$good" ] || fail "the assignment left its version"
for uid in "$first" "$second_uid"; do
  wait_for 90 "$uid did not return to the previous version" check "d['in_sync'] and d['desired']['hash'] == '$fleet_hash'"
  agent "d.get('health', {}).get('healthy') and 'otlp/managed' not in json.dumps(d.get('components', []))" || fail "$uid still runs the new version"
done
echo "rolled back: both Collectors run the previous version"

logs=$(docker logs "$fleet" 2>&1)
for value in "$admin" "$token" "nosuchreceiver" "nop/managed" "0.0.0.0:4317"; do
  case "$logs" in *"$value"*) fail "secret or config material appeared in fleet logs";; esac
done
events="" cursor=""
for _ in $(seq 20); do
  req GET "/api/v1/events?limit=500${cursor:+&cursor=$cursor}" > "$run/events"
  events="$events $(json "' '.join(sorted({e['kind'] for e in d['items'] if e['kind'].startswith(('config.', 'rollout.'))}))" < "$run/events")"
  cursor=$(json 'd["next_cursor"]' < "$run/events")
  [ "$(json 'len(d["items"])' < "$run/events")" -lt 500 ] && break
done
for kind in config.created config.version_created config.assignment_changed config.held config.hold_cleared \
  rollout.created rollout.wave_released rollout.completed rollout.paused rollout.rolling_back rollout.rolled_back; do
  case " $events " in *" $kind "*) ;; *) fail "missing event $kind (have: $events)";; esac
done
echo 'PASS: official supervisor with the contrib Collector: good config applied, invalid config failed and held, crashing config rolled back and held without a loop, no resend after a supervisor restart, retry sent once, release to the local config, a two-wave rollout paused by a failing second wave and rolled back, no secrets or config text in logs.'
