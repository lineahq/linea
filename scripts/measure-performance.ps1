[CmdletBinding()]
param(
    # Timed repetitions per workload; the median is reported.
    [int]$Repeat = 5,
    # Where the results are written; defaults to
    # .local/perf/performance-<date>.txt.
    [string]$Out = ''
)

# Measures the performance workloads (testdata/perf/README.md) against the
# documented targets: the built CLI end to end (Collector startup included, which
# is stricter than the targets), then the in-process Go benchmarks.

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
if (-not $Out) {
    $Out = Join-Path $repoRoot ".local/perf/performance-$(Get-Date -Format 'yyyy-MM-dd').txt"
}

$workloads = @(
    @{ Name = 'W1 inspect, 100 components'; Target = 'under 2 s'; Args = @('inspect', '--config', 'testdata/perf/components-100.yaml') },
    @{ Name = 'W2 run, 100 records, synchronous'; Target = 'under 5 s'; Args = @('run', '--config', 'testdata/perf/sync-logs.yaml', '--fixture', 'testdata/perf/logs-100.json') },
    @{ Name = 'W3 run, 1,000 records, synchronous'; Target = '-'; Args = @('run', '--config', 'testdata/perf/sync-logs.yaml', '--fixture', 'testdata/perf/logs-1000.json') },
    @{ Name = 'W4 run, 100 records through 100 components'; Target = '-'; Args = @('run', '--config', 'testdata/perf/components-100.yaml', '--fixture', 'testdata/perf/logs-100.json') }
)

# Invoke-Timed runs the binary with stdout copied to a file and returns its
# exit code. Arguments are workload paths and flags without spaces.
function Invoke-Timed([string]$exe, [string[]]$arguments, [string]$stdout) {
    $info = [System.Diagnostics.ProcessStartInfo]::new($exe, ($arguments -join ' '))
    $info.UseShellExecute = $false
    $info.RedirectStandardOutput = $true
    $info.WorkingDirectory = $repoRoot
    $proc = [System.Diagnostics.Process]::Start($info)
    $file = [System.IO.File]::Create($stdout)
    try { $proc.StandardOutput.BaseStream.CopyTo($file) } finally { $file.Dispose() }
    $proc.WaitForExit()
    return $proc.ExitCode
}

$work = Join-Path ([System.IO.Path]::GetTempPath()) "linea-perf-$PID"
New-Item -ItemType Directory -Force $work | Out-Null
$binary = Join-Path $work 'linea.exe'

Push-Location $repoRoot
try {
    go build -o $binary ./cmd/linea
    if ($LASTEXITCODE -ne 0) { throw 'build failed' }

    $lines = [System.Collections.Generic.List[string]]::new()
    $lines.Add('# Performance measurement')
    $lines.Add("date:    $(Get-Date -Format 'yyyy-MM-ddTHH:mm:ssK')")
    $lines.Add("commit:  $(git rev-parse --short HEAD)$(if (git status --porcelain) { ' (dirty)' })")
    $lines.Add("go:      $(go version)")
    $lines.Add("os:      $([System.Environment]::OSVersion.VersionString)")
    $lines.Add("cpu:     $((Get-CimInstance Win32_Processor | Select-Object -First 1).Name)")
    $lines.Add("repeat:  $Repeat (median; whole process, Collector startup included)")
    $lines.Add('')
    $lines.Add('| Workload | Median | Min | Max | Report bytes | Target |')
    $lines.Add('| --- | --- | --- | --- | --- | --- |')

    foreach ($w in $workloads) {
        $report = Join-Path $work 'out.json'
        $times = @()
        for ($i = 0; $i -lt $Repeat; $i++) {
            # stdout is copied to the file as raw bytes: a PowerShell
            # redirection re-encodes it, and Start-Process -Wait polls about
            # once a second; either would dominate the timing.
            $sw = [System.Diagnostics.Stopwatch]::StartNew()
            $exit = Invoke-Timed $binary ($w.Args + @('--format', 'json')) $report
            $sw.Stop()
            if ($exit -ne 0) { throw "$($w.Name) exited $exit" }
            $times += $sw.Elapsed.TotalMilliseconds
        }
        $sorted = $times | Sort-Object
        $median = $sorted[[int][math]::Floor($sorted.Count / 2)]
        $bytes = (Get-Item $report).Length
        $lines.Add([string]::Format([cultureinfo]::InvariantCulture, '| {0} | {1:N0} ms | {2:N0} ms | {3:N0} ms | {4:N0} | {5} |', $w.Name, $median, $sorted[0], $sorted[-1], $bytes, $w.Target))
    }

    $lines.Add('')
    $lines.Add('## In-process benchmarks (internal/debugger/engine)')
    $lines.Add('')
    $bench = go test ./internal/debugger/engine -run '^$' -bench . -benchtime "${Repeat}x" -count 1 2>&1
    if ($LASTEXITCODE -ne 0) { $bench | Write-Host; throw 'benchmarks failed' }
    $bench | ForEach-Object { $lines.Add("    $_") }

    New-Item -ItemType Directory -Force (Split-Path -Parent $Out) | Out-Null
    $lines | Out-File -Encoding utf8 $Out
    $lines | Write-Host
    Write-Host "written to $Out"
}
finally {
    Pop-Location
    Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}
