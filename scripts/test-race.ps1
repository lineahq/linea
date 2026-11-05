[CmdletBinding()]
param(
    # A MinGW-w64 C compiler producing native Windows code; the race detector
    # needs cgo. Cygwin's plain gcc targets Cygwin and is refused by Go.
    [string]$CC = $(if ($env:CC_RACE) { $env:CC_RACE } else { 'C:\dev\tools\cygwin64\bin\x86_64-w64-mingw32-gcc.exe' }),
    # Packages to test; defaults to the whole module.
    [string[]]$Packages = @('./...'),
    # Extra `go test` arguments, for example -run or -count.
    [string[]]$TestArgs = @(),
    # Test binaries built and run at once. Each race-instrumented link peaks
    # at several GB, so Go's default (one per CPU) exhausts memory on a
    # 16 GB machine. Raise it only with memory to spare.
    [int]$Parallel = 2
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot

if (-not (Test-Path $CC)) {
    throw "C compiler not found: $CC (install Cygwin's mingw64-x86_64-gcc-core or pass -CC)"
}

$env:CGO_ENABLED = '1'
$env:CC = $CC
# The compiler loads its DLLs and helper programs from its own directory.
# Appended, not prepended: Cygwin's bin also holds a git.exe that cannot
# read Windows paths, and the build-source tests run git.
$env:PATH = "$env:PATH;$(Split-Path -Parent $CC)"

Push-Location $repoRoot
try {
    # The race detector slows the Collector fidelity tests several times over.
    go test -race -p $Parallel -timeout 60m @TestArgs @Packages
    if ($LASTEXITCODE -ne 0) { throw 'race tests failed' }
}
finally {
    Pop-Location
}
