# The default runtime's worker: linea-worker built from this module, so it
# runs the built-in Collector release and components
# (build/linea-runtime.yaml). It is the image `linea runtime build` makes for
# that manifest, built here without Docker-in-Docker, and the worker of the
# Compose stack (deploy/compose/linea).
#
#   docker build -f deploy/docker/linea-worker.Dockerfile -t linea-worker:dev .
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
ARG GO_BUILD_PARALLELISM=2
ARG GOMAXPROCS=
# The release version without its "v"; empty for a development build.
ARG VERSION=
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOMAXPROCS=${GOMAXPROCS} go build -p ${GO_BUILD_PARALLELISM} -trimpath -ldflags="-s -w -X github.com/lineahq/linea/internal/debugger/release.version=${VERSION}" -o /out/worker ./cmd/linea-worker
# The pool worker's binary cache (linea-worker --pool --cache-dir /cache): a
# volume mounted there starts with this directory's owner.
RUN mkdir /out/cache

# Distroless rather than scratch: a worker that dials an https server needs
# the system roots.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/worker /worker
COPY --from=build --chown=65534:65534 /out/cache /cache
# The label marks a runtime image, so `linea runtime register` accepts it;
# the worker's hello describes the build.
LABEL io.linea.runtime=default
USER 65534:65534
ENTRYPOINT ["/worker"]
