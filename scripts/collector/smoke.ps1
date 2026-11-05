[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Collector,
    [Parameter(Mandatory = $true)][string]$Config
)

$ErrorActionPreference = 'Stop'
$artifactDirectory = Join-Path (Split-Path -Parent (Split-Path -Parent $PSScriptRoot)) '.artifacts'
New-Item -ItemType Directory -Force -Path $artifactDirectory | Out-Null
$stdoutPath = Join-Path $artifactDirectory 'smoke.stdout.log'
$stderrPath = Join-Path $artifactDirectory 'smoke.stderr.log'

function Test-LocalPort {
    param([int]$Port)
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $connection = $client.ConnectAsync('127.0.0.1', $Port)
        return ($connection.Wait(200) -and $client.Connected)
    } catch { return $false }
    finally { $client.Dispose() }
}

foreach ($port in @(4317, 4318)) {
    if (Test-LocalPort $port) { throw "Port $port is occupied. Stop the local Collector before running Smoke." }
}

$startParameters = @{
    FilePath = $Collector
    ArgumentList = @('--config', ('"{0}"' -f $Config))
    PassThru = $true
    RedirectStandardOutput = $stdoutPath
    RedirectStandardError = $stderrPath
}
if ($env:OS -eq 'Windows_NT') { $startParameters.WindowStyle = 'Hidden' }
$process = Start-Process @startParameters
try {
    $deadline = [DateTime]::UtcNow.AddSeconds(20)
    do {
        if ($process.HasExited) { throw "Collector exited during startup. See $stderrPath" }
        if (Test-LocalPort 4318) { break }
        Start-Sleep -Milliseconds 200
    } while ([DateTime]::UtcNow -lt $deadline)
    if (-not (Test-LocalPort 4318)) { throw 'Collector did not become ready within 20 seconds.' }

    $marker = 'livecapture-smoke-' + [Guid]::NewGuid().ToString('N')
    $now = ([DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds() * 1000000L).ToString()
    $resource = @{ attributes = @(@{ key = 'service.name'; value = @{ stringValue = $marker } }) }
    $scope = @{ name = 'livecapture.bootstrap' }
    $payloads = @{
        traces = @{ resourceSpans = @(@{
            resource = $resource
            scopeSpans = @(@{
                scope = $scope
                spans = @(@{
                    traceId = '0123456789abcdef0123456789abcdef'
                    spanId = '0123456789abcdef'
                    name = "$marker-span"
                    kind = 1
                    startTimeUnixNano = $now
                    endTimeUnixNano = $now
                })
            })
        }) }
        metrics = @{ resourceMetrics = @(@{
            resource = $resource
            scopeMetrics = @(@{
                scope = $scope
                metrics = @(@{
                    name = "$marker-metric"
                    gauge = @{ dataPoints = @(@{ timeUnixNano = $now; asInt = '1' }) }
                })
            })
        }) }
        logs = @{ resourceLogs = @(@{
            resource = $resource
            scopeLogs = @(@{
                scope = $scope
                logRecords = @(@{
                    timeUnixNano = $now
                    severityNumber = 9
                    body = @{ stringValue = "$marker-log" }
                })
            })
        }) }
    }

    foreach ($signal in @('traces', 'metrics', 'logs')) {
        $body = $payloads[$signal] | ConvertTo-Json -Depth 20 -Compress
        $response = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:4318/v1/$signal" -Method Post -ContentType 'application/json' -Body $body
        if ($response.StatusCode -ne 200) { throw "$signal request failed: $($response.StatusCode)" }
        $result = $response.Content | ConvertFrom-Json
        $rejectedField = switch ($signal) {
            'traces' { 'rejectedSpans' }
            'metrics' { 'rejectedDataPoints' }
            'logs' { 'rejectedLogRecords' }
        }
        $partial = $result.partialSuccess
        # An empty partialSuccess object represents zero rejected records.
        if ($null -ne $partial -and
            ([decimal]$partial.$rejectedField -gt 0 -or -not [string]::IsNullOrEmpty($partial.errorMessage))) {
            throw "$signal returned partial success: $($response.Content)"
        }
    }

    $deadline = [DateTime]::UtcNow.AddSeconds(10)
    do {
        $output = (Get-Content -LiteralPath $stdoutPath, $stderrPath -Raw -ErrorAction SilentlyContinue) -join "`n"
        $allSeen = $true
        foreach ($suffix in @('span', 'metric', 'log')) {
            if (-not $output.Contains("$marker-$suffix")) { $allSeen = $false }
        }
        if ($allSeen) { break }
        Start-Sleep -Milliseconds 200
    } while ([DateTime]::UtcNow -lt $deadline)
    if (-not $allSeen) { throw "Telemetry was accepted but not all three signals reached the debug exporter. See $artifactDirectory" }
    Write-Output 'Smoke passed: traces, metrics, and logs reached the local debug exporter.'
} finally {
    if (-not $process.HasExited) { Stop-Process -Id $process.Id -Force }
    $process.Dispose()
}
