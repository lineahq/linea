#!/usr/bin/env bash
# Checks that the standalone CLI needs no network: it builds the linea CLI for
# Linux and runs inspect, test and run --compare on every sample in a
# container started with --network none. A command fails the check when its
# exit code differs from the sample's documented outcome (samples/index.yaml
# expect: pass is exit 0, fail is exit 3; inspect and run --compare exit 0) or
# its output mentions a dial, a lookup or a refused connection. The
# container runs with bounded resources and is removed on exit.
# LINEA_BINARY checks that Linux CLI instead, such as one linea build made.
#
#   scripts/debugger/airgap-check.sh
#   LINEA_BINARY=./linea-custom scripts/debugger/airgap-check.sh
set -euo pipefail
export MSYS_NO_PATHCONV=1
root="$(cd "$(dirname "$0")/../.." && pwd)"
image='alpine:3.20'
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Docker and go on Windows take Windows paths.
host() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf '%s' "$1"; fi; }

if [ -n "${LINEA_BINARY:-}" ]; then
  echo "checking $LINEA_BINARY"
  cp "$LINEA_BINARY" "$work/linea"
else
  arch="$(docker version --format '{{.Server.Arch}}')"
  echo "building linea for linux/$arch"
  (cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" GOMAXPROCS="${GOMAXPROCS:-2}" \
    go build -p "${GO_BUILD_PARALLELISM:-2}" -trimpath -ldflags='-s -w' -o "$(host "$work/linea")" ./cmd/linea)
fi

# id expect, one sample per line.
awk '/^- id:/ {id=$3} /^  expect:/ {print id, $2}' "$root/samples/index.yaml" >"$work/expect"
[ -s "$work/expect" ] || { echo "no samples in samples/index.yaml" >&2; exit 1; }

cat >"$work/check.sh" <<'EOF'
#!/bin/sh
# Runs inside the container: /check/linea is the CLI, /samples the repository's
# samples (read-only), /check this script and the expected outcomes.
set -u
cp /check/linea /tmp/linea && chmod +x /tmp/linea || exit 1
cp -r /samples /tmp/samples
cd /tmp/samples
failed=0
while read -r id expect; do
  for cmd in inspect test run; do
    want=0
    case $cmd in
      inspect) set -- inspect --config "$id/config.yaml" --scenario "$id/scenario.yaml" ;;
      test)    set -- test --config "$id/config.yaml" --scenario "$id/scenario.yaml"
               [ "$expect" = fail ] && want=3 ;;
      run)     set -- run --config "$id/config.yaml" --scenario "$id/scenario.yaml" --compare ;;
    esac
    /tmp/linea "$@" >"/tmp/out" 2>&1
    rc=$?
    status=ok
    if [ "$rc" -ne "$want" ]; then
      status="exit $rc, want $want"
    elif grep -qiE "dial |no such host|network is unreachable|connection refused|lookup " /tmp/out; then
      status="network access"
    fi
    echo "$id $cmd: $status"
    if [ "$status" != ok ]; then
      failed=$((failed + 1))
      head -20 /tmp/out | sed 's/^/    /'
    fi
  done
done </check/expect
if [ "$failed" -gt 0 ]; then
  echo "$failed command(s) failed without network"
  exit 1
fi
echo "every sample ran without network"
EOF

docker run --rm --network none --cpus 2 --memory 2g \
  --volume "$(host "$root/samples"):/samples:ro" \
  --volume "$(host "$work"):/check:ro" \
  "$image" sh /check/check.sh
