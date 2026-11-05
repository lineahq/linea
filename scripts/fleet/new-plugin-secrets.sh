#!/bin/sh
set -eu
umask 077
root=$(cd "$(dirname "$0")/../.." && pwd)
directory=${1:-$root/.local/plugin-example}
if [ -e "$directory" ]; then
    echo 'Plugin secret directory exists; preserve it or choose another.' >&2
    exit 1
fi
mkdir -p "$(dirname "$directory")"
mkdir "$directory"
chmod 700 "$directory"
openssl rand -hex 32 | tr -d '\r' > "$directory/host_token"
openssl rand -hex 32 | tr -d '\r' > "$directory/service_token"
chmod 444 "$directory/host_token" "$directory/service_token"
echo 'Created private plugin host/service tokens; their values are not displayed.'
