# Build stages run on the build host and cross-compile for the target platform.
FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9 AS source
WORKDIR /src
ENV GOMAXPROCS=2 CGO_ENABLED=0 GOWORK=off
COPY go.mod go.sum ./
COPY collector/provider/go.mod ./collector/provider/
COPY collector/livecapture/go.mod collector/livecapture/go.sum ./collector/livecapture/
COPY pluginsdk/go.mod ./pluginsdk/
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY pluginsdk ./pluginsdk
COPY LICENSE ./
COPY tools/notices ./tools/notices
ARG VERSION=0.0.0-dev
ARG TARGETOS TARGETARCH
ENV GOOS=${TARGETOS} GOARCH=${TARGETARCH}
FROM source AS core-build
RUN go build -p 1 -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/linea-fleet ./cmd/linea-fleet
# The third-party notices of what the binary links, for the target platform
# (GOOS and GOARCH above). The tool itself is built for this host.
RUN GOOS= GOARCH= go build -o /tmp/notices ./tools/notices \
    && mkdir -p /out/licenses && cp LICENSE /out/licenses/LICENSE \
    && /tmp/notices -title linea-fleet -o /out/licenses/THIRD_PARTY_NOTICES.txt ./cmd/linea-fleet
FROM source AS simulator-build
RUN go build -p 1 -trimpath -ldflags="-s -w" -o /out/agent-simulator ./cmd/linea-fleet-simulator

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS agent-simulator
COPY --from=simulator-build --chown=65532:65532 /out/agent-simulator /agent-simulator
USER 65532:65532
ENTRYPOINT ["/agent-simulator"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS fleet
COPY --from=core-build --chown=65532:65532 /out/linea-fleet /linea-fleet
COPY --from=core-build /out/licenses /licenses
USER 65532:65532
EXPOSE 8080 4320
ENTRYPOINT ["/linea-fleet"]
