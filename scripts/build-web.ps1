[CmdletBinding()]
param(
    # Also build the linea-server binary with the web client embedded.
    [switch]$Binary
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$web = Join-Path $repoRoot 'web'

Push-Location $web
try {
    # The lockfile pins every package; never resolve new versions here.
    pnpm install --frozen-lockfile
    if ($LASTEXITCODE -ne 0) { throw 'pnpm install failed' }
    # Regenerate the TypeScript API types from the committed OpenAPI contract.
    pnpm gen:api
    if ($LASTEXITCODE -ne 0) { throw 'API type generation failed' }
    pnpm lint
    if ($LASTEXITCODE -ne 0) { throw 'web lint failed' }
    pnpm test
    if ($LASTEXITCODE -ne 0) { throw 'web tests failed' }
    # Type-checks, then writes internal/debugger/webui/static/app (ignored by git).
    pnpm build
    if ($LASTEXITCODE -ne 0) { throw 'web build failed' }
}
finally {
    Pop-Location
}

if ($Binary) {
    Push-Location $repoRoot
    try {
        go build -o (Join-Path $repoRoot 'dist/linea-server.exe') ./cmd/linea-server
        if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
    }
    finally {
        Pop-Location
    }
}
