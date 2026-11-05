# The team server image: linea-server with the web client embedded. It links
# no Collector: Collectors run in runtime workers (the images
# `linea runtime build` makes), never in this image, so it holds no worker
# source and cannot build runtimes.
#
#   docker build -f deploy/docker/linea-server.Dockerfile -t linea-server:dev .
#
# GO_BUILD_PARALLELISM bounds the Go build's parallel packages (go build -p);
# lower it on a constrained host, for example --build-arg
# GO_BUILD_PARALLELISM=1. GOMAXPROCS bounds the threads of each.

FROM --platform=$BUILDPLATFORM node:24.12.0-bookworm-slim@sha256:7326fb2dbdce998edd72140946851be64ef4a643e8715e138ca467e8e9d92c99 AS web
WORKDIR /src/web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml ./
# The lockfile pins every package; never resolve new versions here.
RUN pnpm install --frozen-lockfile
COPY internal/debugger/api/openapi.json /src/internal/debugger/api/openapi.json
COPY web/ ./
# Type-checks, then writes /src/internal/debugger/webui/static/app.
RUN pnpm gen:api && pnpm build

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
COPY --from=web /src/internal/debugger/webui/static/app ./internal/debugger/webui/static/app
ARG GO_BUILD_PARALLELISM=2
ARG GOMAXPROCS=
# The release version without its "v"; empty for a development build.
ARG VERSION=
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOMAXPROCS=${GOMAXPROCS} go build -p ${GO_BUILD_PARALLELISM} -trimpath -ldflags="-s -w -X github.com/lineahq/linea/internal/debugger/release.version=${VERSION}" -o /out/linea-server ./cmd/linea-server

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/linea-server /linea-server
USER 65532:65532
EXPOSE 7070
ENTRYPOINT ["/linea-server"]
CMD ["--config", "/etc/linea/config.yaml", "--addr", "0.0.0.0:7070", "--allow-http"]
