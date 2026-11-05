#!/usr/bin/env bash
# Saturation profile for one fleet replica (M1/M2 gates): synthetic OpAMP
# sessions from one bounded simulator container against the Compose stack.
# It measures ramp time, replica memory per session, database writes during
# steady heartbeats, an optional crash storm (SIGKILL + restart) with cursor
# survival, and an optional UID-churn soak against capacity/retention bounds.
# Official opamp-go clients set up like the Collector's OpAMP extension, one
# per session; no Collector pipelines are involved.
#
# Run one step at a time and never two profiles at once. Every container has
# hard CPU/memory caps; the run stops at the first breached threshold.
#
# AGENTS (1-20000, default 1000)        HEARTBEAT (s, 30)    STEADY (s, 120)
# CONNECT_RATE (/s, 200)                PAYLOAD_BYTES (1024) RECONNECT_PERCENT (1)
# STORM=1 kill and restart the replica, STORM_TIMEOUT (s, 120)
# CHURN_RATE (/s, 0), MAX_AGENTS (default max(2*AGENTS, 1000)), RETENTION (168h)
# FLEET_MEMORY (1g), FLEET_GOMEMLIMIT (896MiB), SIM_MEMORY (1g), SIM_CPUS (2)
# FLEET_ARGS: extra replica flags (space separated)
# SUBNET (10.248, uses .1 and .2 /24), TAG (saturation), SKIP_BUILD=1, KEEP=1
set -u
export MSYS_NO_PATHCONV=1
winpath() { (cd "$1" && { pwd -W 2>/dev/null || pwd; }); }
root="$(winpath "$(dirname "$0")/../..")"
python=$(command -v python || command -v python3)
agents=${AGENTS:-1000} heartbeat=${HEARTBEAT:-30} steady=${STEADY:-120}
connect_rate=${CONNECT_RATE:-200} payload=${PAYLOAD_BYTES:-1024} reconnect=${RECONNECT_PERCENT:-1}
storm=${STORM:-0} storm_timeout=${STORM_TIMEOUT:-120} churn=${CHURN_RATE:-0}
max_agents=${MAX_AGENTS:-$(( agents * 2 > 1000 ? agents * 2 : 1000 ))} retention=${RETENTION:-168h}
fleet_memory=${FLEET_MEMORY:-1g} fleet_gomemlimit=${FLEET_GOMEMLIMIT:-896MiB} sim_memory=${SIM_MEMORY:-1g} sim_cpus=${SIM_CPUS:-2}
subnet=${SUBNET:-10.248} tag=${TAG:-saturation}
max_connections=$(( agents + 16 > 10000 ? 10000 : agents + 16 ))
id="$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
project="fleet-sat-$id"
run="$root/.local/$project"
sim="$project-sim"
created=0 started=0
main=$$
trap 'exit 1' USR1
fail() { echo "FAIL: $*" >&2; record failure "\"$*\""; kill -USR1 "$main"; exit 1; }
note() { echo "$(date +%H:%M:%S) $*"; }
compose() { docker compose --project-name "$project" --env-file "$run/compose.env" --file "$root/deploy/compose/fleet/compose.yaml" --file "$run/override.json" "$@"; }
cleanup() {
  [ "$started" = 1 ] && { docker logs "$sim" 2>/dev/null | grep '^{' > "$run/observations.jsonl"; docker rm --force "$sim" >/dev/null 2>&1; }
  [ "${KEEP:-}" = 1 ] && { echo "kept $project"; return; }
  [ "$created" = 1 ] && { compose logs --no-color fleet 2>&1 | tail -n 200 > "$run/fleet.log"; compose down --volumes --remove-orphans --timeout 5 >/dev/null 2>&1; }
  echo "results: $run/result.json"
}
trap cleanup EXIT
# record KEY JSON-VALUE: merge one value into result.json.
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
# req METHOD PATH [BODY] [EXPECTED] -> body on stdout; the token never appears in output.
req() {
  local method=$1 path=$2 body=${3:-} expected=${4:-200} code
  local args=(-s -o "$run/resp" -w '%{http_code}' -X "$method" -H "@$run/auth_header" --max-time 15)
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' --data "$body")
  code=$(curl "${args[@]}" "$base$path")
  [ "$code" = "$expected" ] || fail "$method $path returned $code, expected $expected"
  cat "$run/resp"
}
connected() { req GET /api/v1/summary | json 'd["connected"]'; }
metric() { # metric FILE NAME -> value (0 when absent)
  "$python" -c "
import re,sys
m=re.search(r'(?m)^'+re.escape(sys.argv[2])+r' ([0-9.eE+-]+)\s*$', open(sys.argv[1]).read())
print(float(m.group(1)) if m else 0)" "$1" "$2"
}
snapshot() { # snapshot PHASE: metrics, container resources and PostgreSQL counters
  req GET /metrics > "$run/$1.prom"
  docker stats --no-stream --format '{{json .}}' "$fleet" "$postgres" "$sim" > "$run/$1.resources.jsonl"
  compose exec -T postgres psql -U postgres -d fleet -Atc "
    SELECT json_build_object('commits', (SELECT xact_commit FROM pg_stat_database WHERE datname = 'fleet'),
      'agent_updates', (SELECT n_tup_upd FROM pg_stat_user_tables WHERE relid = 'fleet.agents'::regclass),
      'agent_inserts', (SELECT n_tup_ins FROM pg_stat_user_tables WHERE relid = 'fleet.agents'::regclass),
      'agent_deletes', (SELECT n_tup_del FROM pg_stat_user_tables WHERE relid = 'fleet.agents'::regclass),
      'writes', (SELECT sum(n_tup_ins + n_tup_upd + n_tup_del) FROM pg_stat_user_tables WHERE schemaname = 'fleet'),
      'wal_bytes', pg_current_wal_lsn() - '0/0'::pg_lsn,
      'agents', (SELECT count(*) FROM fleet.agents), 'events', (SELECT count(*) FROM fleet.events),
      'database_bytes', pg_database_size('fleet'), 'agents_bytes', pg_total_relation_size('fleet.agents'),
      'events_bytes', pg_total_relation_size('fleet.events'), 'at', extract(epoch FROM clock_timestamp()))" \
    > "$run/$1.pg.json" || fail "PostgreSQL counters for $1"
  local memory heap live stack goroutines sessions
  memory=$(metric "$run/$1.prom" linea_fleet_go_memory_bytes)
  heap=$(metric "$run/$1.prom" linea_fleet_go_heap_objects_bytes)
  live=$(metric "$run/$1.prom" linea_fleet_go_heap_live_bytes)
  stack=$(metric "$run/$1.prom" linea_fleet_go_stack_bytes)
  goroutines=$(metric "$run/$1.prom" linea_fleet_goroutines)
  sessions=$(metric "$run/$1.prom" linea_fleet_agent_connections)
  local rss
  rss=$(docker stats --no-stream --format '{{.MemUsage}}' "$fleet" | cut -d/ -f1 | tr -d ' ')
  record "snapshot_$1" "{\"go_memory_bytes\": $memory, \"go_heap_objects_bytes\": $heap, \"go_heap_live_bytes\": $live, \"go_stack_bytes\": $stack, \"goroutines\": $goroutines, \"connections\": $sessions, \"container_memory\": \"$rss\", \"postgres\": $(cat "$run/$1.pg.json")}"
  note "$1: connections=$sessions go_memory=$(( ${memory%.*} >> 20 ))MiB heap=$(( ${heap%.*} >> 20 ))MiB goroutines=${goroutines%.*} container=$rss"
  # Memory gates: the heap live at the last GC plus
  # goroutine stacks stay within 64 KiB per session, and all Go memory within
  # GOMEMLIMIT. Total Go memory per session also counts garbage awaiting the
  # next GC, up to GOGC times the live heap, so it doubles after a storm.
  local held=$(( ${live%.*} + ${stack%.*} )) limit
  limit=$("$python" -c 'import re,sys; n,u=re.fullmatch(r"(\d+)(B|KiB|MiB|GiB)",sys.argv[1]).groups(); print(int(n)<<{"B":0,"KiB":10,"MiB":20,"GiB":30}[u])' "$fleet_gomemlimit")
  [ "${memory%.*}" -le "$limit" ] || fail "Go memory above GOMEMLIMIT at $1: ${memory%.*} bytes"
  if [ "${sessions%.*}" -ge 1000 ] && [ $(( held / ${sessions%.*} )) -gt 65536 ]; then
    fail "memory budget exceeded at $1: $(( held / ${sessions%.*} )) live and stack bytes per session"
  fi
}
release() { : > "$run/checkpoints/$1"; }
# phase NAME TIMEOUT -> the simulator's observation line for NAME.
phase() {
  local deadline=$((SECONDS + $2)) line
  while :; do
    line=$(docker logs "$sim" 2>/dev/null | "$python" -c "
import json,sys
want=sys.argv[1]
for raw in sys.stdin:
    if not raw.startswith('{'): continue
    o=json.loads(raw)
    if o.get('phase')=='stopped' and not o.get('success'):
        print('FAILED '+str(o.get('failure'))); break
    if o.get('phase')==want:
        print(raw.strip()); break" "$1")
    case "$line" in
      FAILED*) fail "simulator: ${line#FAILED }";;
      ?*) echo "$line"; return;;
    esac
    [ "$(docker inspect "$sim" --format '{{.State.Running}}' 2>/dev/null)" = true ] || {
      sleep 1; line=$(docker logs "$sim" 2>/dev/null | grep "\"phase\":\"$1\"" | head -n 1)
      [ -n "$line" ] && { echo "$line"; return; }; fail "simulator stopped before $1"; }
    [ $SECONDS -lt $deadline ] || fail "timed out waiting for $1"
    sleep 1
  done
}
oom() { [ "$(docker inspect "$fleet" --format '{{.State.OOMKilled}}')" = false ] || fail "fleet replica was OOM-killed"; }

mkdir -p "$root/.local"
sh "$root/scripts/fleet/new-deployment-secrets.sh" "$run/secrets" >/dev/null || fail "secrets"
mkdir -p "$run/checkpoints"
chmod 755 "$run/checkpoints"
record inputs "{\"agents\": $agents, \"heartbeat_seconds\": $heartbeat, \"steady_seconds\": $steady, \"connect_rate\": $connect_rate, \"payload_bytes\": $payload, \"reconnect_percent\": $reconnect, \"storm\": $storm, \"storm_timeout_seconds\": $storm_timeout, \"churn_rate\": $churn, \"max_agents\": $max_agents, \"retention\": \"$retention\", \"max_connections\": $max_connections, \"fleet_args\": \"${FLEET_ARGS:-}\"}"
record limits "{\"fleet\": {\"cpus\": 2, \"memory\": \"$fleet_memory\", \"gomemlimit\": \"$fleet_gomemlimit\"}, \"postgres\": {\"cpus\": 1, \"memory\": \"512m\"}, \"simulator\": {\"cpus\": $sim_cpus, \"memory\": \"$sim_memory\"}}"
record source "\"$(git -C "$root" rev-parse HEAD 2>/dev/null)$(git -C "$root" diff --quiet 2>/dev/null || echo '-dirty')\""
record machine "\"$(docker info --format '{{.OSType}} {{.Architecture}} cpus={{.NCPU}} memory={{.MemTotal}} docker={{.ServerVersion}}')\""
extra=""
for flag in ${FLEET_ARGS:-}; do extra="$extra, \"$flag\""; done
printf "FLEET_SECRET_DIR=%s/secrets\nFLEET_SERVER_ID=%s\nFLEET_API_PORT=0\nFLEET_AGENT_PORT=0\nFLEET_VERSION=%s\n" "$run" "$project" "$tag" > "$run/compose.env"
cat > "$run/override.json" <<EOF
{"services": {"fleet": {"mem_limit": "$fleet_memory",
  "environment": {"GOMAXPROCS": "2", "GOMEMLIMIT": "$fleet_gomemlimit", "LINEA_FLEET_DATABASE_URL_FILE": "/run/secrets/runtime_url"},
  "command": ["--server-id", "$project", "--management-listen", "0.0.0.0:8080", "--agent-listen", "0.0.0.0:4320", "--allow-insecure-remote",
    "--max-agents", "$max_agents", "--max-connections", "$max_connections", "--agent-retention", "$retention"$extra]}},
 "networks": {"database": {"ipam": {"config": [{"subnet": "$subnet.1.0/24"}]}}, "ingress": {"ipam": {"config": [{"subnet": "$subnet.2.0/24"}]}}}}
EOF
if [ "${SKIP_BUILD:-}" != 1 ]; then
  note "building images"
  docker build --quiet --file "$root/deploy/docker/linea-fleet.Dockerfile" --target fleet --tag "ghcr.io/lineahq/linea-fleet:$tag" "$root" >/dev/null || fail "fleet image build"
  docker build --quiet --file "$root/deploy/docker/linea-fleet.Dockerfile" --target agent-simulator --tag "linea-fleet-simulator:$tag" "$root" >/dev/null || fail "simulator image build"
fi
record images "{\"fleet\": \"$(docker image inspect "ghcr.io/lineahq/linea-fleet:$tag" --format '{{.Id}}')\", \"simulator\": \"$(docker image inspect "linea-fleet-simulator:$tag" --format '{{.Id}}')\"}"

created=1
compose up --detach --wait postgres >/dev/null 2>&1 || fail "postgres start"
compose run --rm -T migrate >/dev/null 2>&1 || fail "migrate"
compose run --rm -T bootstrap 2>/dev/null | json 'd["token"]' | tr -d '\r' | sed 's/^/Authorization: Bearer /' > "$run/auth_header"
[ -s "$run/auth_header" ] || fail "bootstrap"
compose up --detach --wait fleet >/dev/null 2>&1 || fail "fleet start"
fleet=$(compose ps --quiet fleet) postgres=$(compose ps --quiet postgres)
base="http://127.0.0.1:$(docker port "$fleet" 8080/tcp | head -n 1 | sed 's/.*://' | tr -d '\r')"
credential=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
expires=$("$python" -c "import datetime; print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(hours=3)).isoformat())")
req POST /api/v1/credentials "{\"id\": \"$credential\", \"name\": \"saturation\", \"role\": \"agent\", \"expires_at\": \"$expires\"}" 201 | json 'd["token"]' | tr -d '\r\n' > "$run/agent_token"
chmod 444 "$run/agent_token"

simargs=(--endpoint ws://fleet:4320/v1/opamp --allow-insecure-transport --token-file /run/agent_token --checkpoint-dir /checkpoints
  --agents "$agents" --heartbeat "${heartbeat}s" --duration "${steady}s" --connect-rate "$connect_rate"
  --payload-bytes "$payload" --reconnect-percent "$reconnect" --health-change-every 0 --hold 10s --churn-rate "$churn")
[ "$storm" = 1 ] && simargs+=(--storm --storm-timeout "${storm_timeout}s")
started=1
docker run --detach --name "$sim" --network "${project}_ingress" --cpus "$sim_cpus" --memory "$sim_memory" --pids-limit 256 --read-only \
  --cap-drop ALL --security-opt no-new-privileges:true --log-driver local --log-opt max-size=4m --log-opt max-file=2 \
  --env GOMAXPROCS="$sim_cpus" --mount "type=bind,source=$run/agent_token,target=/run/agent_token,readonly" \
  --mount "type=bind,source=$run/checkpoints,target=/checkpoints,readonly" "linea-fleet-simulator:$tag" "${simargs[@]}" >/dev/null || fail "simulator start"
note "ramping $agents sessions at $connect_rate/s"

ready=$(phase ready $(( agents / connect_rate + 120 )))
record phase_ready "$ready"
note "ready after $(echo "$ready" | json 'round(d["elapsed_seconds"],1)') s"
[ "$(connected)" = "$agents" ] || fail "summary does not show $agents connected"
# Table statistics reach the shared counters up to 10 s late; let the ramp's
# writes settle (the checkpoint holds for at most 20 s).
sleep 11
oom; snapshot ready
release ready

# soak_sample: one line of bounded-growth evidence per call during churn.
soak_sample() {
  req GET /metrics > "$run/soak.prom"
  local summary pg
  summary=$(req GET /api/v1/summary)
  pg=$(compose exec -T postgres psql -U postgres -d fleet -Atc "SELECT json_build_object('agents', (SELECT count(*) FROM fleet.agents),
    'events', (SELECT count(*) FROM fleet.events), 'agents_bytes', pg_total_relation_size('fleet.agents'),
    'events_bytes', pg_total_relation_size('fleet.events'), 'database_bytes', pg_database_size('fleet'))") || fail "soak sample"
  echo "{\"t\": $SECONDS, \"total\": $(echo "$summary" | json 'd["total"]'), \"connected\": $(echo "$summary" | json 'd["connected"]'), \"go_memory_bytes\": $(metric "$run/soak.prom" linea_fleet_go_memory_bytes), \"expired\": $(metric "$run/soak.prom" linea_fleet_agents_expired_total), \"postgres\": $pg}" >> "$run/soak.jsonl"
}

# Steady heartbeats. Presence must hold throughout.
deadline=$((SECONDS + steady + 60)) next_sample=0
while ! docker logs "$sim" 2>/dev/null | grep -q '"phase":"steady_complete"'; do
  [ "$churn" != 0 ] || [ "$(connected)" = "$agents" ] || fail "presence changed during steady reporting"
  if [ "$churn" != 0 ] && [ $SECONDS -ge $next_sample ]; then soak_sample; next_sample=$((SECONDS + 15)); fi
  [ $SECONDS -lt $deadline ] || fail "steady phase timed out"
  sleep 5
done
if [ "$churn" != 0 ]; then
  soak_sample
  # Bounded growth: inventory never exceeds --max-agents, expiry runs, and the
  # second half of the soak grows neither inventory nor replica memory by more
  # than a quarter over the first half's peak.
  "$python" - "$run/soak.jsonl" "$max_agents" <<'PY' > "$run/soak.json" || fail "soak accounting"
import json, sys
rows = [json.loads(l) for l in open(sys.argv[1])]
limit = int(sys.argv[2])
half = len(rows) // 2
first, second = rows[1:half] or rows[:1], rows[half:]
peak = lambda rs, f: max(f(r) for r in rs)
out = {"samples": len(rows), "max_total": peak(rows, lambda r: r["total"]), "max_agents_limit": limit,
       "expired": rows[-1]["expired"],
       "first_half_peak_total": peak(first, lambda r: r["total"]), "second_half_peak_total": peak(second, lambda r: r["total"]),
       "first_half_peak_memory": peak(first, lambda r: r["go_memory_bytes"]), "second_half_peak_memory": peak(second, lambda r: r["go_memory_bytes"]),
       "first_half_peak_database_bytes": peak(first, lambda r: r["postgres"]["database_bytes"]),
       "second_half_peak_database_bytes": peak(second, lambda r: r["postgres"]["database_bytes"]),
       "final": rows[-1]}
out["passed"] = (out["max_total"] <= limit and out["expired"] > 0
                 and out["second_half_peak_total"] <= 1.25 * out["first_half_peak_total"]
                 and out["second_half_peak_memory"] <= 1.25 * out["first_half_peak_memory"])
print(json.dumps(out))
PY
  record soak "$(cat "$run/soak.json")"
  note "soak: $(cat "$run/soak.json" | cut -c1-400)"
  [ "$(json 'd["passed"]' < "$run/soak.json")" = True ] || fail "churn soak bounded-growth gate"
fi
complete=$(phase steady_complete 10)
record phase_steady "$complete"
oom; snapshot steady
"$python" - "$run" "$agents" "$heartbeat" "$churn" <<'EOF' > "$run/steady.json" || fail "steady accounting"
import json, re, sys
run, agents, heartbeat, churn = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4])
a, b = (json.load(open(f"{run}/{p}.pg.json")) for p in ("ready", "steady"))
def m(p, name):
    r = re.search(r"(?m)^" + re.escape(name) + r" ([0-9.eE+-]+)\s*$", open(f"{run}/{p}.prom").read())
    return float(r.group(1)) if r else 0.0
seconds = b["at"] - a["at"]
reports = m("steady", "linea_fleet_report_duration_seconds_count") - m("ready", "linea_fleet_report_duration_seconds_count")
out = {"seconds": round(seconds, 1), "server_reports": reports,
       "server_report_errors": m("steady", "linea_fleet_report_errors_total") - m("ready", "linea_fleet_report_errors_total"),
       # Transactions include read-only statements (authentication, API reads).
       "transactions_per_second": round((b["commits"] - a["commits"]) / seconds, 2),
       "tuple_writes_per_second": round((b["writes"] - a["writes"]) / seconds, 2),
       "wal_bytes_per_second": round((b["wal_bytes"] - a["wal_bytes"]) / seconds),
       "agent_row_updates": b["agent_updates"] - a["agent_updates"],
       "agent_row_inserts": b["agent_inserts"] - a["agent_inserts"],
       "agent_row_deletes": b["agent_deletes"] - a["agent_deletes"],
       # Changed reports go through the batched writer; unchanged heartbeats must not.
       "writer_items": m("steady", "linea_fleet_writer_batch_items_total") - m("ready", "linea_fleet_writer_batch_items_total"),
       "writer_batches": m("steady", "linea_fleet_writer_batches_total") - m("ready", "linea_fleet_writer_batches_total"),
       "new_events": b["events"] - a["events"], "new_agents": b["agents"] - a["agents"],
       "agents": b["agents"], "events": b["events"],
       "database_bytes": b["database_bytes"], "agents_bytes": b["agents_bytes"], "events_bytes": b["events_bytes"]}
# Heartbeats cause no write of their own: only the 10 s last_seen flush
# touches rows, so updates stay at most one per agent and flush.
out["max_flush_updates"] = int(agents * (seconds / 10 + 1))
ok = out["server_report_errors"] == 0 and out["agent_row_updates"] <= out["max_flush_updates"]
if churn == 0:
    ok = ok and out["writer_items"] == 0 and out["new_events"] == 0 and out["new_agents"] == 0
out["passed"] = ok
print(json.dumps(out))
EOF
record steady "$(cat "$run/steady.json")"
note "steady: $(cat "$run/steady.json")"
[ "$(json 'd["passed"]' < "$run/steady.json")" = True ] || fail "steady database-write gate"
release steady_complete

reconnected=$(phase reconnected 120)
record phase_reconnected "$reconnected"
[ "$churn" != 0 ] || [ "$(connected)" = "$agents" ] || fail "planned reconnects lost a session"
oom; snapshot reconnected

if [ "$storm" = 1 ]; then
  # Read the whole event stream to its end; the cursor must survive the crash.
  cursor="" pages=0
  while :; do
    page=$(req GET "/api/v1/events?limit=500${cursor:+&cursor=$cursor}")
    next=$(echo "$page" | json 'd["next_cursor"]'); count=$(echo "$page" | json 'len(d["items"])')
    cursor=$next pages=$((pages + 1))
    [ "$count" -gt 0 ] || break
    [ $pages -lt 400 ] || fail "event stream exceeded the page bound"
  done
  release reconnected
  phase storm_armed 30 >/dev/null
  note "killing the replica"
  compose kill fleet >/dev/null 2>&1 || fail "kill"
  killed=$SECONDS
  compose up --detach fleet >/dev/null 2>&1 || fail "restart"
  fleet=$(compose ps --quiet fleet)
  base="http://127.0.0.1:$(docker port "$fleet" 8080/tcp | head -n 1 | sed 's/.*://' | tr -d '\r')"
  # The storm ends once every lost session is back after this point.
  release storm_end
  recovered=$(phase recovered $(( storm_timeout + 30 )))
  record phase_recovered "$recovered"
  note "storm: all sessions back $(echo "$recovered" | json 'round(d["storm_seconds"],1)') s after the first loss (driver: $((SECONDS - killed)) s after kill)"
  wait_until=$((SECONDS + 30))
  until [ "$(connected)" = "$agents" ]; do [ $SECONDS -lt $wait_until ] || fail "summary after storm: $(connected) connected"; sleep 1; done
  code=$(curl -s -o "$run/resp" -w '%{http_code}' -H "@$run/auth_header" --max-time 15 "$base/api/v1/events?limit=500&cursor=$cursor")
  record cursor_after_restart "{\"status\": $code, \"pages_before\": $pages, \"events_after\": $( [ "$code" = 200 ] && json 'len(d["items"])' < "$run/resp" || echo null)}"
  [ "$code" = 200 ] || fail "event cursor did not survive the restart (HTTP $code)"
  oom; snapshot recovered
  storm_seconds=$(echo "$recovered" | json 'd["storm_seconds"]')
  "$python" -c "import sys; sys.exit(0 if float(sys.argv[1]) < 60 else 1)" "$storm_seconds" || fail "storm took $storm_seconds s (gate < 60 s)"
  release recovered
else
  release reconnected
fi

stopped=$(phase stopped 60)
record phase_stopped "$stopped"
[ "$(docker wait "$sim" | tr -d '\r')" = 0 ] || fail "simulator exit"
[ "$(echo "$stopped" | json 'd["success"] and d["unexpected_errors"] == 0')" = True ] || fail "simulator reported errors"
record status '"passed"'
echo "PASS: $agents sessions ($project)"
