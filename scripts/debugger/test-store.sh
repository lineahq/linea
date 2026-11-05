#!/usr/bin/env bash
# Runs the debugger store tests (internal/debugger/store/pg), including the
# shared repository contract, against disposable PostgreSQL and SeaweedFS
# (S3 gateway) containers. Both listen on random localhost ports, run with
# bounded resources and are removed on exit. Extra arguments go to go test.
#
#   scripts/debugger/test-store.sh [-run TestContract] [-v]
set -euo pipefail
export MSYS_NO_PATHCONV=1
root="$(cd "$(dirname "$0")/../.." && pwd)"
id="$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')"
pg_name="linea-store-pg-$id"
s3_name="linea-store-s3-$id"
pg_image='postgres:17'
s3_image='chrislusf/seaweedfs:4.51'
bucket='linea-store-test'

cleanup() { docker rm --force "$pg_name" "$s3_name" >/dev/null 2>&1 || true; }
trap cleanup EXIT

random_hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }
pg_password="$(random_hex 16)"
s3_access="AK$(random_hex 8)"
s3_secret="SK$(random_hex 16)"

docker run --detach --name "$pg_name" --cpus 1 --memory 512m \
  --env POSTGRES_PASSWORD="$pg_password" --env POSTGRES_DB=linea \
  --publish '127.0.0.1::5432' --tmpfs /var/lib/postgresql/data "$pg_image" >/dev/null

# SeaweedFS: master, volume server, filer and S3 gateway in one process, with
# one static identity written from the environment into the gateway's config.
docker run --detach --name "$s3_name" --cpus 1 --memory 1g \
  --env S3_ACCESS_KEY="$s3_access" --env S3_SECRET_KEY="$s3_secret" \
  --publish '127.0.0.1::8333' --entrypoint sh "$s3_image" -c '
printf "{\"identities\":[{\"name\":\"linea\",\"credentials\":[{\"accessKey\":\"%s\",\"secretKey\":\"%s\"}],\"actions\":[\"Admin\",\"Read\",\"Write\",\"List\",\"Tagging\"]}]}" "$S3_ACCESS_KEY" "$S3_SECRET_KEY" > /tmp/s3.json
mkdir -p /tmp/data
exec weed server -dir=/tmp/data -master.volumeSizeLimitMB=64 -volume.max=30 -filer -s3 -s3.port=8333 -s3.config=/tmp/s3.json' >/dev/null

host_port() { docker port "$1" "$2" | head -n 1 | sed 's/.*://'; }
pg_port="$(host_port "$pg_name" 5432/tcp)"
s3_port="$(host_port "$s3_name" 8333/tcp)"

deadline=$((SECONDS + 90))
until docker exec "$pg_name" pg_isready -h 127.0.0.1 -U postgres -d linea >/dev/null 2>&1; do
  [ "$SECONDS" -lt "$deadline" ] || { echo 'PostgreSQL did not become ready' >&2; exit 1; }
  sleep 0.5
done

# Create the bucket with a signed request; this also waits for the gateway.
until [ "$(curl -s -o /dev/null -w '%{http_code}' --aws-sigv4 'aws:amz:us-east-1:s3' --user "$s3_access:$s3_secret" \
  -X PUT "http://127.0.0.1:$s3_port/$bucket" || true)" = 200 ]; do
  [ "$SECONDS" -lt "$deadline" ] || { echo 'SeaweedFS S3 gateway did not become ready' >&2; docker logs "$s3_name" 2>&1 | tail -20 >&2; exit 1; }
  sleep 1
done

export LINEA_TEST_DATABASE_URL="postgres://postgres:$pg_password@127.0.0.1:$pg_port/linea?sslmode=disable"
export LINEA_TEST_S3_ENDPOINT="127.0.0.1:$s3_port"
export LINEA_TEST_S3_ACCESS_KEY="$s3_access"
export LINEA_TEST_S3_SECRET_KEY="$s3_secret"
export LINEA_TEST_S3_BUCKET="$bucket"

cd "$root"
GOMAXPROCS=2 go test -p 1 -count 1 "$@" ./internal/debugger/store/pg/...
