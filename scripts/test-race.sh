#!/bin/bash
# Runs `go test -race` over the given packages, the shell twin of test-race.ps1.
# The race detector needs cgo and a C compiler. Pass the packages and the extra
# `go test` arguments explicitly; the default is the whole module.
#
#   scripts/test-race.sh [--cc COMPILER] [--packages LIST] [--parallel N] \
#       [--test-arg ARG]... [-- GO_TEST_ARG...]
#
#   --cc COMPILER   C compiler for cgo; defaults to $CC_RACE, then $CC, then cc
#   --packages LIST packages to test, comma separated; default ./...
#   --parallel N    test binaries built and run at once (go test -p); default
#                   2. Each race-instrumented link peaks at several GB, so raise
#                   it only with memory to spare.
#   --test-arg ARG  one extra `go test` argument, repeatable (for example
#                   --test-arg -run --test-arg '^TestX$'); every argument after
#                   a bare `--` is passed the same way
#   -h, --help      print this text
#
#   scripts/test-race.sh --packages ./internal/debugger/platform \
#       -- -run '^TestX$' -count=1
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)

usage() { awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; }
die() { echo "$*" >&2; exit 1; }

cc=${CC_RACE:-${CC:-cc}}
packages='./...'
parallel=2
test_args=()
while [ $# -gt 0 ]; do
    case $1 in
        --cc) [ $# -ge 2 ] || { usage >&2; exit 2; }; cc=$2; shift;;
        --packages) [ $# -ge 2 ] || { usage >&2; exit 2; }; packages=$2; shift;;
        --parallel) [ $# -ge 2 ] || { usage >&2; exit 2; }; parallel=$2; shift;;
        --test-arg) [ $# -ge 2 ] || { usage >&2; exit 2; }; test_args+=("$2"); shift;;
        --) shift; while [ $# -gt 0 ]; do test_args+=("$1"); shift; done; break;;
        -h|--help) usage; exit 0;;
        *) echo "unknown argument: $1" >&2; usage >&2; exit 2;;
    esac
    shift
done
case $parallel in
    ''|*[!0-9]*) echo "--parallel must be a positive integer" >&2; exit 2;;
esac

command -v "$cc" >/dev/null 2>&1 \
    || die "C compiler not found: $cc (the race detector needs cgo; install gcc or clang, or pass --cc)"

package_list=()
old_ifs=$IFS
IFS=,
for p in $packages; do package_list+=("$p"); done
IFS=$old_ifs

export CGO_ENABLED=1
export CC="$cc"

cd "$root"
# The race detector slows the Collector fidelity tests several times over.
go test -race -p "$parallel" -timeout 60m \
    ${test_args[@]+"${test_args[@]}"} "${package_list[@]}" \
    || die 'race tests failed'
