[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$toolBin = Join-Path $repoRoot '.tools/bin'
$env:GOBIN = $toolBin

New-Item -ItemType Directory -Force -Path $toolBin | Out-Null
go install go.opentelemetry.io/collector/cmd/builder@v0.162.0

& (Join-Path $toolBin 'builder.exe') version
