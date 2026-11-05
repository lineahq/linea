#!/usr/bin/env bash
# Regenerates collector/livecapture/controlpb from api/livecapture/control/v1/control.proto.
# Runs protoc and the Go plugins in a pinned container; needs only Docker.
set -euo pipefail

PROTOC_VERSION=31.1
PROTOC_GEN_GO_VERSION=v1.36.12
PROTOC_GEN_GO_GRPC_VERSION=v1.5.1
GO_IMAGE='golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9'

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

export MSYS_NO_PATHCONV=1
docker run --rm \
  --mount "type=bind,source=$root,target=/src" \
  --workdir /src \
  --env "PROTOC_VERSION=$PROTOC_VERSION" \
  --env "PROTOC_GEN_GO_VERSION=$PROTOC_GEN_GO_VERSION" \
  --env "PROTOC_GEN_GO_GRPC_VERSION=$PROTOC_GEN_GO_GRPC_VERSION" \
  "$GO_IMAGE" bash -euc '
    out=collector/livecapture/controlpb
    proto=livecapture/control/v1/control.proto
    apt-get update -qq >/dev/null
    apt-get install -y -qq --no-install-recommends unzip >/dev/null
    tmp=$(mktemp -d)
    curl -fsSL -o "$tmp/protoc.zip" \
      "https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-linux-x86_64.zip"
    unzip -q "$tmp/protoc.zip" -d "$tmp/protoc"
    export GOBIN="$tmp/bin"
    (cd "$tmp" && go install "google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION}")
    (cd "$tmp" && go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION}")
    mkdir "$tmp/gen"
    PATH="$tmp/bin:$PATH" "$tmp/protoc/bin/protoc" \
      -I api -I "$tmp/protoc/include" \
      --go_out="$tmp/gen" --go_opt=paths=source_relative \
      --go-grpc_out="$tmp/gen" --go-grpc_opt=paths=source_relative \
      "api/$proto"
    cp "$tmp/gen/${proto%.proto}.pb.go" "$tmp/gen/${proto%.proto}_grpc.pb.go" "$out/"
  '
