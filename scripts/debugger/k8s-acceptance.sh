#!/usr/bin/env bash
# The Kubernetes acceptance gate for server runtime builds:
# installs the Helm chart (deploy/charts/linea) on a disposable k3d cluster
# with the worker pool, a builder and NetworkPolicies, then checks that
#
#   1. the builder compiles the default runtime (`linea runtime build
#      --server`) and registers it; workers need no image;
#   2. `linea run` and `linea inspect` on that runtime pass on the pool,
#      which downloads the binary through a presigned SeaweedFS URL;
#   3. a pod with the pool's labels reaches the server and the object store
#      but not the module proxy, an external address or PostgreSQL;
#   4. a pod with the builder's labels reaches the module proxy;
#   5. the builder exported the runtime's image to an in-cluster registry
#      (builder.export), and the build records it by digest.
#
# Every check prints PASS or FAIL; the script exits non-zero on any failure.
#
#   scripts/debugger/k8s-acceptance.sh [--keep]
#
# Needs docker, k3d (k3s enforces NetworkPolicy), kubectl, helm, go, curl and
# openssl, about 8 GiB of memory for Docker (the builder is bounded to 5 GiB
# and 4 CPUs, the pool to 4 GiB) and a network that reaches the module proxy.
# A cold build takes 5 to 10 minutes. Do not run it on a shared or constrained
# machine.
#
# Settings: K8S_GATE_PORT (7078, the local port-forward to the server);
# K8S_GATE_BUILD_TIMEOUT (1800, seconds the runtime build may take);
# K8S_GATE_SUBNET (a CIDR for the cluster's Docker network, when Docker's
# default address pools are used up);
# K8S_GATE_KEEP=1 or --keep keeps the cluster, the images and the files for
# inspection (print where, and how to remove them).
#
# The gate touches nothing of yours: the cluster is named linea-gate-<id>, its
# kubeconfig lives in a temporary directory that KUBECONFIG points at for the
# whole script (your kubeconfig and current context are never read or
# written), the images are tagged gate-<id>, and an exit trap removes all of
# it. Authentication uses trusted headers: the server config trusts requests from
# the port-forward (127.0.0.1 inside the pod) and the pod network, and binds
# the group gate-admins as admin. That is acceptable only in this throwaway
# cluster.
set -euo pipefail
export MSYS_NO_PATHCONV=1
# Native paths: Go, Docker, kubectl and helm on Windows (Git Bash) cannot read
# /f/... paths.
native() { cygpath -m "$1" 2>/dev/null || printf '%s' "$1"; }
root="$(native "$(cd "$(dirname "$0")/../.." && pwd)")"
keep="${K8S_GATE_KEEP:-}"
for arg in "$@"; do
  case "$arg" in
    --keep) keep=1 ;;
    *) echo "usage: $0 [--keep]" >&2; exit 2 ;;
  esac
done
port="${K8S_GATE_PORT:-7078}"
build_timeout="${K8S_GATE_BUILD_TIMEOUT:-1800}"
subnet=()
[ -n "${K8S_GATE_SUBNET:-}" ] && subnet=(--subnet "$K8S_GATE_SUBNET")
id="$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
cluster="linea-gate-$id"
tag="gate-$id"
ns=linea
release=linea
server="http://127.0.0.1:$port"
probe_image="curlimages/curl:8.11.1"

for tool in docker k3d kubectl helm go curl openssl; do
  command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 2; }
done
docker info >/dev/null 2>&1 || { echo "docker does not answer: start Docker first" >&2; exit 2; }

umask 077
work="$(native "$(mktemp -d)")"
# The gate's own kubeconfig: kubectl, helm and k3d never see yours.
export KUBECONFIG="$work/kubeconfig"
linea="$work/bin/linea$(go env GOEXE)"
forwards=()

kc() { kubectl -n "$ns" "$@"; }

cleanup() {
  status=$?
  trap - EXIT
  for pid in "${forwards[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" >/dev/null 2>&1 || true
  done
  if [ "$status" -ne 0 ] && [ -s "$KUBECONFIG" ]; then
    echo "--- cluster state" >&2
    kc get pods -o wide >&2 2>&1 || true
    for d in linea linea-pool linea-builder; do
      echo "--- logs of deploy/$d" >&2
      kc logs "deploy/$d" --tail=40 >&2 2>&1 || true
    done
  fi
  if [ "$keep" = 1 ]; then
    echo "kept: cluster $cluster, images *:$tag, files in $work" >&2
    echo "  KUBECONFIG=$KUBECONFIG kubectl -n $ns get pods" >&2
    echo "  remove: k3d cluster delete $cluster; docker rmi linea-server:$tag linea-worker:$tag linea-builder:$tag; rm -rf $work" >&2
    exit "$status"
  fi
  k3d cluster delete "$cluster" >/dev/null 2>&1 || true
  docker rmi "linea-server:$tag" "linea-worker:$tag" "linea-builder:$tag" >/dev/null 2>&1 || true
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT

failures=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1" >&2; failures=$((failures + 1)); }

# portforward SVC LOCAL REMOTE starts kubectl port-forward in the background.
portforward() {
  kc port-forward "svc/$1" "$2:$3" >"$work/pf-$1.log" 2>&1 &
  forwards+=("$!")
}

echo "== building the images (tag $tag)"
go -C "$root" generate ./internal/debugger/workersrc
docker build -q -f "$root/deploy/docker/linea-server.Dockerfile" -t "linea-server:$tag" "$root" >/dev/null
docker build -q -f "$root/deploy/docker/linea-worker.Dockerfile" -t "linea-worker:$tag" "$root" >/dev/null
docker build -q -f "$root/deploy/docker/linea-builder.Dockerfile" -t "linea-builder:$tag" "$root" >/dev/null
go build -C "$root" -o "$linea" ./cmd/linea

echo "== creating the cluster $cluster"
# k3s enforces NetworkPolicy by default. Traefik is not needed, and the
# default kubeconfig is left alone.
k3d cluster create "$cluster" --no-lb --wait "${subnet[@]}" \
  --k3s-arg "--disable=traefik@server:*" \
  --kubeconfig-update-default=false --kubeconfig-switch-context=false
k3d kubeconfig write "$cluster" --output "$KUBECONFIG" >/dev/null
k3d image import -c "$cluster" "linea-server:$tag" "linea-worker:$tag" "linea-builder:$tag"
kubectl create namespace "$ns"

echo "== PostgreSQL and SeaweedFS"
db_password="$(openssl rand -hex 16 | tr -d '\r\n')"
access_key="$(openssl rand -hex 8 | tr -d '\r\n')"
secret_key="$(openssl rand -hex 16 | tr -d '\r\n')"
cat > "$work/store.yaml" <<YAML
apiVersion: v1
kind: Secret
metadata: {name: seaweedfs-config, namespace: $ns}
stringData:
  s3.json: |
    {"identities":[{"name":"linea","credentials":[{"accessKey":"$access_key","secretKey":"$secret_key"}],
      "actions":["Admin","Read","Write","List","Tagging"]}]}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: postgres, namespace: $ns}
spec:
  selector: {matchLabels: {app: postgres}}
  template:
    metadata: {labels: {app: postgres}}
    spec:
      containers:
        - name: postgres
          image: postgres:17
          env:
            - {name: POSTGRES_PASSWORD, value: "$db_password"}
            - {name: POSTGRES_DB, value: linea}
          ports: [{containerPort: 5432}]
---
apiVersion: v1
kind: Service
metadata: {name: postgres, namespace: $ns}
spec:
  selector: {app: postgres}
  ports: [{port: 5432}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: seaweedfs, namespace: $ns}
spec:
  selector: {matchLabels: {app: seaweedfs}}
  template:
    metadata: {labels: {app: seaweedfs}}
    spec:
      # Service links would set SEAWEEDFS_* variables in the pod.
      enableServiceLinks: false
      containers:
        - name: seaweedfs
          image: chrislusf/seaweedfs:4.51
          command: [weed]
          args: [server, -dir=/data, -master.volumeSizeLimitMB=64, -volume.max=30,
                 -filer, -s3, -s3.port=8333, -s3.config=/etc/seaweedfs/s3.json]
          ports: [{containerPort: 8333}]
          volumeMounts:
            - {name: config, mountPath: /etc/seaweedfs, readOnly: true}
            - {name: data, mountPath: /data}
      volumes:
        - {name: config, secret: {secretName: seaweedfs-config}}
        - {name: data, emptyDir: {}}
---
apiVersion: v1
kind: Service
metadata: {name: seaweedfs, namespace: $ns}
spec:
  selector: {app: seaweedfs}
  ports: [{port: 8333}]
---
# The image export's target: an open registry without TLS.
apiVersion: apps/v1
kind: Deployment
metadata: {name: registry, namespace: $ns}
spec:
  selector: {matchLabels: {app: registry}}
  template:
    metadata: {labels: {app: registry}}
    spec:
      containers:
        - name: registry
          image: registry:2.8.3
          ports: [{containerPort: 5000}]
---
apiVersion: v1
kind: Service
metadata: {name: registry, namespace: $ns}
spec:
  selector: {app: registry}
  ports: [{port: 5000}]
YAML
kubectl apply -f "$work/store.yaml" >/dev/null
for d in postgres seaweedfs registry; do
  kc rollout status "deploy/$d" --timeout=300s
done
# Create the bucket with a signed request through a port-forward; this also
# waits for the gateway.
portforward seaweedfs 8334 8333
deadline=$((SECONDS + 120))
until [ "$(curl -4 -s -o /dev/null -w '%{http_code}' --aws-sigv4 'aws:amz:us-east-1:s3' \
  --user "$access_key:$secret_key" -X PUT http://127.0.0.1:8334/linea || true)" = 200 ]; do
  [ "$SECONDS" -lt "$deadline" ] || { echo "the bucket was not created" >&2; exit 1; }
  sleep 2
done
kill "${forwards[0]}" >/dev/null 2>&1 || true

echo "== installing the chart"
# trusted_headers needs no identity provider. Requests through the
# port-forward reach the pod from 127.0.0.1; the pod network is trusted too.
cat > "$work/values.yaml" <<YAML
image: {repository: linea-server, tag: $tag}
server: {replicas: 1}
store:
  databaseURL: postgres://postgres:$db_password@postgres:5432/linea?sslmode=disable
  objects:
    endpoint: http://seaweedfs:8333
    bucket: linea
    accessKey: $access_key
    secretKey: $secret_key
config:
  authentication:
    trusted_headers:
      proxies: [127.0.0.1/32, 10.42.0.0/16]
      issuer: gate
      user: X-Forwarded-User
      groups: X-Forwarded-Groups
  bindings:
    - {group: gate-admins, role: admin, scope: "*"}
pool:
  image: {repository: linea-worker, tag: $tag}
builder:
  image: {repository: linea-builder, tag: $tag}
  export:
    repository: registry:5000/linea/runtimes
    plainHTTP: ["registry:5000"]
networkPolicy:
  objectStorage:
    - to:
        - podSelector: {matchLabels: {app: seaweedfs}}
      ports: [{port: 8333}]
  # The default (every address) and the in-cluster registry, which an
  # ipBlock need not match.
  builderEgress:
    - to: [{ipBlock: {cidr: 0.0.0.0/0}}]
    - to: [{podSelector: {matchLabels: {app: registry}}}]
      ports: [{port: 5000}]
YAML
helm install "$release" "$root/deploy/charts/linea" -n "$ns" -f "$work/values.yaml" >/dev/null
for d in linea linea-pool linea-builder; do
  kc rollout status "deploy/$d" --timeout=600s
done

echo "== signing in"
portforward linea "$port" 7070
# The proxy headers name a member of gate-admins; the API token acts as that
# user, with the groups it had when created.
token=""
deadline=$((SECONDS + 120))
while [ -z "$token" ]; do
  token="$(curl -4 -s -X POST "$server/api/v1/tokens" -H 'Content-Type: application/json' \
    -H 'X-Forwarded-User: gate' -H 'X-Forwarded-Groups: gate-admins' \
    -d '{"label":"k8s-acceptance","expires_in_days":1}' \
    | sed -n 's/.*"secret": *"\([^"]*\)".*/\1/p' || true)"
  [ -n "$token" ] && break
  [ "$SECONDS" -lt "$deadline" ] || { echo "no API token could be created at $server" >&2; exit 1; }
  sleep 2
done
export LINEA_TOKEN="$token"
unset token

echo "== building the default runtime on the builder (a cold build takes minutes)"
cd "$root"
if timeout "$build_timeout" "$linea" runtime build --server "$server" --name default --manifest build/linea-runtime.yaml; then
  pass "runtime build on the builder registered the runtime default"
else
  fail "runtime build --server did not finish (exit $?)"
fi

if "$linea" runtime list --server "$server" 2>&1 | grep -qw default; then
  pass "the catalog lists the runtime default"
else
  fail "the catalog does not list the runtime default"
fi

# The build's record names the exported image by digest in the repository.
image="$(curl -4 -s -H "Authorization: Bearer $LINEA_TOKEN" "$server/api/v1/runtime-builds?limit=1" \
  | sed -n 's/.*"image": *"\([^"]*\)".*/\1/p' | head -n 1 || true)"
case "$image" in
  registry:5000/linea/runtimes@sha256:*) pass "the builder exported the image $image" ;;
  *) fail "the build records no exported image (got '$image')" ;;
esac

echo "== running on the pool"
if "$linea" run --server "$server" --runtime default \
  --config samples/logs-cleanup/config.yaml --scenario samples/logs-cleanup/scenario.yaml; then
  pass "linea run passed on the pool (runtime default)"
else
  fail "linea run on the runtime default did not pass (exit $?)"
fi
if "$linea" inspect --server "$server" --runtime default \
  --config samples/logs-cleanup/config.yaml --scenario samples/logs-cleanup/scenario.yaml >/dev/null; then
  pass "linea inspect ran on the pool (runtime default)"
else
  fail "linea inspect on the runtime default failed (exit $?)"
fi

echo "== egress"
if kc get networkpolicy "$release-pool" "$release-builder" >/dev/null 2>&1; then
  pass "the pool and the builder have NetworkPolicies"
else
  fail "the pool or the builder has no NetworkPolicy"
fi

# Throwaway pods carry exactly the selector labels of the pool and the
# builder, so their NetworkPolicies apply. They need a shell and a writable
# root, so nothing of the chart's hardening is copied; only the labels matter.
labels() { echo "app.kubernetes.io/name=linea,app.kubernetes.io/instance=$release,app.kubernetes.io/component=$1"; }
kc run probe-pool --image="$probe_image" --restart=Never --labels="$(labels pool)" --command -- sleep 3600 >/dev/null
kc run probe-builder --image="$probe_image" --restart=Never --labels="$(labels builder)" --command -- sleep 3600 >/dev/null
kc wait --for=condition=Ready pod/probe-pool pod/probe-builder --timeout=180s >/dev/null

# probe POD URL runs curl in the pod and prints its exit code: 0 is a reply
# (any HTTP status), 7 a refusal and 28 a timeout. DNS is allowed for every
# pod, so a blocked address times out.
probe() {
  local code=0
  kc exec "$1" -- curl -s -o /dev/null -m 6 --connect-timeout 5 "$2" >/dev/null 2>&1 || code=$?
  echo "$code"
}
# reaches POD URL LABEL passes when the pod gets a reply within a minute (a
# new policy can take a few seconds to be enforced).
reaches() {
  local deadline=$((SECONDS + 60)) code
  while :; do
    code="$(probe "$1" "$2")"
    if [ "$code" = 0 ]; then pass "$3"; return; fi
    [ "$SECONDS" -lt "$deadline" ] || { fail "$3 (curl exit $code)"; return; }
    sleep 3
  done
}
# blocked POD URL LABEL passes when the connection never gets a reply: it
# must time out or be refused, and still do so on a second try.
blocked() {
  local deadline=$((SECONDS + 90)) code
  while :; do
    code="$(probe "$1" "$2")"
    case "$code" in
      7|28)
        code="$(probe "$1" "$2")"
        case "$code" in
          7|28) pass "$3"; return ;;
        esac
        ;;
    esac
    [ "$SECONDS" -lt "$deadline" ] || { fail "$3 (curl exit $code, want a timeout)"; return; }
    sleep 3
  done
}

reaches probe-pool "http://$release:7070/" "the pool reaches the server"
reaches probe-pool "http://seaweedfs:8333/" "the pool reaches the object store"
blocked probe-pool "https://proxy.golang.org/" "the pool cannot reach proxy.golang.org"
blocked probe-pool "https://1.1.1.1/" "the pool cannot reach 1.1.1.1"
blocked probe-pool "http://postgres:5432/" "the pool cannot reach PostgreSQL"
reaches probe-builder "http://$release:7070/" "the builder reaches the server"
reaches probe-builder "http://seaweedfs:8333/" "the builder reaches the object store"
reaches probe-builder "https://proxy.golang.org/" "the builder reaches proxy.golang.org"

if [ "$failures" -ne 0 ]; then
  echo "FAIL: $failures check(s) failed" >&2
  exit 1
fi
echo "PASS: runtime build, pool run and egress checks passed on $cluster"
