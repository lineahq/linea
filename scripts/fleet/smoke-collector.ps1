# Requires PowerShell 7+, Docker Desktop (Linux containers), and a built Windows binary.
[CmdletBinding()]
param([string]$Binary = (Join-Path $PSScriptRoot '..\..\bin\linea-fleet.exe'))

$ErrorActionPreference = 'Stop'
$fleetRoot = Split-Path (Split-Path $PSScriptRoot -Parent) -Parent
$fleetBinary = (Resolve-Path -LiteralPath $Binary).Path
$fleetRunID = [Guid]::NewGuid().ToString('N')
$fleetContainer = "linea-fleet-smoke-$fleetRunID"
$fleetLogDir = Join-Path $fleetRoot ".local\$fleetRunID"
$null = New-Item -ItemType Directory -Path $fleetLogDir -Force
$fleetPreviousAPI = $env:LINEA_FLEET_API_TOKEN
$fleetPreviousAgent = $env:LINEA_FLEET_AGENT_TOKEN
$fleetPreviousEndpoint = $env:LINEA_FLEET_OPAMP_ENDPOINT
$fleetProcess = $null
$fleetContainerCreated = $false

function New-FleetPort {
    $fleetProbe = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    $fleetProbe.Start()
    try { return $fleetProbe.LocalEndpoint.Port } finally { $fleetProbe.Stop() }
}

function Wait-FleetCondition([scriptblock]$Check, [string]$Failure) {
    $fleetDeadline = [DateTime]::UtcNow.AddSeconds(30)
    while ([DateTime]::UtcNow -lt $fleetDeadline) {
        if ($fleetProcess.HasExited) { throw "Fleet process exited; inspect $fleetLogDir" }
        if (& $Check) { return }
        Start-Sleep -Milliseconds 200
    }
    throw $Failure
}

try {
    $env:LINEA_FLEET_API_TOKEN = [Convert]::ToHexString([System.Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
    $env:LINEA_FLEET_AGENT_TOKEN = [Convert]::ToHexString([System.Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
    $fleetManagementPort = New-FleetPort
    do { $fleetAgentPort = New-FleetPort } while ($fleetAgentPort -eq $fleetManagementPort)
    $fleetBaseURL = "http://127.0.0.1:$fleetManagementPort"
    $env:LINEA_FLEET_OPAMP_ENDPOINT = "ws://host.docker.internal:$fleetAgentPort/v1/opamp"
    # Docker Desktop reaches the temporary agent listener through the host gateway.
    # Management remains loopback-only. This local smoke is not a deployment config.
    $fleetProcess = Start-Process -FilePath $fleetBinary -ArgumentList @(
        '--storage', 'memory', '--server-id', 'collector-smoke', '--management-listen', "127.0.0.1:$fleetManagementPort",
        '--agent-listen', "0.0.0.0:$fleetAgentPort", '--allow-insecure-remote'
    ) -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $fleetLogDir 'stdout.log') -RedirectStandardError (Join-Path $fleetLogDir 'stderr.log')
    Wait-FleetCondition {
        try { return (Invoke-WebRequest "$fleetBaseURL/readyz" -TimeoutSec 2).StatusCode -eq 200 } catch { return $false }
    } 'Fleet readiness timed out'

    $fleetHeaders = @{ Authorization = "Bearer $env:LINEA_FLEET_API_TOKEN" }
    $fleetDiscovery = Invoke-RestMethod "$fleetBaseURL/api/v1/server" -Headers $fleetHeaders
    if ($fleetDiscovery.storage -ne 'memory' -or $fleetDiscovery.plugins.Count -ne 0) { throw 'Unexpected server discovery' }
    $fleetAnonymous = Invoke-WebRequest "$fleetBaseURL/api/v1/agents" -SkipHttpErrorCheck
    $fleetWrongRole = Invoke-WebRequest "$fleetBaseURL/api/v1/agents" -Headers @{ Authorization = "Bearer $env:LINEA_FLEET_AGENT_TOKEN" } -SkipHttpErrorCheck
    if ($fleetAnonymous.StatusCode -ne 401 -or $fleetWrongRole.StatusCode -ne 401) { throw 'Management auth isolation failed' }
    $fleetWrongAgent = Invoke-WebRequest "http://127.0.0.1:$fleetAgentPort/v1/opamp" -Headers $fleetHeaders -SkipHttpErrorCheck
    if ($fleetWrongAgent.StatusCode -ne 401) { throw 'Agent auth isolation failed' }

    $fleetConfig = Join-Path $fleetRoot 'examples\fleet-collector.yaml'
    $fleetImage = 'otel/opentelemetry-collector-contrib:0.162.0'
    $fleetContainerID = & docker run --detach --name $fleetContainer --cpus 1 --memory 256m --env LINEA_FLEET_AGENT_TOKEN --env LINEA_FLEET_OPAMP_ENDPOINT --mount "type=bind,source=$fleetConfig,target=/etc/otelcol-contrib/config.yaml,readonly" $fleetImage
    if ($LASTEXITCODE -ne 0) { throw 'Collector container could not start' }
    $fleetContainerCreated = $true
    Wait-FleetCondition {
        $fleetPage = Invoke-RestMethod "$fleetBaseURL/api/v1/agents?connected=true&limit=1" -Headers $fleetHeaders
        return $fleetPage.items.Count -eq 1 -and $null -ne $fleetPage.items[0].health
    } 'Collector reporting timed out'
    $fleetPage = Invoke-RestMethod "$fleetBaseURL/api/v1/agents?connected=true&limit=1" -Headers $fleetHeaders
    $fleetExpectedID = $fleetPage.items[0].id
    $fleetAgent = Invoke-RestMethod "$fleetBaseURL/api/v1/agents/$fleetExpectedID" -Headers $fleetHeaders
    if (-not $fleetAgent.connected -or -not $fleetAgent.health.healthy) { throw 'Collector did not report healthy' }
    $null = & docker stop --time 5 $fleetContainer
    if ($LASTEXITCODE -ne 0) { throw 'Collector stop failed' }
    Wait-FleetCondition {
        $fleetAgent = Invoke-RestMethod "$fleetBaseURL/api/v1/agents/$fleetExpectedID" -Headers $fleetHeaders
        return -not $fleetAgent.connected
    } 'Collector disconnect was not observed'
    $fleetDigest = & docker image inspect $fleetImage --format '{{index .RepoDigests 0}}'
    Write-Output "PASS: standalone Collector reporting, health, HTTP discovery/detail/filtering, role isolation, and disconnect."
    Write-Output "Collector: $fleetDigest"
} finally {
    if ($fleetContainerCreated) { $null = & docker rm --force $fleetContainer }
    if ($null -ne $fleetProcess -and -not $fleetProcess.HasExited) { Stop-Process -Id $fleetProcess.Id; $fleetProcess.WaitForExit() }
    $env:LINEA_FLEET_API_TOKEN = $fleetPreviousAPI
    $env:LINEA_FLEET_AGENT_TOKEN = $fleetPreviousAgent
    $env:LINEA_FLEET_OPAMP_ENDPOINT = $fleetPreviousEndpoint
}
