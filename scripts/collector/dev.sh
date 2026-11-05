#!/bin/bash
# Collector component development tasks, the shell twin of dev.ps1.
#
#   scripts/collector/dev.sh [--task] TASK
#
# TASK (case-insensitive; default Doctor):
#   Setup           install the pinned OCB into .tools/bin
#   Build           build collector/distribution with the installed OCB
#   Validate        validate the local Collector configuration
#   Test            go test and go vet for collector/livecapture,
#                   collector/tools/testlab and collector/distribution
#   Race            go test -race for the same modules
#   Smoke           run scripts/collector/smoke.sh against the built Collector
#   Run             run the built Collector with the local configuration
#   Testbed         capture processor benchmarks on the Collector Contrib testbed
#                   (collector/tools/testbed; TESTCASE_DURATION sets the per-case time)
#   Doctor          report the installed tools and the build state
#   DockerBuild     build the Collector image
#   DockerTest      race tests, then the smoke and capture-smoke checks, in containers
#   DockerSmoke     the container smoke check
#   DockerRun       build and run the Collector in the foreground
#   DockerDown      remove the project containers
#   DockerLab       build and start the capture test lab in the background
#   DockerLabCheck  exercise the running lab
#   DockerLabDown   stop and remove the lab
#   DockerPerf      run the container performance experiment
#   DockerProto     regenerate the live-capture protocol code (scripts/gen-proto.sh)
#   -h, --help      print this text
#
# Go caches and tools stay inside the repository (.cache/, .tools/bin);
# the user's global Go settings are not modified.
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)

usage() { awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; }

task=Doctor
while [ $# -gt 0 ]; do
    case $1 in
        --task) [ $# -ge 2 ] || { usage >&2; exit 2; }; task=$2; shift;;
        -h|--help) usage; exit 0;;
        -*) echo "unknown argument: $1" >&2; usage >&2; exit 2;;
        *) task=$1;;
    esac
    shift
done

compose_directory=deploy/compose/collector
builder_version=v0.162.0
case "$(uname -s)" in
    MINGW*|MSYS*|CYGWIN*) binary_suffix=.exe;;
    *) binary_suffix=;;
esac
builder=$root/.tools/bin/builder$binary_suffix
collector=$root/collector/distribution/otelcol-livecapture$binary_suffix
config=$root/$compose_directory/config/collector.local.yaml

compose() { docker compose -f "$compose_directory/$1" "${@:2}"; }

canonical=
for name in Setup Build Validate Test Race Smoke Run Testbed Doctor DockerBuild DockerTest DockerSmoke \
    DockerRun DockerDown DockerLab DockerLabCheck DockerLabDown DockerPerf DockerProto; do
    lower_a=$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')
    lower_b=$(printf '%s' "$task" | tr '[:upper:]' '[:lower:]')
    if [ "$lower_a" = "$lower_b" ]; then canonical=$name; fi
done
if [ -z "$canonical" ]; then
    echo "unknown task: $task" >&2
    usage >&2
    exit 2
fi
task=$canonical

cd "$root"

case $task in
    DockerBuild) compose compose.yaml build collector;;
    DockerTest)
        compose compose.yaml --profile test run --build --rm tests
        compose compose.yaml --profile test run --build --rm smoke
        compose compose.yaml --profile test run --build --rm capture-smoke;;
    DockerSmoke) compose compose.yaml --profile test run --build --rm smoke;;
    DockerRun) compose compose.yaml up --build collector;;
    DockerDown) compose compose.yaml down;;
    DockerLab)
        compose compose.lab.yaml up --build -d
        echo 'Capture test lab: http://localhost:8080';;
    DockerLabCheck) compose compose.lab.yaml exec -T lab /testlab -check;;
    DockerLabDown) compose compose.lab.yaml down;;
    DockerPerf) compose compose.perf.yaml run --build --rm -T perf;;
    DockerProto) bash scripts/gen-proto.sh;;
    *)
        # Keep downloads and tools local; do not modify the user's global Go settings.
        GOCACHE=$root/.cache/go-build
        GOMODCACHE=$root/.cache/go-mod
        GOPATH=$root/.cache/go
        GOBIN=$root/.tools/bin
        export GOCACHE GOMODCACHE GOPATH GOBIN
        mkdir -p "$GOCACHE" "$GOMODCACHE" "$GOPATH" "$GOBIN"
        case $task in
            Doctor)
                git --version
                if command -v go >/dev/null 2>&1; then
                    go version
                else
                    echo 'Host Go is not installed; Docker supplies the toolchain.'
                fi
                echo "Builder pin: $builder_version (requires Go 1.26+)"
                if [ -e "$builder" ]; then echo 'Builder installed: True'; else echo 'Builder installed: False'; fi
                if [ -e "$collector" ]; then echo 'Collector built: True'; else echo 'Collector built: False'; fi
                docker version
                echo 'Use DockerBuild and DockerTest for the container workflow, including Linux race checks.';;
            Setup)
                go install "go.opentelemetry.io/collector/cmd/builder@$builder_version"
                go version -m "$builder";;
            Build)
                [ -e "$builder" ] || { echo 'Run scripts/collector/dev.sh Setup first.' >&2; exit 1; }
                # OCB writes unformatted Go; format it so a rebuild leaves no diff.
                (cd "$root/collector" && "$builder" --config distribution/builder-config.yaml && gofmt -w distribution);;
            Validate)
                "$collector" validate --config "$config";;
            Test)
                for module in collector/livecapture collector/tools/testlab collector/distribution; do
                    (cd "$root/$module" && go test ./... && go vet ./...)
                done;;
            Race)
                for module in collector/livecapture collector/tools/testlab collector/distribution; do
                    (cd "$root/$module" && go test -race ./...)
                done;;
            Smoke)
                bash "$root/scripts/collector/smoke.sh" --collector "$collector" --config "$config";;
            Run)
                "$collector" --config "$config";;
            Testbed)
                # Outside go.work: contrib's testbed would raise the
                # workspace's dependency versions for every other module.
                (cd "$root/collector/tools/testbed" && RUN_TESTBED=1 GOWORK=off go test -count=1 -v -timeout 60m .)
                echo 'Results: collector/tools/testbed/results/TESTRESULTS.md';;
        esac;;
esac
