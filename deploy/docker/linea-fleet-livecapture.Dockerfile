# Build stages run on the build host and cross-compile for the target platform.
FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9 AS source
WORKDIR /src
ENV GOMAXPROCS=2 CGO_ENABLED=0 GOWORK=off
COPY go.mod go.sum ./
COPY pluginsdk ./pluginsdk
COPY collector/provider/go.mod ./collector/provider/
COPY collector/livecapture ./collector/livecapture
COPY internal/fleetplugins ./internal/fleetplugins
COPY cmd/linea-fleet-livecapture ./cmd/linea-fleet-livecapture
COPY cmd/linea-fleet-livecapture-client ./cmd/linea-fleet-livecapture-client
COPY cmd/linea-fleet-livecapture-fixture ./cmd/linea-fleet-livecapture-fixture
COPY LICENSE ./
COPY tools/notices ./tools/notices
RUN --mount=type=cache,target=/go/pkg/mod go mod download
ARG VERSION=0.0.0-dev
ARG TARGETOS TARGETARCH
ENV GOOS=${TARGETOS} GOARCH=${TARGETARCH}
FROM source AS server-build
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build go build -mod=readonly -p 1 -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/plugin ./cmd/linea-fleet-livecapture
# The third-party notices of what the plugin links, for the target platform
# (GOOS and GOARCH above). The tool itself is built for this host.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build GOOS= GOARCH= go build -mod=readonly -o /tmp/notices ./tools/notices \
    && mkdir -p /out/licenses && cp LICENSE /out/licenses/LICENSE \
    && /tmp/notices -title linea-fleet-livecapture -o /out/licenses/THIRD_PARTY_NOTICES.txt ./cmd/linea-fleet-livecapture
FROM source AS client-build
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build go build -mod=readonly -p 1 -trimpath -o /out/client ./cmd/linea-fleet-livecapture-client
FROM source AS fixture-build
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build go build -mod=readonly -p 1 -trimpath -o /out/fixture ./cmd/linea-fleet-livecapture-fixture

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS client
COPY --from=client-build --chown=65532:65532 /out/client /client
USER 65532:65532
ENTRYPOINT ["/client"]
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS fixture
COPY --from=fixture-build --chown=65532:65532 /out/fixture /fixture
USER 65532:65532
ENTRYPOINT ["/fixture"]
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS plugin
COPY --from=server-build --chown=65532:65532 /out/plugin /plugin
COPY --from=server-build /out/licenses /licenses
USER 65532:65532
ENTRYPOINT ["/plugin"]
CMD ["serve"]
