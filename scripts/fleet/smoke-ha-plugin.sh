#!/usr/bin/env bash
# Plugin behind several fleet replicas: compose.ha.yaml (REPLICAS replicas,
# HAProxy, a PostgreSQL standby) with the example plugin, whose host requests
# go through the load balancer's plugin_host frontend.
#
# Checks, all through the load balancer:
#   1. every replica shows the plugin ready under one epoch;
#   2. the plugin's public API answers;
#   3. killing one replica changes neither the epoch nor readiness, and the
#      plugin keeps answering (its session poll reaches a survivor);
#   4. a drain sent to one replica is shown by every replica, with one new
#      epoch, and the plugin's API stops answering;
#   5. enable makes it ready again on every replica under one new epoch.
#
# Needs Docker. Runs about two minutes; not on a shared or constrained
# machine. REPLICAS (2-8, default 2), SUBNET (default 10.246, uses .1 to .4
# /24), SKIP_BUILD=1 reuses the images, KEEP=1 leaves everything running.
set -u
export MSYS_NO_PATHCONV=1
winpath() { (cd "$1" && { pwd -W 2>/dev/null || pwd; }); }
root="$(winpath "$(dirname "$0")/../..")"
python=$(command -v python || command -v python3)
replicas=${REPLICAS:-2} subnet=${SUBNET:-10.246}
[ "$replicas" -ge 2 ] && [ "$replicas" -le 8 ] || { echo "REPLICAS must be 2-8" >&2; exit 2; }
id="$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
project="linea-fleet-ha-plugin-$id"
run="$root/.local/docker-$id"
plugin=agent-summary
created=0
main=$$
trap 'exit 1' USR1
fail() { echo "FAIL: $*" >&2; kill -USR1 "$main"; exit 1; }
note() { echo "$(date +%H:%M:%S) $*"; }
compose() {
  docker compose --project-name "$project" --env-file "$run/compose.env" \
    --file "$root/deploy/compose/fleet/compose.yaml" --file "$root/deploy/compose/fleet/compose.ha.yaml" \
    --file "$root/examples/fleet-agent-summary-plugin/compose.yaml" --file "$root/examples/fleet-agent-summary-plugin/compose.ha.yaml" \
    --file "$run/override.json" "$@"
}
cleanup() {
  [ "${KEEP:-}" = 1 ] && { echo "kept $project ($run)"; return; }
  [ "$created" = 1 ] && {
    compose logs --no-color --timestamps fleet lb agent-summary-plugin 2>&1 | tail -n 300 > "$run.log"
    compose down --volumes --remove-orphans --timeout 5 >/dev/null 2>&1; }
  case "$run" in "$root/.local/docker-"*) rm -rf "$run";; esac
}
trap cleanup EXIT
json() { "$python" -c "import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1"; }
# req METHOD PATH [BODY] [EXPECTED] [IF-MATCH] -> body, as the admin.
req() {
  local method=$1 path=$2 body=${3:-} expected=${4:-200} match=${5:-} code
  local args=(-s -o "$run/resp" -w '%{http_code}' -X "$method" --max-time 8 -H "@$run/admin.header")
  [ -n "$match" ] && args+=(-H "If-Match: $match")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' --data "$body")
  code=$(curl "${args[@]}" "$base$path")
  [ "$code" = "$expected" ] || fail "$method $path returned $code, expected $expected (body omitted)"
  cat "$run/resp"
}
status() { curl -s -o /dev/null -w '%{http_code}' --max-time 8 -H "@$run/admin.header" "$base$1"; }
wait_for() { # wait_for SECONDS DESCRIPTION COMMAND...
  local deadline=$((SECONDS + $1)) what=$2; shift 2
  until "$@"; do [ $SECONDS -lt $deadline ] || fail "$what"; sleep 0.5; done
}
# views prints "state epoch" as seen by 4 * REPLICAS requests, which the load
# balancer spreads over every replica, once per distinct answer.
views() {
  for _ in $(seq $((4 * replicas))); do
    req GET "/api/v1/plugins/$plugin" | json 'd["state"] + " " + d["epoch"]'
  done | tr -d '\r' | sort -u
}
# agreed STATE: every replica shows STATE under one epoch; sets epoch.
agreed() {
  local seen
  seen=$(views)
  [ "$(printf '%s\n' "$seen" | wc -l | tr -d ' ')" = 1 ] || return 1
  [ "${seen%% *}" = "$1" ] || return 1
  epoch=${seen#* }
}
summary_ok() { [ "$(status "/api/v1/plugins/$plugin/api/v1/summary")" = 200 ]; }

mkdir -p "$root/.local"
sh "$root/scripts/fleet/new-deployment-secrets.sh" "$run" >/dev/null || fail "secrets"
sh "$root/scripts/fleet/new-plugin-secrets.sh" "$run/summary" >/dev/null || fail "plugin secrets"
printf "FLEET_SECRET_DIR=%s\nFLEET_SUMMARY_SECRET_DIR=%s\nFLEET_SERVER_ID=ha-plugin-%s\nFLEET_REPLICAS=%s\nFLEET_API_PORT=0\nFLEET_AGENT_PORT=0\nFLEET_BOOTSTRAP_PLUGIN_ACCESS=agent-summary=admin\n" \
  "$run" "$run/summary" "$id" "$replicas" > "$run/compose.env"
cat > "$run/override.json" <<EOF
{"networks": {"database": {"ipam": {"config": [{"subnet": "$subnet.1.0/24"}]}}, "ingress": {"ipam": {"config": [{"subnet": "$subnet.2.0/24"}]}}, "plugins": {"ipam": {"config": [{"subnet": "$subnet.3.0/24"}]}}}}
EOF
[ "${SKIP_BUILD:-}" = 1 ] || compose build fleet agent-summary-plugin >/dev/null 2>&1 || fail "build"
created=1
compose up --detach --wait postgres postgres-standby >/dev/null 2>&1 || fail "postgres start"
compose run --rm -T migrate >/dev/null 2>&1 || fail "migrate"
admin=$(compose run --rm -T bootstrap 2>/dev/null | json 'd["token"]' | tr -d '\r')
[ -n "$admin" ] || fail "bootstrap did not disclose a credential"
printf 'Authorization: Bearer %s' "$admin" > "$run/admin.header"
compose up --detach --wait fleet lb agent-summary-plugin >/dev/null 2>&1 || fail "fleet, load balancer or plugin start"
lb=$(compose ps --quiet lb)
base="http://127.0.0.1:$(docker port "$lb" 8080/tcp | head -n 1 | sed 's/.*://' | tr -d '\r')"
[ "$(compose ps --quiet fleet | wc -l | tr -d ' ')" = "$replicas" ] || fail "expected $replicas fleet replicas"

note "1. one epoch on every replica"
epoch=
wait_for 30 "the plugin did not become ready under one epoch on every replica" agreed ready
first=$epoch
note "2. the plugin's API answers through any replica"
wait_for 30 "the plugin's summary did not answer" summary_ok

note "3. a lost replica changes nothing"
victim=$(compose ps --quiet fleet | head -n 1)
docker kill --signal SIGKILL "$victim" >/dev/null || fail "kill a replica"
# Longer than the 3 s seen window and the plugin's 2 s session tolerance.
sleep 6
agreed ready || fail "the survivors do not agree on a ready plugin"
[ "$epoch" = "$first" ] || fail "losing one replica rotated the epoch"
for _ in $(seq 10); do summary_ok || fail "the plugin stopped answering after one replica died"; done
docker start "$victim" >/dev/null || fail "restart the killed replica"
wait_for 30 "the restarted replica did not agree" agreed ready

note "4. a drain on one replica is seen by all"
rev=$(req GET "/api/v1/plugins/$plugin" | json 'd["revision"]' | tr -d '\r')
req POST "/api/v1/plugins/$plugin/lifecycle" '{"action":"drain"}' 200 "\"$rev\"" >/dev/null
wait_for 3 "a drain was not shown by every replica within the 500 ms sweep" agreed draining
[ "$epoch" != "$first" ] || fail "a drain kept the epoch"
drained=$epoch
not_answering() { [ "$(status "/api/v1/plugins/$plugin/api/v1/summary")" = 503 ]; }
wait_for 5 "the drained plugin still answered" not_answering

note "5. enable restores one new epoch everywhere"
rev=$(req GET "/api/v1/plugins/$plugin" | json 'd["revision"]' | tr -d '\r')
req POST "/api/v1/plugins/$plugin/lifecycle" '{"action":"enable"}' 200 "\"$rev\"" >/dev/null
wait_for 30 "the re-enabled plugin did not become ready under one epoch" agreed ready
[ "$epoch" != "$drained" ] || fail "enable kept the drained epoch"
wait_for 30 "the re-enabled plugin's summary did not answer" summary_ok
note "PASS"
