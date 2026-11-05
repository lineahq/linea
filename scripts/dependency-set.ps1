[CmdletBinding()]
param(
    # The release directory under build/versions, such as 0.162.0. It must
    # hold the release's manifest.yaml (upstream's otelcol-contrib manifest,
    # unchanged) and may hold unsupported.yaml.
    [Parameter(Mandatory = $true)]
    [string]$Version,
    # Writes only catalog.json, from the existing dependency set.
    [switch]$CatalogOnly,
    [string]$Memory = '3g',
    [string]$Cpus = '2'
)

# Generates a supported release's dependency set (dependencies.mod and
# dependencies.sum: every core and contrib component of the release resolved
# with the debugger) and its component catalog (catalog.json) in
# the pinned Go image, with the module cache volume runtime builds use. The
# set resolves the release's modules at their versions with go get before
# go mod tidy, so tidy never looks up a newer Collector. The first run
# downloads every module of the release (about 5 GB extracted).

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$release = Join-Path $repoRoot "build/versions/$Version"
if (-not (Test-Path (Join-Path $release 'manifest.yaml'))) {
    throw "build/versions/$Version/manifest.yaml is missing: add the release's otelcol-contrib manifest first"
}
$goImage = (Select-String -Path (Join-Path $repoRoot 'internal/debugger/container/build.go') -Pattern 'const GoImage = "([^"]+)"').Matches[0].Groups[1].Value

$script = @'
set -eu
dir=build/versions/$VERSION
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
fi
cd /repo
go run ./build/versions/gen catalog -dir $dir
'@

# The script goes in on standard input: Windows PowerShell mangles quotes in
# native arguments, and its input may start with a byte order mark.
$script | docker run --rm -i --name "linea-depset-$Version" `
    --memory $Memory --memory-swap $Memory --cpus $Cpus `
    -v linea-gomod:/go/pkg/mod -v linea-gocache:/root/.cache/go-build `
    -v "${repoRoot}:/repo" `
    -e "VERSION=$Version" -e "CATALOG_ONLY=$([int][bool]$CatalogOnly)" -e GOTOOLCHAIN=local `
    $goImage sh -c "tr -d '\r' | sed '1s/^\xEF\xBB\xBF//' > /tmp/gen.sh && sh /tmp/gen.sh"
if ($LASTEXITCODE -ne 0) {
    throw "generating build/versions/$Version failed"
}
