# -InventoryRecords N runs only the opt-in inventory latency measurement at N records.
[CmdletBinding()]
param([switch]$Race,[ValidateRange(0,100000)][int]$InventoryRecords=0)
$ErrorActionPreference='Stop'
$fleetTestRoot=Split-Path (Split-Path $PSScriptRoot -Parent) -Parent
$fleetTestName='linea-fleet-pg-test-'+[Guid]::NewGuid().ToString('N')
$fleetOldPassword=$env:POSTGRES_PASSWORD
$fleetOldURL=$env:LINEA_FLEET_TEST_DATABASE_URL
$fleetOldProcs=$env:GOMAXPROCS
$fleetOldRecords=$env:LINEA_FLEET_TEST_INVENTORY_RECORDS
$fleetCreated=$false
Push-Location $fleetTestRoot
try {
    $env:POSTGRES_PASSWORD=[Convert]::ToHexString([System.Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
    $fleetImage='postgres:17@sha256:b8faeff7712663c2773a190181e1246e37e364b887611c04658c28b44e2408fe'
    $null=& docker run --detach --name $fleetTestName --cpus 1 --memory 256m --env POSTGRES_PASSWORD --env POSTGRES_DB=fleet --publish '127.0.0.1::5432' --tmpfs /var/lib/postgresql/data $fleetImage
    if($LASTEXITCODE -ne 0){throw 'PostgreSQL fixture could not start'}
    $fleetCreated=$true
    $fleetDeadline=[DateTime]::UtcNow.AddSeconds(30)
    do {
        & docker exec $fleetTestName pg_isready -h 127.0.0.1 -U postgres -d fleet *> $null
        if($LASTEXITCODE -eq 0){break}
        if([DateTime]::UtcNow -gt $fleetDeadline){throw 'PostgreSQL readiness timed out'}
        Start-Sleep -Milliseconds 200
    } while($true)
    $fleetPortLine=& docker port $fleetTestName 5432/tcp
    $fleetPort=($fleetPortLine -split ':')[-1]
    $env:LINEA_FLEET_TEST_DATABASE_URL="postgres://postgres:$env:POSTGRES_PASSWORD@127.0.0.1:$fleetPort/fleet?sslmode=disable"
    $env:GOMAXPROCS='2'
    if($InventoryRecords -gt 0){
        $env:LINEA_FLEET_TEST_INVENTORY_RECORDS=[string]$InventoryRecords
        & go test -p 1 -count 1 -v -run '^TestPostgresInventoryLatency$' ./internal/fleet/postgres
    } elseif($Race){
        # The container reaches the host-bound fixture over the Docker network.
        $fleetIP=& docker inspect $fleetTestName --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}'
        $env:LINEA_FLEET_TEST_DATABASE_URL="postgres://postgres:$env:POSTGRES_PASSWORD@${fleetIP}:5432/fleet?sslmode=disable"
        & docker run --rm --cpus 2 --memory 2g --mount "type=bind,source=$fleetTestRoot,target=/src,readonly" --workdir /src --env GOMAXPROCS=2 --env LINEA_FLEET_TEST_DATABASE_URL golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9 go test -mod=readonly -race -p 1 -count 1 -run '^TestPostgres(Lifecycle|Replicas|Inventory|ReapBudget|RemoteConfig|Configs|BootstrapToken)$' ./internal/fleet/postgres
    } else {
        & go test -p 1 -count 1 -run '^TestPostgres(Lifecycle|Replicas|Inventory|ReapBudget|RemoteConfig|Configs|BootstrapToken)$' ./internal/fleet/postgres
    }
    if($LASTEXITCODE -ne 0){throw 'PostgreSQL lifecycle checks failed'}
} finally {
    if($fleetCreated){$null=& docker rm --force $fleetTestName}
    $env:POSTGRES_PASSWORD=$fleetOldPassword
    $env:LINEA_FLEET_TEST_DATABASE_URL=$fleetOldURL
    $env:GOMAXPROCS=$fleetOldProcs
    $env:LINEA_FLEET_TEST_INVENTORY_RECORDS=$fleetOldRecords
    Pop-Location
}
