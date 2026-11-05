#!/bin/bash
# Starts a Collector on the local ports 4317 and 4318, posts one trace, one
# metric and one log over OTLP/HTTP and checks that all three reach its debug
# exporter. The shell twin of smoke.ps1. The Collector's output goes to
# .artifacts/smoke.stdout.log and .artifacts/smoke.stderr.log.
#
#   scripts/collector/smoke.sh --collector PATH --config PATH
#
#   --collector PATH   the Collector binary to start (required)
#   --config PATH      the Collector configuration to run it with (required)
#   -h, --help         print this text
#
# Exit status: 0 when all three signals arrive, 1 on any failure, 2 on a usage
# error. Needs curl and bash with /dev/tcp support.
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)

usage() { awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; }
die() { echo "$*" >&2; exit 1; }

collector=
config=
while [ $# -gt 0 ]; do
    case $1 in
        --collector) [ $# -ge 2 ] || { usage >&2; exit 2; }; collector=$2; shift;;
        --config) [ $# -ge 2 ] || { usage >&2; exit 2; }; config=$2; shift;;
        -h|--help) usage; exit 0;;
        *) echo "unknown argument: $1" >&2; usage >&2; exit 2;;
    esac
    shift
done
if [ -z "$collector" ] || [ -z "$config" ]; then
    echo '--collector and --config are required' >&2
    usage >&2
    exit 2
fi

artifact_directory=$root/.artifacts
mkdir -p "$artifact_directory"
stdout_path=$artifact_directory/smoke.stdout.log
stderr_path=$artifact_directory/smoke.stderr.log

port_open() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

for port in 4317 4318; do
    if port_open "$port"; then die "Port $port is occupied. Stop the local Collector before running Smoke."; fi
done

pid=
stop_collector() {
    [ -n "$pid" ] || return 0
    if kill -0 "$pid" 2>/dev/null; then
        kill "$pid" 2>/dev/null || true
        n=0
        while kill -0 "$pid" 2>/dev/null && [ "$n" -lt 50 ]; do sleep 0.1; n=$((n + 1)); done
        if kill -0 "$pid" 2>/dev/null; then kill -9 "$pid" 2>/dev/null || true; fi
    fi
    wait "$pid" 2>/dev/null || true
    pid=
}
trap stop_collector EXIT
trap 'exit 1' INT TERM

"$collector" --config "$config" > "$stdout_path" 2> "$stderr_path" &
pid=$!

deadline=$((SECONDS + 20))
while :; do
    kill -0 "$pid" 2>/dev/null || die "Collector exited during startup. See $stderr_path"
    if port_open 4318; then break; fi
    [ "$SECONDS" -lt "$deadline" ] || die 'Collector did not become ready within 20 seconds.'
    sleep 0.2
done

marker="livecapture-smoke-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
now=$(($(date +%s) * 1000000000))
resource="{\"attributes\":[{\"key\":\"service.name\",\"value\":{\"stringValue\":\"$marker\"}}]}"
scope='{"name":"livecapture.bootstrap"}'

payload() {
    case $1 in
        traces)
            printf '{"resourceSpans":[{"resource":%s,"scopeSpans":[{"scope":%s,"spans":[{"traceId":"0123456789abcdef0123456789abcdef","spanId":"0123456789abcdef","name":"%s-span","kind":1,"startTimeUnixNano":"%s","endTimeUnixNano":"%s"}]}]}]}' \
                "$resource" "$scope" "$marker" "$now" "$now";;
        metrics)
            printf '{"resourceMetrics":[{"resource":%s,"scopeMetrics":[{"scope":%s,"metrics":[{"name":"%s-metric","gauge":{"dataPoints":[{"timeUnixNano":"%s","asInt":"1"}]}}]}]}]}' \
                "$resource" "$scope" "$marker" "$now";;
        logs)
            printf '{"resourceLogs":[{"resource":%s,"scopeLogs":[{"scope":%s,"logRecords":[{"timeUnixNano":"%s","severityNumber":9,"body":{"stringValue":"%s-log"}}]}]}]}' \
                "$resource" "$scope" "$now" "$marker";;
    esac
}

response_file=$artifact_directory/smoke.response.json
for signal in traces metrics logs; do
    case $signal in
        traces) rejected=rejectedSpans;;
        metrics) rejected=rejectedDataPoints;;
        logs) rejected=rejectedLogRecords;;
    esac
    status=$(payload "$signal" | curl -s -o "$response_file" -w '%{http_code}' -X POST \
        -H 'Content-Type: application/json' --data-binary @- "http://127.0.0.1:4318/v1/$signal") \
        || die "$signal request failed"
    [ "$status" = 200 ] || die "$signal request failed: $status"
    body=$(cat "$response_file")
    # An empty partialSuccess object represents zero rejected records.
    if printf '%s' "$body" | grep -Eq "\"$rejected\"[[:space:]]*:[[:space:]]*\"?[1-9]" \
        || printf '%s' "$body" | grep -Eq '"errorMessage"[[:space:]]*:[[:space:]]*"[^"]'; then
        die "$signal returned partial success: $body"
    fi
done
rm -f "$response_file"

deadline=$((SECONDS + 10))
all_seen=0
while :; do
    all_seen=1
    for suffix in span metric log; do
        if ! cat "$stdout_path" "$stderr_path" 2>/dev/null | grep -Fq "$marker-$suffix"; then all_seen=0; fi
    done
    [ "$all_seen" = 1 ] && break
    [ "$SECONDS" -lt "$deadline" ] || break
    sleep 0.2
done
[ "$all_seen" = 1 ] || die "Telemetry was accepted but not all three signals reached the debug exporter. See $artifact_directory"
echo 'Smoke passed: traces, metrics, and logs reached the local debug exporter.'
