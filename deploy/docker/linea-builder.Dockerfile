# The runtime builder: linea-builder with the worker source
# embedded and the Go toolchain beside it. It dials a team server, compiles
# the runtimes it is given natively (no Docker daemon) and uploads their
# binaries. It is the only debugger component that needs outbound network
# access, to the module proxy. The Compose stack (deploy/compose/linea) runs
# it.
#
#   docker build -f deploy/docker/linea-builder.Dockerfile -t linea-builder:dev .
#
# The toolchain is /usr/local/go of the exact Go image the CLI builds
# runtimes in (container.GoImage, a test checks it): only that toolchain
# compiles the binary a CLI build produces, and linea-builder refuses to
# start with another. Runtimes build with CGO_ENABLED=0, so the final stage
# needs no C toolchain and the Go image's Debian tools stay out of it: it is
# Alpine with git, which fetches private modules (GOPRIVATE) directly.
#
# GO_BUILD_PARALLELISM bounds the Go build's parallel packages (go build -p).

# Build stages run on the build host and cross-compile for the target platform.
FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
ENV GOWORK=off
COPY go.mod go.sum ./
COPY collector/provider/go.mod ./collector/provider/
COPY collector/livecapture/go.mod collector/livecapture/go.sum ./collector/livecapture/
COPY pluginsdk/go.mod ./pluginsdk/
RUN go mod download
COPY . .
# The archive of the worker source a build compiles (not committed).
RUN go generate ./internal/debugger/workersrc
ARG GO_BUILD_PARALLELISM=2
ARG GOMAXPROCS=
# The release version without its "v"; empty for a development build.
ARG VERSION=
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOMAXPROCS=${GOMAXPROCS} go build -p ${GO_BUILD_PARALLELISM} -trimpath -ldflags="-s -w -X github.com/lineahq/linea/internal/debugger/release.version=${VERSION}" -o /out/linea-builder ./cmd/linea-builder

# The toolchain, for the target platform.
FROM golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9 AS toolchain

FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
# git for private modules, an unprivileged user and a work directory it
# owns. A volume mounted on /work (the Compose stack mounts one) must allow
# executing files: go run starts the gate generator from it, and a build
# starts the binary it built.
RUN apk add --no-cache git \
    && addgroup -g 65532 builder \
    && adduser -u 65532 -G builder -h /work -H -s /sbin/nologin -D builder \
    && mkdir /work && chown 65532:65532 /work
COPY --from=toolchain /usr/local/go /usr/local/go
ENV GOTOOLCHAIN=local PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
COPY --from=build /out/linea-builder /usr/local/bin/linea-builder
ENV HOME=/work
WORKDIR /work
USER 65532:65532
ENTRYPOINT ["linea-builder"]
CMD ["--work-dir", "/work"]
