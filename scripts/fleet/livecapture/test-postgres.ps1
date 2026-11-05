[CmdletBinding()]
param([switch]$Race)
$ErrorActionPreference='Stop'
$captureRoot=Split-Path (Split-Path (Split-Path $PSScriptRoot -Parent) -Parent) -Parent
$captureFixture='capture-pg-test-'+[Guid]::NewGuid().ToString('N')
$captureOldPassword=$env:POSTGRES_PASSWORD
$captureOldURL=$env:OTEL_CAPTURE_TEST_DATABASE_URL
$captureOldProcs=$env:GOMAXPROCS
$captureCreated=$false
try{
    $env:POSTGRES_PASSWORD=[Convert]::ToHexString([System.Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
    $null=& docker run --detach --name $captureFixture --cpus 1 --memory 256m --pids-limit 128 --env POSTGRES_PASSWORD --env POSTGRES_DB=capture_fixture --publish '127.0.0.1::5432' --tmpfs /var/lib/postgresql/data postgres:17@sha256:b8faeff7712663c2773a190181e1246e37e364b887611c04658c28b44e2408fe
    if($LASTEXITCODE -ne 0){throw 'Disposable capture database startup failed'}
    $captureCreated=$true
    $captureDeadline=[DateTime]::UtcNow.AddSeconds(30)
    do{& docker exec $captureFixture pg_isready -h 127.0.0.1 -U postgres -d capture_fixture *> $null;if($LASTEXITCODE -eq 0){break};if([DateTime]::UtcNow -gt $captureDeadline){throw 'Capture fixture readiness failed'};Start-Sleep -Milliseconds 200}while($true)
    $capturePort=(& docker port $captureFixture 5432/tcp) -replace '^.*:',''
    $env:OTEL_CAPTURE_TEST_DATABASE_URL="postgres://postgres:$env:POSTGRES_PASSWORD@127.0.0.1:$capturePort/capture_fixture?sslmode=disable"
    $env:GOMAXPROCS='2'
    if($Race){
        $captureIP=& docker inspect $captureFixture --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}'
        $env:OTEL_CAPTURE_TEST_DATABASE_URL="postgres://postgres:$env:POSTGRES_PASSWORD@${captureIP}:5432/capture_fixture?sslmode=disable"
        $captureFleetRoot=$captureRoot
        & docker run --rm --cpus 2 --memory 2g --mount "type=bind,source=$captureFleetRoot,target=/src,readonly" --workdir /src --env GOMAXPROCS=2 --env OTEL_CAPTURE_TEST_DATABASE_URL golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9 go test -mod=readonly -race -p 1 -count=1 -run '^TestPostgresReservationRecoveryAndGrants$' ./internal/fleetplugins/livecapture
    }else{& go -C $captureRoot test -p 1 -count=1 -run '^TestPostgresReservationRecoveryAndGrants$' ./internal/fleetplugins/livecapture}
    if($LASTEXITCODE -ne 0){throw 'Capture database checks failed'}
}finally{
    if($captureCreated){$null=& docker rm --force $captureFixture}
    $env:POSTGRES_PASSWORD=$captureOldPassword
    $env:OTEL_CAPTURE_TEST_DATABASE_URL=$captureOldURL
    $env:GOMAXPROCS=$captureOldProcs
}
