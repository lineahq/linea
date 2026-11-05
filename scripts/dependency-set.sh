#!/bin/bash
# Generates a supported release's dependency set (dependencies.mod and
# dependencies.sum: every core and contrib component of the release resolved
# with the debugger) and its component catalog (catalog.json) in the pinned Go
# image, with the module cache volume runtime builds use. The shell twin of
# dependency-set.ps1. The set resolves the release's modules at their versions
# with go get before go mod tidy, so tidy never looks up a newer Collector. The
# first run downloads every module of the release (about 5 GB extracted).
#
#   scripts/dependency-set.sh --version V [--catalog-only] [--memory 3g] [--cpus 2]
#
#   --version V      the release directory under build/versions, such as
#                    0.162.0. It must hold the release's manifest.yaml
#                    (upstream's otelcol-contrib manifest, unchanged) and may
#                    hold unsupported.yaml. A bare first argument works too.
#   --catalog-only   write only catalog.json, from the existing dependency set
#   --memory SIZE    container memory limit (and swap limit); default 3g
#   --cpus N         container CPU limit; default 2
#   -h, --help       print this text
set -euo pipefail
export MSYS_NO_PATHCONV=1
winpath() { (cd "$1" && { pwd -W 2>/dev/null || pwd; }); }
root=$(winpath "$(dirname "$0")/..")

usage() { awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; }
die() { echo "$*" >&2; exit 1; }

version=
catalog_only=0
memory=3g
cpus=2
while [ $# -gt 0 ]; do
    case $1 in
        --version) [ $# -ge 2 ] || { usage >&2; exit 2; }; version=$2; shift;;
        --catalog-only) catalog_only=1;;
        --memory) [ $# -ge 2 ] || { usage >&2; exit 2; }; memory=$2; shift;;
        --cpus) [ $# -ge 2 ] || { usage >&2; exit 2; }; cpus=$2; shift;;
        -h|--help) usage; exit 0;;
        -*) echo "unknown argument: $1" >&2; usage >&2; exit 2;;
        *) [ -z "$version" ] || { echo "unexpected argument: $1" >&2; exit 2; }; version=$1;;
    esac
    shift
done
[ -n "$version" ] || { echo '--version is required' >&2; usage >&2; exit 2; }
case $version in
    *[!0-9A-Za-z._-]*) echo "invalid version: $version" >&2; exit 2;;
esac

[ -f "$root/build/versions/$version/manifest.yaml" ] \
    || die "build/versions/$version/manifest.yaml is missing: add the release's otelcol-contrib manifest first"
go_image=$(sed -n 's/.*const GoImage = "\([^"]*\)".*/\1/p' "$root/internal/debugger/container/build.go" | head -n 1)
[ -n "$go_image" ] || die 'could not read GoImage from internal/debugger/container/build.go'

# Files the container writes into the mounted checkout are owned by root on a
# Linux host; the script hands them back to the invoking user afterwards.
host_owner=
if [ "$(uname -s)" = Linux ]; then host_owner=$(id -u):$(id -g); fi

# The script goes in on standard input and is written to a file first, because
# the commands it runs (go run) would otherwise read the rest of it.
docker run --rm -i --name "linea-depset-$version" \
    --memory "$memory" --memory-swap "$memory" --cpus "$cpus" \
    -v linea-gomod:/go/pkg/mod -v linea-gocache:/root/.cache/go-build \
    -v "$root:/repo" \
    -e "VERSION=$version" -e "CATALOG_ONLY=$catalog_only" -e "HOST_OWNER=$host_owner" -e GOTOOLCHAIN=local \
    "$go_image" sh -c 'cat > /tmp/gen.sh && sh /tmp/gen.sh' <<'SCRIPT' \
    || die "generating build/versions/$version failed"
set -eu
dir=build/versions/$VERSION
own() {
  [ -z "$HOST_OWNER" ] || chown "$HOST_OWNER" "$@" 2>/dev/null || true
}
if [ "$CATALOG_ONLY" != 1 ]; then
  rm -rf /work && mkdir /work
  tar -C /repo --exclude=./.git --exclude=./web --exclude=./dist --exclude=./.tools --exclude=./.linea -cf - . | tar -C /work -xf -
  cd /work
  go run ./build/versions/gen manifest -dir $dir > /tmp/set.yaml
  go run ./internal/debugger/registry/gen -manifest /tmp/set.yaml -source "the $VERSION dependency set" -out internal/debugger/registry/components.go \
    -ottl internal/debugger/compat/ottl/variant.go -features internal/debugger/featureset/featureset.go
  # Only the release's OTTL variant resolves against it; a nested
  # variant module becomes a package of the work module, as in a build.
  variant=$(sed -n 's|.*/internal/debugger/compat/ottl/\(since[0-9]*\)"|\1|p' internal/debugger/compat/ottl/variant.go)
  for d in internal/debugger/compat/ottl/since*; do
    if [ "${d##*/}" = "$variant" ]; then rm -f "$d/go.mod" "$d/go.sum"; else rm -rf "$d"; fi
  done
  sed -n 's/^  *- \(.*\) => \(.*\)$/\1=\2/p' /tmp/set.yaml | while read -r r; do
    old=${r%%=*}; new=${r#*=}
    go mod edit -replace "$(echo $old | tr -s ' ' '@')=$(echo $new | tr -s ' ' '@')"
  done
  mods=$(sed -n 's/^ *- gomod: \([^ ]*\) \(v[^ ]*\).*/\1@\2/p' /tmp/set.yaml)
  # The root module requires the OpenTelemetry libraries at its own release;
  # dropping them lets the release's components choose their versions, so
  # an older release never mixes in the root's newer libraries.
  for r in $(sed -n 's/^\t\(go\.opentelemetry\.io\/[^ ]*\|github\.com\/open-telemetry\/[^ ]*\) v.*/\1/p' go.mod); do
    go mod edit -droprequire "$r"
  done
  # tools.go's OCB, at the release rather than looked up at its latest.
  go mod edit -require "go.opentelemetry.io/collector/cmd/builder@v$VERSION"
  go get $mods
  go mod tidy
  cp go.mod /repo/$dir/dependencies.mod
  cp go.sum /repo/$dir/dependencies.sum
  own /repo/$dir/dependencies.mod /repo/$dir/dependencies.sum
fi
cd /repo
go run ./build/versions/gen catalog -dir $dir
own /repo/$dir/catalog.json
SCRIPT
