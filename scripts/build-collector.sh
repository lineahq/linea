#!/bin/sh
# Recreates and compiles the debug Collector into the ignored dist/ directory
# from build/linea-runtime.yaml with the pinned OCB, the shell twin of
# build-collector.ps1. Installs OCB first when .tools/bin/builder is missing.
#
#   scripts/build-collector.sh [--generate-only]
#
#   --generate-only   generate the Go source only (OCB --skip-compilation)
#   -h, --help        print this text
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)

usage() { awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"; }
generate_only=0
while [ $# -gt 0 ]; do
    case $1 in
        --generate-only) generate_only=1;;
        -h|--help) usage; exit 0;;
        *) echo "unknown argument: $1" >&2; usage >&2; exit 2;;
    esac
    shift
done

builder=$root/.tools/bin/builder
manifest=$root/build/linea-runtime.yaml
distribution_root=$root/dist
distribution_path=$distribution_root/linea-runtime

# Go appends .exe on Windows (Git Bash); look for either spelling.
if [ ! -e "$builder" ] && [ ! -e "$builder.exe" ]; then
    sh "$root/scripts/install-tools.sh"
fi

# OCB creates only its final output directory, not its parent on every platform.
mkdir -p "$distribution_root"

# OCB requires an empty output directory. This is restricted to its own ignored
# generated distribution and never touches user Collector configurations.
if [ -e "$distribution_path" ]; then
    rm -rf -- "$distribution_path"
fi

cd "$root"
if [ "$generate_only" = 1 ]; then
    "$builder" "--config=$manifest" --skip-compilation
else
    "$builder" "--config=$manifest"
fi
