#!/usr/bin/env bash
# Multi-replica profile (M5 gates): REPLICAS identical fleet replicas behind
# the compose.ha.yaml TCP load balancer, PostgreSQL with a streaming standby,
# and synthetic OpAMP sessions from one bounded simulator container.
# After ramp, steady heartbeats and planned reconnects it runs three outages:
#   1. replica kill: SIGKILL one replica; only its sessions may reconnect,
#      all to the others, within 60 s; the replica is then restarted.
#   2. takeover fencing: freeze one replica until the leader reaps it, then
#      thaw it; it must fence itself without reclaiming any agent, and its
#      sessions reconnect elsewhere within 60 s.
#   3. database failover: stop the primary, promote the standby; replicas
#      follow the multi-host URL, a pre-failover event cursor still works, and
#      every session is connected afterwards.
# Official opamp-go clients set up like the Collector's OpAMP extension, one
# per session; no Collector pipelines are involved.
#
# ROLLOUT=1 (M6 6c gate) runs the replicas with --config-management and the
# sessions like supervised Collectors (-accept-config, four builds, 5 % on a
# minimal build without filelog). A four-wave creating rollout of version 1
# runs through the replica kill, and a four-wave rollout of version 2, which
# uses filelog, through the database failover; each storm window stays open
# until its rollout completes. Gates: both complete with every compatible
# target succeeded, the minimal build incompatible, nothing failed or held;
# every session is offered each config it can run exactly once; and no
# replica's asynchronous config sends exceed the 200/s pacing bound.
#
# Run one step at a time and never two profiles at once. Every container has
# hard CPU/memory caps; the run stops at the first breached threshold.
#
# AGENTS (1-20000, default 3000)  REPLICAS (2-8, 3)  HEARTBEAT (s, 30)  STEADY (s, 60)
# CONNECT_RATE (/s, 500)          PAYLOAD_BYTES (1024)  STORM_TIMEOUT (s, 180)
# FLEET_MEMORY (1g), FLEET_GOMEMLIMIT (896MiB), FLEET_GODEBUG (e.g. gctrace=1), SIM_MEMORY (1g, 2g above 10,000)
# SIM_CPUS (2, 4 above 10,000): one opamp-go client per session
# SUBNET (10.247, uses .1 and .2 /24), TAG (saturation), SKIP_BUILD=1, KEEP=1
# ROLLOUT=1 (STORM_TIMEOUT then defaults to 600)
set -u
export MSYS_NO_PATHCONV=1
winpath() { (cd "$1" && { pwd -W 2>/dev/null || pwd; }); }
root="$(winpath "$(dirname "$0")/../..")"
python=$(command -v python || command -v python3)
agents=${AGENTS:-3000} replicas=${REPLICAS:-3} heartbeat=${HEARTBEAT:-30} steady=${STEADY:-60}
rollout=${ROLLOUT:-0}
connect_rate=${CONNECT_RATE:-500} payload=${PAYLOAD_BYTES:-1024} storm_timeout=${STORM_TIMEOUT:-$( [ "$rollout" = 1 ] && echo 600 || echo 180 )}
fleet_memory=${FLEET_MEMORY:-1g} fleet_gomemlimit=${FLEET_GOMEMLIMIT:-896MiB} fleet_godebug=${FLEET_GODEBUG:-}
sim_memory=${SIM_MEMORY:-$( [ "$agents" -gt 10000 ] && echo 2g || echo 1g )}
sim_cpus=${SIM_CPUS:-$( [ "$agents" -gt 10000 ] && echo 4 || echo 2 )}
subnet=${SUBNET:-10.247} tag=${TAG:-saturation}
[ "$replicas" -ge 2 ] && [ "$replicas" -le 8 ] || { echo "REPLICAS must be 2-8" >&2; exit 2; }
# Every replica can absorb the sessions of a lost peer, up to the 10,000 cap.
max_connections=$(( agents / (replicas - 1) + 64 ))
[ $max_connections -le 10000 ] || max_connections=10000
max_agents=$(( agents * 2 > 1000 ? agents * 2 : 1000 ))
id="$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
project="fleet-ha-$id"
run="$root/.local/$project"
sim="$project-sim"
created=0 started=0 pgsvc=postgres sampler=
main=$$
trap 'exit 1' USR1
fail() { echo "FAIL: $*" >&2; record failure "\"$*\""; kill -USR1 "$main"; exit 1; }
note() { echo "$(date +%H:%M:%S) $*"; }
compose() { docker compose --project-name "$project" --env-file "$run/compose.env" --file "$root/deploy/compose/fleet/compose.yaml" --file "$root/deploy/compose/fleet/compose.ha.yaml" --file "$run/override.json" "$@"; }
cleanup() {
  [ -n "$sampler" ] && kill "$sampler" 2>/dev/null
  [ "$started" = 1 ] && {
    docker logs "$sim" > "$run/simulator.out" 2>&1
    grep '^{' "$run/simulator.out" > "$run/observations.jsonl"
    grep -v '^{' "$run/simulator.out" | tail -n 50 > "$run/simulator.log"; rm -f "$run/simulator.out"
    docker rm --force "$sim" >/dev/null 2>&1; }
  [ "${KEEP:-}" = 1 ] && { echo "kept $project"; return; }
  [ "$created" = 1 ] && {
    for c in $(compose ps --all --quiet fleet 2>/dev/null); do docker unpause "$c" >/dev/null 2>&1; done
    compose logs --no-color --timestamps fleet lb postgres postgres-standby 2>&1 | tail -n 400 > "$run/compose.log"
    compose down --volumes --remove-orphans --timeout 5 >/dev/null 2>&1; }
  echo "results: $run/result.json"
}
trap cleanup EXIT
record() {
  "$python" - "$run/result.json" "$1" "$2" <<'EOF'
import json, os, sys
path, key, value = sys.argv[1:]
data = json.load(open(path)) if os.path.exists(path) else {}
data[key] = json.loads(value)
open(path, "w").write(json.dumps(data, indent=2, sort_keys=True))
EOF
}
json() { "$python" -c "import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1"; }
port() { docker port "$1" "$2/tcp" 2>/dev/null | head -n 1 | sed 's/.*://' | tr -d '\r'; }
# get URL [EXPECTED] -> body; the token never appears in output.
get() {
  local code
  code=$(curl -s -o "$run/resp" -w '%{http_code}' -H "@$run/auth_header" --max-time 15 "$1")
  [ "$code" = "${2:-200}" ] || return 1
  cat "$run/resp"
}
req() { # req METHOD PATH [BODY] [EXPECTED] through the load balancer
  local method=$1 path=$2 body=${3:-} expected=${4:-200} code
  local args=(-s -o "$run/resp" -w '%{http_code}' -X "$method" -H "@$run/auth_header" --max-time 15)
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' --data "$body")
  code=$(curl "${args[@]}" "$base$path")
  [ "$code" = "$expected" ] || fail "$method $path returned $code, expected $expected"
  cat "$run/resp"
}
# connected -> count; empty while the API answers 503 (e.g. during failover),
# so polling loops keep waiting instead of aborting the run.
connected() { get "$base/api/v1/summary" | json 'd["connected"]' 2>/dev/null; }
metric() { # metric FILE NAME -> value (0 when absent)
  "$python" -c "
import re,sys
m=re.search(r'(?m)^'+re.escape(sys.argv[2])+r' ([0-9.eE+-]+)\s*$', open(sys.argv[1]).read())
print(float(m.group(1)) if m else 0)" "$1" "$2"
}
psql_json() { compose exec -T "$pgsvc" psql -U postgres -d fleet -Atc "$1"; }
# replicas PHASE: per running replica, its replica ID, sessions and memory,
# written to PHASE.replicas.jsonl; enforces the memory budgets: the heap live
# at the last GC plus goroutine stacks stay within 64 KiB per session, and all
# Go memory stays within GOMEMLIMIT. Total Go memory per session is not the
# budget: it includes garbage awaiting the next GC, up to GOGC times the live
# heap, so it doubles after a reconnect storm without any session costing more.
replicas() {
  local c p rid
  : > "$run/$1.replicas.jsonl"
  for c in $(compose ps --quiet fleet); do
    p=$(port "$c" 8080); [ -n "$p" ] || continue
    # A replica restarting between listing and sampling is skipped.
    rid=$(get "http://127.0.0.1:$p/api/v1/server" 2>/dev/null | json 'd.get("replica_id","")' 2>/dev/null) && [ -n "$rid" ] || continue
    get "http://127.0.0.1:$p/metrics" > "$run/$1.$c.prom" || continue
    printf '{"container": "%s", "replica_id": "%s", "connections": %s, "go_memory_bytes": %s, "go_heap_live_bytes": %s, "go_stack_bytes": %s, "writer_items": %s, "report_errors": %s, "reports": %s, "restarts": %s}\n' \
      "$c" "$rid" "$(metric "$run/$1.$c.prom" linea_fleet_agent_connections)" "$(metric "$run/$1.$c.prom" linea_fleet_go_memory_bytes)" \
      "$(metric "$run/$1.$c.prom" linea_fleet_go_heap_live_bytes)" "$(metric "$run/$1.$c.prom" linea_fleet_go_stack_bytes)" \
      "$(metric "$run/$1.$c.prom" linea_fleet_writer_batch_items_total)" "$(metric "$run/$1.$c.prom" linea_fleet_report_errors_total)" \
      "$(metric "$run/$1.$c.prom" linea_fleet_report_duration_seconds_count)" "$(docker inspect "$c" --format '{{.RestartCount}}')" >> "$run/$1.replicas.jsonl"
  done
  "$python" - "$run/$1.replicas.jsonl" "$fleet_gomemlimit" <<'EOF' || fail "memory budget exceeded at $1"
import json, re, sys
n, unit = re.fullmatch(r"(\d+)(B|KiB|MiB|GiB)", sys.argv[2]).groups()
limit = int(n) << {"B": 0, "KiB": 10, "MiB": 20, "GiB": 30}[unit]
for r in map(json.loads, open(sys.argv[1])):
    held = r["go_heap_live_bytes"] + r["go_stack_bytes"]
    if r["go_memory_bytes"] > limit or (r["connections"] >= 1000 and held / r["connections"] > 65536):
        sys.exit(1)
EOF
  record "replicas_$1" "[$(paste -sd, "$run/$1.replicas.jsonl")]"
  note "$1: sessions per replica $("$python" -c "import json,sys; print([int(json.loads(l)['connections']) for l in open(sys.argv[1])])" "$run/$1.replicas.jsonl")"
}
field() { # field PHASE CONTAINER KEY
  "$python" -c "
import json,sys
for l in open(sys.argv[1]):
    r=json.loads(l)
    if r['container']==sys.argv[2]: print(r[sys.argv[3]])" "$run/$1.replicas.jsonl" "$2" "$3"
}
pgstats() { # pgstats PHASE
  psql_json "SELECT json_build_object('commits', (SELECT xact_commit FROM pg_stat_database WHERE datname = 'fleet'),
      'writes', (SELECT sum(n_tup_ins + n_tup_upd + n_tup_del) FROM pg_stat_user_tables WHERE schemaname = 'fleet'),
      'agents', (SELECT count(*) FROM fleet.agents), 'events', (SELECT count(*) FROM fleet.events),
      'owners', (SELECT count(DISTINCT owner_replica) FROM fleet.agents WHERE owner_replica IS NOT NULL),
      'at', extract(epoch FROM clock_timestamp()))" > "$run/$1.pg.json" || fail "PostgreSQL counters for $1"
}
release() { : > "$run/checkpoints/$1"; }
# phase NAME TIMEOUT [STORM] -> the simulator's observation line for NAME.
phase() {
  local deadline=$((SECONDS + $2)) line running
  while :; do
    # Read the state before the logs: a simulator that exits in between
    # has already written its last line.
    running=$(docker inspect "$sim" --format '{{.State.Running}}' 2>/dev/null)
    line=$(docker logs "$sim" 2>/dev/null | "$python" -c "
import json,sys
want, storm = sys.argv[1], int(sys.argv[2])
for raw in sys.stdin:
    if not raw.startswith('{'): continue
    o=json.loads(raw)
    if o.get('phase')=='stopped' and not o.get('success'):
        print('FAILED '+str(o.get('failure'))); break
    if o.get('phase')==want and (storm==0 or o.get('storm')==storm):
        print(raw.strip()); break" "$1" "${3:-0}")
    case "$line" in
      FAILED*) fail "simulator: ${line#FAILED }";;
      ?*) echo "$line"; return;;
    esac
    [ "$running" = true ] || fail "simulator stopped before $1"
    [ $SECONDS -lt $deadline ] || fail "timed out waiting for $1"
    sleep 0.5
  done
}
oom() { for c in $(compose ps --quiet fleet); do [ "$(docker inspect "$c" --format '{{.State.OOMKilled}}')" = false ] || fail "replica OOM-killed"; done; }
all_connected() { # all_connected SECONDS WHAT
  local until_=$((SECONDS + $1))
  until [ "$(connected 2>/dev/null)" = "$agents" ]; do [ $SECONDS -lt $until_ ] || fail "$2: $(connected) of $agents connected"; sleep 1; done
}
lt60() { "$python" -c "import sys; sys.exit(0 if 0 < float(sys.argv[1]) < 60 else 1)" "$1"; }
# Rollouts (ROLLOUT=1). Four waves; a 10 s bake; any failure pauses.
policy='"waves":[{"percent":1},{"percent":10},{"percent":50},{"percent":100}],"bake_seconds":10,"wave_timeout_seconds":300,"max_failures":0,"max_failure_percent":1,"on_failure":"pause"'
rollout_get() { get "$base/api/v1/rollouts/$1" | json "$2" 2>/dev/null; }
wait_wave() { # wait_wave ID WAVE: until that wave is released
  local until_=$((SECONDS + 300))
  until w=$(rollout_get "$1" 'd["current_wave"]') && [ -n "$w" ] && [ "$w" -ge "$2" ]; do
    [ $SECONDS -lt $until_ ] || fail "rollout $1 did not reach wave $2"; sleep 1; done
}
wait_completed() { # wait_completed ID NAME: records the rollout's duration and outcomes
  local until_=$((SECONDS + storm_timeout - 60)) begun=$SECONDS state
  until state=$(rollout_get "$1" 'd["state"]') && [ "$state" = completed ]; do
    case "$state" in paused|rolled_back|cancelled) fail "rollout $2 ended $state: $(get "$base/api/v1/rollouts/$1" | head -c 600)";; esac
    [ $SECONDS -lt $until_ ] || fail "rollout $2 did not complete ($state)"; sleep 2; done
  psql_json "SELECT coalesce(json_object_agg(outcome, n), '{}') FROM (SELECT outcome, count(*) n FROM fleet.rollout_targets WHERE rollout_id = '$1' GROUP BY outcome) o" | tr -d '\r' > "$run/$2.outcomes.json"
  record "rollout_$2" "{\"outcomes\": $(cat "$run/$2.outcomes.json"), \"completed_after_seconds\": $(( SECONDS - begun ))}"
  note "rollout $2 completed: $(cat "$run/$2.outcomes.json")"
}
# sample_sends: per replica and second, the config sends counters.
sample_sends() {
  local c p
  while :; do
    for c in $(compose ps --quiet fleet 2>/dev/null); do
      p=$(port "$c" 8080); [ -n "$p" ] || continue
      curl -s --max-time 2 -H "@$run/auth_header" "http://127.0.0.1:$p/metrics" 2>/dev/null |
        sed -n "s/^linea_fleet_config_sends_total{path=\"\([a-z]*\)\"} \([0-9]*\)/$(date +%s.%N) ${c:0:12} \1 \2/p"
    done
    sleep 1
  done >> "$run/sends.txt"
}

mkdir -p "$root/.local"
sh "$root/scripts/fleet/new-deployment-secrets.sh" "$run/secrets" >/dev/null || fail "secrets"
mkdir -p "$run/checkpoints"
chmod 755 "$run/checkpoints"
record inputs "{\"agents\": $agents, \"replicas\": $replicas, \"heartbeat_seconds\": $heartbeat, \"steady_seconds\": $steady, \"connect_rate\": $connect_rate, \"payload_bytes\": $payload, \"storm_timeout_seconds\": $storm_timeout, \"max_agents\": $max_agents, \"max_connections_per_replica\": $max_connections}"
record limits "{\"fleet\": {\"cpus\": 2, \"memory\": \"$fleet_memory\", \"gomemlimit\": \"$fleet_gomemlimit\"}, \"postgres\": {\"cpus\": 1, \"memory\": \"512m\"}, \"standby\": {\"cpus\": 1, \"memory\": \"512m\"}, \"lb\": {\"cpus\": 1, \"memory\": \"512m\"}, \"simulator\": {\"cpus\": $sim_cpus, \"memory\": \"$sim_memory\"}}"
record source "\"$(git -C "$root" rev-parse HEAD 2>/dev/null)$(git -C "$root" diff --quiet 2>/dev/null || echo '-dirty')\""
record machine "\"$(docker info --format '{{.OSType}} {{.Architecture}} cpus={{.NCPU}} memory={{.MemTotal}} docker={{.ServerVersion}}')\""
printf "FLEET_SECRET_DIR=%s/secrets\nFLEET_SERVER_ID=%s\nFLEET_API_PORT=0\nFLEET_AGENT_PORT=0\nFLEET_VERSION=%s\nFLEET_REPLICAS=%s\n" "$run" "$project" "$tag" "$replicas" > "$run/compose.env"
# Each replica also publishes its management port on an ephemeral loopback
# port so the driver can read per-replica metrics; agents use only the LB.
cat > "$run/override.json" <<EOF
{"services": {"fleet": {"mem_limit": "$fleet_memory", "ports": ["127.0.0.1::8080"],
  "environment": {"GOMAXPROCS": "2", "GOMEMLIMIT": "$fleet_gomemlimit", "GODEBUG": "$fleet_godebug", "LINEA_FLEET_DATABASE_URL_FILE": "/run/secrets/runtime_url"},
  "command": ["--server-id", "$project", "--management-listen", "0.0.0.0:8080", "--agent-listen", "0.0.0.0:4320", "--allow-insecure-remote",
    "--max-agents", "$max_agents", "--max-connections", "$max_connections"$( [ "$rollout" = 1 ] && echo ', "--config-management"')]}},
 "networks": {"database": {"ipam": {"config": [{"subnet": "$subnet.1.0/24"}]}}, "ingress": {"ipam": {"config": [{"subnet": "$subnet.2.0/24"}]}}}}
EOF
if [ "${SKIP_BUILD:-}" != 1 ]; then
  note "building images"
  docker build --quiet --file "$root/deploy/docker/linea-fleet.Dockerfile" --target fleet --tag "ghcr.io/lineahq/linea-fleet:$tag" "$root" >/dev/null || fail "fleet image build"
  docker build --quiet --file "$root/deploy/docker/linea-fleet.Dockerfile" --target agent-simulator --tag "linea-fleet-simulator:$tag" "$root" >/dev/null || fail "simulator image build"
fi
record images "{\"fleet\": \"$(docker image inspect "ghcr.io/lineahq/linea-fleet:$tag" --format '{{.Id}}')\", \"simulator\": \"$(docker image inspect "linea-fleet-simulator:$tag" --format '{{.Id}}')\"}"

created=1
compose up --detach --wait postgres postgres-standby >/dev/null 2>&1 || fail "database start"
[ "$(compose exec -T postgres-standby psql -U postgres -d fleet -Atc 'SELECT pg_is_in_recovery()' | tr -d '\r')" = t ] || fail "standby is not streaming"
compose run --rm -T migrate >/dev/null 2>&1 || fail "migrate"
compose run --rm -T bootstrap 2>/dev/null | json 'd["token"]' | tr -d '\r' | sed 's/^/Authorization: Bearer /' > "$run/auth_header"
[ -s "$run/auth_header" ] || fail "bootstrap"
compose up --detach --wait fleet >/dev/null 2>&1 || fail "fleet start"
compose up --detach lb >/dev/null 2>&1 || fail "load balancer start"
lb=$(compose ps --quiet lb)
base="http://127.0.0.1:$(port "$lb" 8080)"
deadline=$((SECONDS + 30))
# Both backends must have a server: the agent port answers HTTP once one is up.
agent_lb="http://127.0.0.1:$(port "$lb" 4320)/v1/opamp"
until curl -sf --max-time 2 "$base/readyz" >/dev/null && [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$agent_lb")" != 000 ]; do
  [ $SECONDS -lt $deadline ] || fail "load balancer not ready"; sleep 0.5; done
[ "$(compose ps --quiet fleet | wc -l)" = "$replicas" ] || fail "expected $replicas replicas"
sleep 3 # health checks: every replica rises before the ramp
credential=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
expires=$("$python" -c "import datetime; print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(hours=3)).isoformat())")
req POST /api/v1/credentials "{\"id\": \"$credential\", \"name\": \"ha\", \"role\": \"agent\"$( [ "$rollout" = 1 ] && echo ', "remote_config": true'), \"expires_at\": \"$expires\"}" 201 | json 'd["token"]' | tr -d '\r\n' > "$run/agent_token"
chmod 444 "$run/agent_token"

simargs=(--endpoint ws://lb:4320/v1/opamp --allow-insecure-transport --token-file /run/agent_token --checkpoint-dir /checkpoints
  --agents "$agents" --heartbeat "${heartbeat}s" --duration "${steady}s" --connect-rate "$connect_rate"
  --payload-bytes "$payload" --reconnect-percent 1 --health-change-every 0 --hold 10s
  --storms 3 --storm-timeout "${storm_timeout}s")
[ "$rollout" = 1 ] && simargs+=(--accept-config --apply-delay 2s --builds 4 --minimal-percent 5)
started=1
docker run --detach --name "$sim" --network "${project}_ingress" --cpus "$sim_cpus" --memory "$sim_memory" --pids-limit 256 --read-only \
  --cap-drop ALL --security-opt no-new-privileges:true --log-driver local --log-opt max-size=4m --log-opt max-file=2 \
  --env GOMAXPROCS="$sim_cpus" --mount "type=bind,source=$run/agent_token,target=/run/agent_token,readonly" \
  --mount "type=bind,source=$run/checkpoints,target=/checkpoints,readonly" "linea-fleet-simulator:$tag" "${simargs[@]}" >/dev/null || fail "simulator start"
note "ramping $agents sessions at $connect_rate/s over $replicas replicas"

ready=$(phase ready $(( agents / connect_rate + 120 )))
record phase_ready "$ready"
note "ready after $(echo "$ready" | json 'round(d["elapsed_seconds"],1)') s"
[ "$(connected)" = "$agents" ] || fail "summary does not show $agents connected"
sleep 11
oom; replicas ready; pgstats ready
[ "$(json 'd["owners"]' < "$run/ready.pg.json")" = "$replicas" ] || fail "sessions are not spread over every replica"
release ready

# Steady heartbeats: presence holds, and no replica writes for them.
deadline=$((SECONDS + steady + 60))
while ! docker logs "$sim" 2>/dev/null | grep -q '"phase":"steady_complete"'; do
  [ "$(connected)" = "$agents" ] || fail "presence changed during steady reporting"
  [ $SECONDS -lt $deadline ] || fail "steady phase timed out"
  sleep 5
done
record phase_steady "$(phase steady_complete 10)"
oom; replicas steady; pgstats steady
"$python" - "$run" <<'EOF' > "$run/steady.json" || fail "steady accounting"
import json, sys
run = sys.argv[1]
rows = lambda p: {r["container"]: r for r in map(json.loads, open(f"{run}/{p}.replicas.jsonl"))}
a, b = rows("ready"), rows("steady")
pa, pb = (json.load(open(f"{run}/{p}.pg.json")) for p in ("ready", "steady"))
d = lambda k: sum(b[c][k] - a[c][k] for c in b if c in a)
seconds = pb["at"] - pa["at"]
out = {"seconds": round(seconds, 1), "server_reports": d("reports"), "server_report_errors": d("report_errors"),
       "writer_items": d("writer_items"), "transactions_per_second": round((pb["commits"] - pa["commits"]) / seconds, 2),
       "tuple_writes_per_second": round((pb["writes"] - pa["writes"]) / seconds, 2),
       "new_events": pb["events"] - pa["events"], "new_agents": pb["agents"] - pa["agents"]}
out["passed"] = len(a) == len(b) and out["server_report_errors"] == 0 and out["writer_items"] == 0 and out["new_events"] == 0 and out["new_agents"] == 0
print(json.dumps(out))
EOF
record steady "$(cat "$run/steady.json")"
note "steady: $(cat "$run/steady.json")"
[ "$(json 'd["passed"]' < "$run/steady.json")" = True ] || fail "steady database-write gate"
release steady_complete
record phase_reconnected "$(phase reconnected 120)"
all_connected 30 "planned reconnects"
release reconnected

# 1. Replica kill.
phase storm_armed 30 1 >/dev/null
if [ "$rollout" = 1 ]; then
  sample_sends & sampler=$!
  config=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n') assignment=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
  req POST /api/v1/configs "{\"id\": \"$config\", \"name\": \"ha\"}" 201 >/dev/null
  req POST "/api/v1/configs/$config/versions" '{"body": "receivers:\n  otlp: {}\n", "message": "v1"}' 201 >/dev/null
  req POST "/api/v1/configs/$config/versions" '{"body": "receivers:\n  otlp: {}\n  filelog: {}\n", "message": "v2 uses filelog"}' 201 >/dev/null
  creating=$(req POST /api/v1/config-assignments "{\"id\": \"$assignment\", \"name\": \"ha\", \"config_id\": \"$config\", \"version\": 1, \"layer\": 10, \"selector\": \"service.name=linea-fleet-simulator\", \"rollout\": {$policy}}" 201 | json 'd["rollout_id"]')
  note "creating rollout $creating started; killing a replica once wave 2 (50 %) is released"
  wait_wave "$creating" 2
fi
replicas before_kill
victim=$(head -n 1 "$run/before_kill.replicas.jsonl" | json 'd["container"]')
victim_sessions=$(field before_kill "$victim" connections); victim_sessions=${victim_sessions%.*}
note "killing replica ${victim:0:12} with $victim_sessions sessions"
docker kill "$victim" >/dev/null || fail "kill"
[ "$rollout" = 1 ] && wait_completed "$creating" v1
release storm_end
kill_obs=$(phase recovered $(( storm_timeout + 30 )) 1)
record phase_recovered_kill "$kill_obs"
lost=$(echo "$kill_obs" | json 'd.get("storm_lost", 0)') kill_seconds=$(echo "$kill_obs" | json 'd.get("storm_seconds",0)')
note "kill: $lost sessions lost and back in $(echo "$kill_obs" | json 'round(d.get("storm_seconds",0),1)') s"
[ "$lost" = "$victim_sessions" ] || fail "replica kill disturbed $lost sessions, expected only the victim's $victim_sessions"
lt60 "$kill_seconds" || fail "replica kill recovery took $kill_seconds s (gate < 60 s)"
all_connected 30 "after replica kill"
oom; replicas after_kill
[ "$(wc -l < "$run/after_kill.replicas.jsonl")" = $((replicas - 1)) ] || fail "unexpected replica count after kill"
# The simulator holds a checkpoint for at most 20 s; release it before
# waiting on the restart.
release recovered
docker start "$victim" >/dev/null || fail "restart killed replica"
deadline=$((SECONDS + 15))
until p=$(port "$victim" 8080) && [ -n "$p" ] && curl -sf --max-time 2 "http://127.0.0.1:$p/readyz" >/dev/null; do
  [ $SECONDS -lt $deadline ] || fail "killed replica did not come back"; sleep 0.5; done

# 2. Takeover fencing: a frozen replica is reaped, then must fence itself.
phase storm_armed 30 2 >/dev/null
replicas before_freeze
frozen=$(sed -n 2p "$run/before_freeze.replicas.jsonl" | json 'd["container"]')
frozen_id=$(field before_freeze "$frozen" replica_id)
frozen_sessions=$(field before_freeze "$frozen" connections); frozen_sessions=${frozen_sessions%.*}
frozen_restarts=$(field before_freeze "$frozen" restarts)
note "freezing replica ${frozen:0:12} with $frozen_sessions sessions"
docker pause "$frozen" >/dev/null || fail "pause"
paused=$SECONDS
deadline=$((SECONDS + 90))
until [ "$(psql_json "SELECT count(*) FROM fleet.agents WHERE owner_replica = '$frozen_id'" | tr -d '\r')" = 0 ] &&
      [ "$(psql_json "SELECT count(*) FROM fleet.replicas WHERE id = '$frozen_id'" | tr -d '\r')" = 0 ]; do
  [ $SECONDS -lt $deadline ] || fail "frozen replica was not reaped"; sleep 1; done
reap_seconds=$((SECONDS - paused))
note "frozen replica reaped after ${reap_seconds} s; thawing"
docker unpause "$frozen" >/dev/null || fail "unpause"
# The thawed replica must notice it was reaped, fence itself and exit.
deadline=$((SECONDS + 30))
until [ "$(docker inspect "$frozen" --format '{{.RestartCount}}')" -gt "$frozen_restarts" ]; do
  [ $SECONDS -lt $deadline ] || fail "frozen replica did not fence itself and restart"; sleep 0.5; done
fenced_seconds=$((SECONDS - paused - reap_seconds))
release storm_end-2
fence_obs=$(phase recovered $(( storm_timeout + 30 )) 2)
record phase_recovered_fence "$fence_obs"
lost=$(echo "$fence_obs" | json 'd.get("storm_lost", 0)') fence_seconds=$(echo "$fence_obs" | json 'd.get("storm_seconds",0)')
note "fence: $lost sessions lost and back in $(echo "$fence_obs" | json 'round(d.get("storm_seconds",0),1)') s"
[ "$lost" = "$frozen_sessions" ] || fail "fencing disturbed $lost sessions, expected the frozen replica's $frozen_sessions"
lt60 "$fence_seconds" || fail "fencing recovery took $fence_seconds s (gate < 60 s)"
all_connected 30 "after fencing"
reclaimed=$(psql_json "SELECT count(*) FROM fleet.agents WHERE owner_replica = '$frozen_id'" | tr -d '\r')
[ "$reclaimed" = 0 ] || fail "fenced replica reclaimed $reclaimed agents"
record fencing "{\"reap_seconds\": $reap_seconds, \"fenced_after_thaw_seconds\": $fenced_seconds, \"sessions\": $frozen_sessions, \"reclaimed\": $reclaimed, \"storm_seconds\": $fence_seconds}"
release recovered-2
deadline=$((SECONDS + 15))
until p=$(port "$frozen" 8080) && [ -n "$p" ] && curl -sf --max-time 2 "http://127.0.0.1:$p/readyz" >/dev/null; do
  [ $SECONDS -lt $deadline ] || fail "fenced replica did not come back"; sleep 0.5; done
oom; replicas after_fence

# 3. Database failover with a surviving event cursor.
phase storm_armed 30 3 >/dev/null
if [ "$rollout" = 1 ]; then
  moving=$(req POST /api/v1/rollouts "{\"assignment_id\": \"$assignment\", \"to_version\": 2, $policy}" 201 | json 'd["id"]')
  note "rollout $moving to version 2 started; failing over once wave 2 is released"
  wait_wave "$moving" 2
fi
cursor="" pages=0
while :; do
  page=$(req GET "/api/v1/events?limit=500${cursor:+&cursor=$cursor}")
  next=$(echo "$page" | json 'd["next_cursor"]'); count=$(echo "$page" | json 'len(d["items"])')
  cursor=$next pages=$((pages + 1))
  [ "$count" -gt 0 ] || break
  [ $pages -lt 400 ] || fail "event stream exceeded the page bound"
done
system_before=$(psql_json "SELECT system_identifier FROM pg_control_system()" | tr -d '\r')
restarts_before=$(for c in $(compose ps --quiet fleet); do docker inspect "$c" --format '{{.RestartCount}}'; done | awk '{s+=$1} END {print s+0}')
note "stopping the primary and promoting the standby"
started_failover=$SECONDS
compose stop postgres >/dev/null 2>&1 || fail "stop primary"
pgsvc=postgres-standby
[ "$(psql_json "SELECT pg_promote(true, 30)" | tr -d '\r')" = t ] || fail "promotion"
promoted=$((SECONDS - started_failover))
deadline=$((SECONDS + 60))
until curl -sf --max-time 2 "$base/readyz" >/dev/null && [ "$(connected 2>/dev/null)" = "$agents" ]; do
  [ $SECONDS -lt $deadline ] || fail "fleet did not recover after database failover"; sleep 0.5; done
failover_seconds=$((SECONDS - started_failover))
[ "$rollout" = 1 ] && wait_completed "$moving" v2
release storm_end-3
db_obs=$(phase recovered $(( storm_timeout + 30 )) 3)
record phase_recovered_failover "$db_obs"
lost=$(echo "$db_obs" | json 'd.get("storm_lost", 0)')
system_after=$(psql_json "SELECT system_identifier FROM pg_control_system()" | tr -d '\r')
restarts_after=$(for c in $(compose ps --quiet fleet); do docker inspect "$c" --format '{{.RestartCount}}'; done | awk '{s+=$1} END {print s+0}')
code=$(curl -s -o "$run/resp" -w '%{http_code}' -H "@$run/auth_header" --max-time 15 "$base/api/v1/events?limit=500&cursor=$cursor")
member=$(req GET "/api/v1/agents?limit=1" | json 'd["items"][0]["id"]')
write_code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H "@$run/auth_header" -H 'Content-Type: application/json' -H 'If-Match: "0"' \
  --max-time 15 --data "{\"name\": \"ha-failover\", \"agent_ids\": [\"$member\"]}" "$base/api/v1/groups/$id$id")
record failover "{\"promotion_seconds\": $promoted, \"fleet_ready_seconds\": $failover_seconds, \"sessions_lost\": $lost, \"replica_restarts\": $((restarts_after - restarts_before)), \"same_cluster\": $( [ "$system_before" = "$system_after" ] && echo true || echo false), \"cursor_status\": $code, \"events_after\": $( [ "$code" = 200 ] && json 'len(d["items"])' < "$run/resp" || echo null), \"write_status\": $write_code}"
note "failover: promoted in ${promoted} s, fleet ready with every session in ${failover_seconds} s, $lost sessions lost, cursor HTTP $code, write HTTP $write_code"
[ "$code" = 200 ] || fail "event cursor did not survive the database failover (HTTP $code)"
case "$write_code" in 200|201) ;; *) fail "write after failover returned $write_code";; esac
[ $((restarts_after - restarts_before)) = 0 ] || fail "a replica restarted during the database failover"
[ "$lost" = 0 ] || lt60 "$(echo "$db_obs" | json 'd.get("storm_seconds",0)')" || fail "sessions lost in the failover took too long to return"
oom; replicas after_failover
release recovered-3

stopped=$(phase stopped 60)
record phase_stopped "$stopped"
[ "$(docker wait "$sim" | tr -d '\r')" = 0 ] || fail "simulator exit"
[ "$(echo "$stopped" | json 'd["success"] and d["unexpected_errors"] == 0')" = True ] || fail "simulator reported errors"
if [ "$rollout" = 1 ]; then
  kill "$sampler" 2>/dev/null; sampler=
  held=$(psql_json "SELECT count(*) FROM fleet.agents WHERE config_hold_hash IS NOT NULL" | tr -d '\r')
  "$python" - "$run" "$agents" "$held" "$stopped" <<'EOF' > "$run/rollout.json" || fail "rollout accounting"
import collections, json, sys
run, agents, held, stopped = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), json.loads(sys.argv[4])
v1, v2 = (json.load(open(f"{run}/{n}.outcomes.json")) for n in ("v1", "v2"))
minimal = v2.get("incompatible", 0)
# Per replica, the highest asynchronous send rate between two samples at
# least a second apart; a restart resets the counter and is skipped.
last, peak = {}, collections.defaultdict(float)
for line in open(f"{run}/sends.txt"):
    t, c, path, n = line.split()
    if path != "async":
        continue
    t, n = float(t), int(n)
    if c in last and t - last[c][0] >= 1 and n >= last[c][1]:
        peak[c] = max(peak[c], (n - last[c][1]) / (t - last[c][0]))
    if c not in last or t - last[c][0] >= 1 or n < last[c][1]:
        last[c] = (t, n)
expected = agents + agents - minimal
out = {"v1": v1, "v2": v2, "held": held, "offered": stopped["configs_offered"], "applied": stopped["configs_applied"],
       "failed": stopped["configs_failed"], "expected_offers": expected, "peak_async_sends_per_second": {c: round(v, 1) for c, v in peak.items()}}
out["passed"] = (v1 == {"succeeded": agents} and v2.get("succeeded", 0) + minimal == agents and 0 < minimal < agents // 10
    and held == 0 and stopped["configs_failed"] == 0 and stopped["configs_offered"] == expected
    and bool(peak) and max(peak.values()) <= 220)
print(json.dumps(out))
EOF
  record rollout "$(cat "$run/rollout.json")"
  note "rollout: $(cat "$run/rollout.json")"
  [ "$(json 'd["passed"]' < "$run/rollout.json")" = True ] || fail "rollout gate"
fi
# Every session is closed now. Presence must follow: an agent still owned by
# a live replica would read as connected with no socket behind it.
deadline=$((SECONDS + 15))
until [ "$(connected)" = 0 ]; do
  [ $SECONDS -lt $deadline ] || fail "presence: $(connected) agents still connected after every session closed"; sleep 1; done
owned=$(psql_json "SELECT count(*) FROM fleet.agents WHERE owner_replica IS NOT NULL" | tr -d '\r')
[ "$owned" = 0 ] || fail "presence: $owned agents still owned after every session closed"
note "presence: 0 connected and 0 owned after the simulator stopped"
record status '"passed"'
echo "PASS: $agents sessions over $replicas replicas ($project)"
