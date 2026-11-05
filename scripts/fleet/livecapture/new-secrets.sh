#!/bin/sh
# Creates the livecapture plugin's private secrets, the shell twin of
# new-secrets.ps1: host/service tokens, database passwords and URLs, a
# metrics token, a 90-day self-signed TLS identity and config.json. Pass a
# directory outside the checkout for a real installation; the default is
# .local/livecapture in the repository. An existing directory is refused.
# The files are made in a staging directory beside it and moved into place
# only when all exist, so a failure leaves no partial directory behind.
set -eu
umask 077
root=$(cd "$(dirname "$0")/../../.." && pwd)
directory=${1:-$root/.local/livecapture}
if [ -e "$directory" ]; then
    echo 'Livecapture secret directory exists; preserve it or choose another.' >&2
    exit 1
fi
staging="$(dirname "$directory")/.$(basename "$directory").partial.$$"
sh "$root/scripts/fleet/new-plugin-secrets.sh" "$staging" >/dev/null
trap 'rm -rf -- "$staging"' EXIT

write() {
    printf '%s' "$2" > "$staging/$1"
    chmod 444 "$staging/$1"
}
token() { openssl rand -hex 32 | tr -d '\r\n'; }

admin=$(token)
migrate=$(token)
runtime=$(token)
write postgres_password "$admin"
write migrate_password "$migrate"
write runtime_password "$runtime"
write migrate_url "postgres://capture_migrator:${migrate}@capture-postgres:5432/livecapture?sslmode=disable"
write runtime_url "postgres://capture_app:${runtime}@capture-postgres:5432/livecapture?sslmode=disable"
write metrics_token "$(token)"

# MSYS2_ARG_CONV_EXCL keeps Git Bash on Windows from rewriting /CN=... as a
# path, while it still converts the output paths for a native openssl.
if ! output=$(MSYS2_ARG_CONV_EXCL='/CN=' openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 90 \
    -subj '/CN=localhost' \
    -addext 'subjectAltName=DNS:localhost,DNS:livecapture-plugin,IP:127.0.0.1' \
    -addext 'basicConstraints=critical,CA:TRUE' \
    -keyout "$staging/tls_key.pem" -out "$staging/tls_cert.pem" 2>&1); then
    printf 'openssl could not create the TLS identity:\n%s\n' "$output" >&2
    exit 1
fi
chmod 444 "$staging/tls_key.pem" "$staging/tls_cert.pem"

write config.json '{
  "id": "livecapture",
  "version": "0.4.0",
  "host_endpoint": "http://fleet:8081",
  "allow_insecure_host": true,
  "host_token_file": "/run/secrets/capture_host_token",
  "service_token_file": "/run/secrets/capture_service_token",
  "http_listen": "0.0.0.0:8090",
  "control_listen": "0.0.0.0:4340",
  "delivery_listen": "0.0.0.0:4341",
  "tls_cert_file": "/run/secrets/capture_tls_cert",
  "tls_key_file": "/run/secrets/capture_tls_key",
  "test_insecure": false,
  "capture_label": "capture",
  "raw_principals": [],
  "max_peers": 256,
  "metrics_token_file": "/run/secrets/capture_metrics_token"
}
'
mv "$staging" "$directory"
trap - EXIT
echo 'Created private plugin credentials, database secrets and a 90-day local TLS identity; values are not displayed. Use operator-managed PKI for remote deployments.'
echo 'Collectors authenticate with fleet agent credentials. Issue one with the label capture=<group>[,<group>...] (or *) through POST /api/v1/credentials.'
