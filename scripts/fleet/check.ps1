[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
$fleetPreviousGOMAXPROCS = $env:GOMAXPROCS
Push-Location (Split-Path (Split-Path $PSScriptRoot -Parent) -Parent)
try {
    $env:GOMAXPROCS = '2'
    $fleetUnformatted = & gofmt -l cmd/linea-fleet cmd/linea-fleet-simulator internal/fleet pluginsdk
    if ($LASTEXITCODE -ne 0 -or $fleetUnformatted) { throw "Run gofmt on: $fleetUnformatted" }
    $fleetPackages = @('./internal/fleet/fleet', './internal/fleet/inventory', './internal/fleet/auth', './pluginsdk', './pluginsdk/metrics', './internal/fleet/plugins', './internal/fleet/api', './internal/fleet/gateway', './internal/fleet/app', './internal/fleet/postgres')
    $fleetPackages += './internal/fleet/agentsimulator'
    & go test -p 1 @fleetPackages
    if ($LASTEXITCODE -ne 0) { throw 'Package tests failed' }
    & go vet -p 1 ./cmd/linea-fleet @fleetPackages
    if ($LASTEXITCODE -ne 0) { throw 'Go vet failed' }
    & go build -p 1 -o bin/linea-fleet.exe ./cmd/linea-fleet
    if ($LASTEXITCODE -ne 0) { throw 'Build failed' }
    & go vet -p 1 ./cmd/linea-fleet-simulator
    if ($LASTEXITCODE -ne 0) { throw 'Simulator vet failed' }
    & go build -p 1 -o bin/agent-simulator.exe ./cmd/linea-fleet-simulator
    if ($LASTEXITCODE -ne 0) { throw 'Simulator build failed' }
    & git diff --check
    if ($LASTEXITCODE -ne 0) { throw 'Whitespace check failed' }
} finally {
    Pop-Location
    $env:GOMAXPROCS = $fleetPreviousGOMAXPROCS
}
