# syntax=docker/dockerfile:1
FROM golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9 AS toolchain
ARG OCB_VERSION=v0.162.0
WORKDIR /src
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go install go.opentelemetry.io/collector/cmd/builder@${OCB_VERSION}
ENV GOWORK=off
COPY collector/distribution/go.mod collector/distribution/go.sum ./collector/distribution/
COPY collector/livecapture/go.mod collector/livecapture/go.sum ./collector/livecapture/
COPY collector/provider/go.mod ./collector/provider/
COPY collector/tools/testlab/go.mod collector/tools/testlab/go.sum ./collector/tools/testlab/
RUN --mount=type=cache,target=/go/pkg/mod \
    cd collector/livecapture && go mod download && cd ../distribution && go mod download && \
    cd ../tools/testlab && go mod download

FROM toolchain AS build
COPY collector/distribution/ ./expected/
COPY collector/ ./collector/
COPY deploy/compose/collector/config/ ./deploy/compose/collector/config/
COPY scripts/collector/container-test.sh ./scripts/collector/container-test.sh
# Regenerate from OCB and check the tracked source, which is gofmt-formatted
# (scripts/collector/dev.sh Build formats it). go.mod and go.sum are
# excluded: go work sync keeps them at the workspace-wide dependency versions,
# which the builder does not reproduce.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd collector && builder --config distribution/builder-config.yaml --skip-compilation && gofmt -w distribution && diff -ru -x go.mod -x go.sum ../expected distribution
WORKDIR /src/collector/distribution
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -mod=readonly -trimpath -o /out/otelcol-livecapture . && \
    CGO_ENABLED=0 go build -mod=readonly -trimpath -o /out/smoke ../tools/smoke/main.go && \
    CGO_ENABLED=0 go build -mod=readonly -trimpath -o /out/capture-smoke ../tools/capture-smoke/main.go && \
    cd ../tools/testlab && CGO_ENABLED=0 go build -mod=readonly -trimpath -o /out/testlab .

FROM build AS test
# Materialize dependencies in this image: checks run every time, without network.
RUN --mount=type=cache,target=/go/pkg/mod \
    cp -a /go/pkg/mod /opt/go-mod
ENV GOMODCACHE=/opt/go-mod GOPROXY=off CGO_ENABLED=1
ENTRYPOINT ["/bin/sh", "/src/scripts/collector/container-test.sh"]

FROM scratch AS runtime-base
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/otelcol-livecapture /otelcol-livecapture
COPY deploy/compose/collector/config/collector.local.yaml /etc/otelcol/config.yaml
ENV OTLP_GRPC_ENDPOINT=0.0.0.0:4317 OTLP_HTTP_ENDPOINT=0.0.0.0:4318
USER 10001:10001
EXPOSE 4317 4318
ENTRYPOINT ["/otelcol-livecapture"]
CMD ["--config", "/etc/otelcol/config.yaml"]

# Exercise the exact runtime filesystem and user, without publishing host ports.
FROM runtime-base AS smoke
COPY --from=build /out/smoke /smoke
ENTRYPOINT ["/smoke"]
CMD ["/otelcol-livecapture", "/etc/otelcol/config.yaml"]

FROM runtime-base AS capture-smoke
COPY --from=build /out/capture-smoke /capture-smoke
COPY deploy/compose/collector/config/collector.capture-test.yaml /etc/otelcol/capture-test.yaml
ENTRYPOINT ["/capture-smoke"]
CMD ["/otelcol-livecapture", "/etc/otelcol/capture-test.yaml"]

# Development-only generator, capture receiver, and embedded web page.
FROM scratch AS lab
COPY --from=build /out/testlab /testlab
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/testlab"]

# Isolated development load/failure experiments with a Collector subprocess.
FROM runtime-base AS perf
COPY --from=build /out/testlab /testlab
ENTRYPOINT ["/testlab", "-perf"]

# A plain `docker build .` produces the Collector runtime.
FROM runtime-base AS runtime
