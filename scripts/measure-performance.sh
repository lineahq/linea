#!/bin/bash
# Measures the performance workloads (testdata/perf/README.md) against the
# documented targets: the built CLI end to end (Collector startup included,
# which is stricter than the targets), then the in-process Go benchmarks. The
# shell twin of measure-performance.ps1. Needs go and python3 (or python).
#
#   scripts/measure-performance.sh [--repeat 5] [--out FILE]
#
#   --repeat N    timed repetitions per workload; the median is reported
#                 (default 5)
#   --out FILE    where the results are written (a relative path is taken from
#                 the repository root); default .local/perf/performance-<date>.txt
#   -h, --help    print this text
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)

usage() { awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; }
die() { echo "$*" >&2; exit 1; }

repeat=5
out=
while [ $# -gt 0 ]; do
    case $1 in
        --repeat) [ $# -ge 2 ] || { usage >&2; exit 2; }; repeat=$2; shift;;
        --out) [ $# -ge 2 ] || { usage >&2; exit 2; }; out=$2; shift;;
        -h|--help) usage; exit 0;;
        *) echo "unknown argument: $1" >&2; usage >&2; exit 2;;
    esac
    shift
done
case $repeat in
    ''|*[!0-9]*|0) echo '--repeat must be a positive integer' >&2; exit 2;;
esac
python=$(command -v python3 || command -v python || true)
[ -n "$python" ] || die 'python3 is required'
if [ -z "$out" ]; then
    out=$root/.local/perf/performance-$(date +%Y-%m-%d).txt
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/linea-perf-XXXXXX")
trap 'rm -rf "$work"' EXIT
trap 'exit 1' INT TERM
binary=$work/linea$(go env GOEXE)

# time_workload NAME TARGET ARG... prints one table row. The binary runs with
# stdout copied to a file as raw bytes and the whole process is timed in
# Python, so neither a shell redirection nor the shell's own startup is in the
# measurement. Arguments are workload paths and flags without spaces.
time_workload() {
    "$python" - "$binary" "$root" "$repeat" "$work/out.json" "$@" <<'PYEOF'
import subprocess, sys, time, os

binary, root, repeat, report, name, target = sys.argv[1:7]
args = sys.argv[7:] + ["--format", "json"]
times = []
for _ in range(int(repeat)):
    with open(report, "wb") as stdout:
        start = time.perf_counter()
        code = subprocess.call([binary] + args, stdout=stdout, cwd=root)
        elapsed = (time.perf_counter() - start) * 1000.0
    if code != 0:
        sys.stderr.write("%s exited %d\n" % (name, code))
        sys.exit(1)
    times.append(elapsed)
times.sort()
median = times[len(times) // 2]
size = os.path.getsize(report)
print("| %s | %s ms | %s ms | %s ms | %s | %s |" % (
    name, format(median, ",.0f"), format(times[0], ",.0f"),
    format(times[-1], ",.0f"), format(size, ","), target))
PYEOF
}

cd "$root"
go build -o "$binary" ./cmd/linea || die 'build failed'

cpu=unknown
if [ -r /proc/cpuinfo ]; then
    cpu=$(sed -n 's/^model name[[:space:]]*:[[:space:]]*//p' /proc/cpuinfo | head -n 1)
elif command -v sysctl >/dev/null 2>&1; then
    cpu=$(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)
fi
commit=$(git rev-parse --short HEAD)
if [ -n "$(git status --porcelain)" ]; then commit="$commit (dirty)"; fi
stamp=$(date +%Y-%m-%dT%H:%M:%S%z | sed 's/\([0-9][0-9]\)$/:\1/')

lines=$work/lines.txt
{
    echo '# Performance measurement'
    echo "date:    $stamp"
    echo "commit:  $commit"
    echo "go:      $(go version)"
    echo "os:      $(uname -sr)"
    echo "cpu:     $cpu"
    echo "repeat:  $repeat (median; whole process, Collector startup included)"
    echo ''
    echo '| Workload | Median | Min | Max | Report bytes | Target |'
    echo '| --- | --- | --- | --- | --- | --- |'
} > "$lines"

{
    time_workload 'W1 inspect, 100 components' 'under 2 s' \
        inspect --config testdata/perf/components-100.yaml
    time_workload 'W2 run, 100 records, synchronous' 'under 5 s' \
        run --config testdata/perf/sync-logs.yaml --fixture testdata/perf/logs-100.json
    time_workload 'W3 run, 1,000 records, synchronous' '-' \
        run --config testdata/perf/sync-logs.yaml --fixture testdata/perf/logs-1000.json
    time_workload 'W4 run, 100 records through 100 components' '-' \
        run --config testdata/perf/components-100.yaml --fixture testdata/perf/logs-100.json
} >> "$lines"

{
    echo ''
    echo '## In-process benchmarks (internal/debugger/engine)'
    echo ''
} >> "$lines"
bench=$work/bench.txt
if ! go test ./internal/debugger/engine -run '^$' -bench . -benchtime "${repeat}x" -count 1 > "$bench" 2>&1; then
    cat "$bench"
    die 'benchmarks failed'
fi
sed 's/^/    /' "$bench" >> "$lines"

mkdir -p "$(dirname "$out")"
cp "$lines" "$out"
cat "$lines"
echo "written to $out"
