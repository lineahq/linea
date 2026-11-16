#!/usr/bin/env bash
# The shared platform's fan-out gate: starts the Compose stack
# (deploy/compose/linea) as a disposable project, with the override
# fanout-compose.yaml that runs two server instances behind a load balancer
# and points the workers at it, and FANOUT_POOLS pool workers. It registers
# nothing (the pools register the built-in runtime), signs in as the built-in
# admin account for an API token, sends FANOUT_RUNS runs that name no runtime
# through FANOUT_CLIENTS concurrent `linea test --server`, restarts one
# instance once half of them were submitted, and fails unless every run
# passes. The project, its volumes and its secrets are removed on exit.
#
#   scripts/debugger/fanout-compose.sh
#
# Settings: FANOUT_RUNS (200), FANOUT_CLIENTS (8), FANOUT_POOLS (4),
# FANOUT_PORT (7079), FANOUT_SAMPLE (logs-cleanup); FANOUT_KEEP=1 keeps the
# project and its files for inspection. The gate publishes
# 127.0.0.1:FANOUT_PORT, so stop a local stack on that port first.
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

# The gate's settings come from this script and its .env, not from the
# shell: --env-file loses to the environment. LINEA_VERSION stays, to name
# the images.
for v in $(compgen -e); do
  case "$v" in
    LINEA_VERSION) ;;
    LINEA_*) unset "$v" ;;
  esac
done
# The gate's shape: pools of capacity 4, the balancer on $port.
export LINEA_PORT="$port" LINEA_POOL_CAPACITY=4
compose() {
  docker compose -p "$project" --env-file "$work/.env" \
    -f "$root/deploy/compose/linea/compose.yaml" -f "$root/scripts/debugger/fanout-compose.yaml" "$@"
}

cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    compose logs --no-color --tail 40 linea lb pool >&2 2>&1 || true
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

# The stack's secrets: fresh random files in the gate's own directory (the
# identity provider's stays empty), named by LINEA_SECRETS_DIR in an .env
# of its own, so a local deploy/compose/linea/.env and secrets/ stay out of
# the gate.
random() { od -An -N32 -tx1 /dev/urandom | tr -d ' \n'; }
mkdir "$work/secrets"
for name in postgres_password s3_access_key s3_secret_key worker_token builder_token admin_password; do
  random >"$work/secrets/$name"
done
: >"$work/secrets/oidc_client_secret"
echo "LINEA_SECRETS_DIR=$work/secrets" >"$work/.env"
linea="$work/bin/linea$(go env GOEXE)"
go build -C "$root" -o "$linea" ./cmd/linea

echo "Starting $project: 2 instances behind a load balancer, $pools pools"
# The gate runs no builds: leave out the builder (its image compiles the
# worker source into a Go toolchain image), and with it the egress network.
# The pools run only their own build, so sharing the cache volume costs
# nothing: they download no binary. --wait returns once PostgreSQL,
# SeaweedFS, both instances and the balancer are healthy and the pools run.
compose up -d --build --wait --wait-timeout 600 --scale "pool=$pools" lb pool

# The CLI signs in as the built-in admin account for an API token. The
# password goes to curl on stdin, never into a command line or the output.
admin_password="$(cat "$work/secrets/admin_password")"
reply="$(printf '{"user":"admin","password":"%s","client":"cli"}' "$admin_password" |
  curl -fsS -H 'Content-Type: application/json' --data-binary @- "$server/auth/password")" ||
  { echo "the admin sign-in failed" >&2; exit 1; }
LINEA_TOKEN="$(printf '%s' "$reply" | sed -n 's/.*"token" *: *"\([^"]*\)".*/\1/p')"
unset admin_password reply
[ -n "$LINEA_TOKEN" ] || { echo "the admin sign-in returned no token" >&2; exit 1; }
export LINEA_TOKEN

# Nobody registers a runtime: wait until a pool registered the built-in one
# and it is the only runtime, the default.
deadline=$((SECONDS + 180))
until "$linea" runtime list --server "$server" >"$work/runtimes.out" 2>&1 &&
  [ "$(awk 'NR > 1 { print $1, $2 }' "$work/runtimes.out")" = "built-in (default)" ]; do
  [ "$SECONDS" -lt "$deadline" ] || { cat "$work/runtimes.out" >&2; exit 1; }
  sleep 2
done

# One run: its exit code and output land in $work/runs/<n>.
mkdir -p "$work/runs"
cat > "$work/run.sh" <<RUN
#!/usr/bin/env bash
touch "$work/runs/\$1.started"
cd "$root"
"$linea" test --server "$server" --config "samples/$sample/config.yaml" --scenario "samples/$sample/scenario.yaml" \
  > "$work/runs/\$1.out" 2>&1
echo \$? > "$work/runs/\$1.code"
RUN
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
