[CmdletBinding()]
param(
    [switch]$GenerateOnly
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$builder = Join-Path $repoRoot '.tools/bin/builder.exe'
$manifest = Join-Path $repoRoot 'build/linea-runtime.yaml'
$distributionRoot = Join-Path $repoRoot 'dist'
$distributionPath = Join-Path $distributionRoot 'linea-runtime'

if (-not (Test-Path $builder)) {
    & (Join-Path $PSScriptRoot 'install-tools.ps1')
}

# OCB creates only its final output directory, not its parent on every platform.
New-Item -ItemType Directory -Force -Path $distributionRoot | Out-Null

# OCB requires an empty output directory. This is restricted to its own ignored
# generated distribution and never touches user Collector configurations.
if (Test-Path $distributionPath) {
    Remove-Item -LiteralPath $distributionPath -Recurse -Force
}

$arguments = @("--config=$manifest")
if ($GenerateOnly) {
    $arguments += '--skip-compilation'
}

Push-Location $repoRoot
try {
    & $builder @arguments
} finally {
    Pop-Location
}
