#!/usr/bin/env bash
# The shared platform's fan-out gate: starts the Compose stack
# (deploy/compose/linea) as a disposable project with two instances and
# FANOUT_POOLS pool workers, registers nothing (the pools register the
# built-in runtime), sends FANOUT_RUNS runs that name no runtime
# through FANOUT_CLIENTS concurrent `linea test --server`, restarts one
# instance once half of them were submitted, and fails unless every run
# passes. The project, its volumes and its secrets are removed on exit.
#
#   scripts/debugger/fanout-compose.sh
#
# Settings: FANOUT_RUNS (200), FANOUT_CLIENTS (8), FANOUT_POOLS (4),
# FANOUT_PORT (7079), FANOUT_SAMPLE (logs-cleanup); FANOUT_KEEP=1 keeps the
# project and its files for inspection. The gate's networks are
# fixed at 10.207.70-73.0/24, so stop a running local stack first.
set -euo pipefail
export MSYS_NO_PATHCONV=1
# Native paths: Go and Docker on Windows (Git Bash) cannot read /f/... paths.
native() { cygpath -m "$1" 2>/dev/null || printf '%s' "$1"; }
root="$(native "$(cd "$(dirname "$0")/../.." && pwd)")"
runs="${FANOUT_RUNS:-200}"
clients="${FANOUT_CLIENTS:-8}"
pools="${FANOUT_POOLS:-4}"
port="${FANOUT_PORT:-7079}"
sample="${FANOUT_SAMPLE:-logs-cleanup}"
id="$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
project="linea-fanout-$id"
work="$(native "$(mktemp -d)")"
server="http://127.0.0.1:$port"

export LINEA_PORT="$port"
# The gate's shape, whatever the shell sets: two instances, so one can
# restart, and pools of capacity 4.
export LINEA_REPLICAS=2 LINEA_POOL_CAPACITY=4
# Fixed subnets beside the stack's edge network, so the gate also runs on a
# host whose Docker default address pools are used up.
cat > "$work/subnets.yaml" <<'YAML'
networks:
  database: {ipam: {config: [{subnet: 10.207.71.0/24}]}}
  storage: {ipam: {config: [{subnet: 10.207.72.0/24}]}}
  ingress: {ipam: {config: [{subnet: 10.207.73.0/24}]}}
YAML
compose() { docker compose -p "$project" --env-file "$work/.env" -f "$root/deploy/compose/linea/compose.yaml" -f "$work/subnets.yaml" "$@"; }

cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    compose logs --no-color --tail 40 linea pool >&2 2>&1 || true
  fi
  if [ "${FANOUT_KEEP:-}" = 1 ]; then
    echo "kept: docker compose -p $project, its .env and runs in $work" >&2
    exit "$status"
  fi
  compose down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT

# The stack's .env, with fresh random values; --env-file keeps a local
# deploy/compose/linea/.env out of the gate.
for name in POSTGRES_PASSWORD S3_ACCESS_KEY S3_SECRET_KEY WORKER_TOKEN BUILDER_TOKEN; do
  echo "LINEA_$name=$(od -An -N32 -tx1 /dev/urandom | tr -d ' 
')"
done >"$work/.env"
linea="$work/bin/linea$(go env GOEXE)"
go build -C "$root" -o "$linea" ./cmd/linea

echo "Starting $project: 2 instances, $pools pools"
# The gate runs no builds: leave out the builder (its image compiles the
# worker source into a Go toolchain image), and with it the egress network.
# The pools run only their own build, so sharing the cache volume costs
# nothing: they download no binary.
compose up -d --build --scale "pool=$pools" proxy pool
# Nobody registers a runtime: wait until a pool registered the built-in one
# and it is the only runtime, the default.
deadline=$((SECONDS + 180))
until "$linea" runtime list --server "$server" >"$work/runtimes.out" 2>&1 &&
  [ "$(awk 'NR > 1 { print $1, $2 }' "$work/runtimes.out")" = "built-in (default)" ]; do
  [ "$SECONDS" -lt "$deadline" ] || { cat "$work/runtimes.out" >&2; exit 1; }
  sleep 2
done
# The proxy signs requests in as the local admin, so it refuses other host
# names: a page rebound to 127.0.0.1 must not reach the API.
rebound="$(curl -s -o "$work/rebound.out" -w '%{http_code}' -H 'Host: rebound.example' "$server/api/v1/runs")"
[ "$rebound" = 421 ] || { echo "a rebound host name got $rebound, want 421" >&2; exit 1; }

# One run: its exit code and output land in $work/runs/<n>.
mkdir -p "$work/runs"
cat > "$work/run.sh" <<EOF
#!/usr/bin/env bash
touch "$work/runs/\$1.started"
cd "$root"
"$linea" test --server "$server" --config "samples/$sample/config.yaml" --scenario "samples/$sample/scenario.yaml" \
  > "$work/runs/\$1.out" 2>&1
echo \$? > "$work/runs/\$1.code"
EOF
chmod +x "$work/run.sh"

start=$SECONDS
seq "$runs" | xargs -P "$clients" -n 1 "$work/run.sh" &
clients_pid=$!

# Restart one instance gracefully once half the runs were submitted; it
# drains and hands its runs back to the queue.
until [ "$(find "$work/runs" -name '*.started' | wc -l)" -ge $((runs / 2)) ]; do
  kill -0 "$clients_pid" 2>/dev/null || break
  sleep 0.2
done
instance="$(compose ps -q linea | head -n 1)"
echo "Restarting instance ${instance:0:12} after $(find "$work/runs" -name '*.code' | wc -l) of $runs runs ended"
docker restart -t 45 "$instance" >/dev/null

wait "$clients_pid" || true
elapsed=$((SECONDS - start))

failed=0
for n in $(seq "$runs"); do
  code="$(cat "$work/runs/$n.code" 2>/dev/null || echo missing)"
  if [ "$code" != 0 ]; then
    failed=$((failed + 1))
    if [ "$failed" -le 5 ]; then
      echo "run $n: exit $code" >&2
      tail -n 5 "$work/runs/$n.out" >&2 2>/dev/null || true
    fi
  fi
done
if [ "$failed" -ne 0 ]; then
  echo "FAIL: $failed of $runs runs did not pass ($elapsed s)" >&2
  exit 1
fi
echo "PASS: $runs runs passed across an instance restart in $elapsed s"
