[CmdletBinding()]
param(
    [ValidateSet('Setup', 'Build', 'Validate', 'Test', 'Race', 'Smoke', 'Run', 'Testbed', 'Doctor',
        'DockerBuild', 'DockerTest', 'DockerSmoke', 'DockerRun', 'DockerDown',
        'DockerLab', 'DockerLabCheck', 'DockerLabDown', 'DockerPerf', 'DockerProto')]
    [string]$Task = 'Doctor'
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$composeDirectory = 'deploy/compose/collector'
$builderVersion = 'v0.162.0'
$isWindowsHost = $env:OS -eq 'Windows_NT'
$binarySuffix = if ($isWindowsHost) { '.exe' } else { '' }
$builder = Join-Path $repoRoot ".tools/bin/builder$binarySuffix"
$collector = Join-Path $repoRoot "collector/distribution/otelcol-livecapture$binarySuffix"
$config = Join-Path $repoRoot "$composeDirectory/config/collector.local.yaml"

function Invoke-Checked {
    param([string]$Program, [string[]]$Arguments)
    & $Program @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$Program exited with code $LASTEXITCODE."
    }
}

function Initialize-GoEnvironment {
    # Keep downloads and tools local; do not modify the user's global Go settings.
    $env:GOCACHE = Join-Path $repoRoot '.cache/go-build'
    $env:GOMODCACHE = Join-Path $repoRoot '.cache/go-mod'
    $env:GOPATH = Join-Path $repoRoot '.cache/go'
    $env:GOBIN = Join-Path $repoRoot '.tools/bin'
    foreach ($directory in @($env:GOCACHE, $env:GOMODCACHE, $env:GOPATH, $env:GOBIN)) {
        New-Item -ItemType Directory -Force -Path $directory | Out-Null
    }
}

Push-Location $repoRoot
$previousGoEnvironment = @{}
foreach ($name in @('GOCACHE', 'GOMODCACHE', 'GOPATH', 'GOBIN', 'GOWORK', 'RUN_TESTBED')) {
    $previousGoEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}
try {
    if ($Task.StartsWith('Docker')) {
        switch ($Task) {
            'DockerBuild' { Invoke-Checked 'docker' @('compose', '-f', "$composeDirectory/compose.yaml", 'build', 'collector') }
            'DockerTest' {
                Invoke-Checked 'docker' @('compose', '-f', "$composeDirectory/compose.yaml", '--profile', 'test', 'run', '--build', '--rm', 'tests')
                Invoke-Checked 'docker' @('compose', '-f', "$composeDirectory/compose.yaml", '--profile', 'test', 'run', '--build', '--rm', 'smoke')
                Invoke-Checked 'docker' @('compose', '-f', "$composeDirectory/compose.yaml", '--profile', 'test', 'run', '--build', '--rm', 'capture-smoke')
            }
            'DockerSmoke' { Invoke-Checked 'docker' @('compose', '-f', "$composeDirectory/compose.yaml", '--profile', 'test', 'run', '--build', '--rm', 'smoke') }
            'DockerRun' { Invoke-Checked 'docker' @('compose', '-f', "$composeDirectory/compose.yaml", 'up', '--build', 'collector') }
            'DockerDown' { Invoke-Checked 'docker' @('compose', '-f', "$composeDirectory/compose.yaml", 'down') }
            'DockerLab' {
                Invoke-Checked 'docker' @('compose', '-f', "$composeDirectory/compose.lab.yaml", 'up', '--build', '-d')
                Write-Output 'Capture test lab: http://localhost:8080'
            }
            'DockerLabCheck' { Invoke-Checked 'docker' @('compose', '-f', "$composeDirectory/compose.lab.yaml", 'exec', '-T', 'lab', '/testlab', '-check') }
            'DockerLabDown' { Invoke-Checked 'docker' @('compose', '-f', "$composeDirectory/compose.lab.yaml", 'down') }
            'DockerPerf' { Invoke-Checked 'docker' @('compose', '-f', "$composeDirectory/compose.perf.yaml", 'run', '--build', '--rm', '-T', 'perf') }
            'DockerProto' { Invoke-Checked 'bash' @('scripts/gen-proto.sh') }
        }
        return
    }
    Initialize-GoEnvironment
    switch ($Task) {
        'Doctor' {
            Invoke-Checked 'git' @('--version')
            if (Get-Command 'go' -ErrorAction SilentlyContinue) {
                Invoke-Checked 'go' @('version')
            } else { Write-Output 'Host Go is not installed; Docker supplies the toolchain.' }
            Write-Output "Builder pin: $builderVersion (requires Go 1.26+)"
            Write-Output "Builder installed: $(Test-Path -LiteralPath $builder)"
            Write-Output "Collector built: $(Test-Path -LiteralPath $collector)"
            Invoke-Checked 'docker' @('version')
            Write-Output 'Use DockerBuild and DockerTest for the container workflow, including Linux race checks.'
        }
        'Setup' {
            Invoke-Checked 'go' @('install', "go.opentelemetry.io/collector/cmd/builder@$builderVersion")
            Invoke-Checked 'go' @('version', '-m', $builder)
        }
        'Build' {
            if (-not (Test-Path -LiteralPath $builder)) { throw 'Run scripts/collector/dev.ps1 Setup first.' }
            Push-Location (Join-Path $repoRoot 'collector')
            # OCB writes unformatted Go; format it so a rebuild leaves no diff.
            try { Invoke-Checked $builder @('--config', 'distribution/builder-config.yaml'); Invoke-Checked 'gofmt' @('-w', 'distribution') }
            finally { Pop-Location }
        }
        'Validate' {
            Invoke-Checked $collector @('validate', '--config', $config)
        }
        'Test' {
            foreach ($module in @('collector/livecapture', 'collector/tools/testlab', 'collector/distribution')) {
                Push-Location (Join-Path $repoRoot $module)
                try {
                    Invoke-Checked 'go' @('test', './...')
                    Invoke-Checked 'go' @('vet', './...')
                } finally { Pop-Location }
            }
        }
        'Race' {
            foreach ($module in @('collector/livecapture', 'collector/tools/testlab', 'collector/distribution')) {
                Push-Location (Join-Path $repoRoot $module)
                try { Invoke-Checked 'go' @('test', '-race', './...') }
                finally { Pop-Location }
            }
        }
        'Smoke' {
            & (Join-Path $PSScriptRoot 'smoke.ps1') -Collector $collector -Config $config
        }
        'Run' {
            Invoke-Checked $collector @('--config', $config)
        }
        'Testbed' {
            # Outside go.work: contrib's testbed would raise the workspace's
            # dependency versions for every other module.
            $env:GOWORK = 'off'
            $env:RUN_TESTBED = '1'
            Push-Location (Join-Path $repoRoot 'collector/tools/testbed')
            try { Invoke-Checked 'go' @('test', '-count=1', '-v', '-timeout', '60m', '.') }
            finally { Pop-Location }
            Write-Output 'Results: collector/tools/testbed/results/TESTRESULTS.md'
        }
    }
} finally {
    foreach ($name in $previousGoEnvironment.Keys) {
        [Environment]::SetEnvironmentVariable($name, $previousGoEnvironment[$name], 'Process')
    }
    Pop-Location
}
