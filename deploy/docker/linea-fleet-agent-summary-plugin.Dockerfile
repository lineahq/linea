# Build from the repository root:
#   docker build -f deploy/docker/linea-fleet-agent-summary-plugin.Dockerfile -t agent-summary-plugin:dev .
FROM golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9 AS build
WORKDIR /src
ENV GOMAXPROCS=2 CGO_ENABLED=0 GOWORK=off
COPY pluginsdk ./pluginsdk
COPY examples/fleet-agent-summary-plugin ./examples/fleet-agent-summary-plugin
WORKDIR /src/examples/fleet-agent-summary-plugin
RUN --mount=type=cache,target=/root/.cache/go-build go build -mod=readonly -p 1 -trimpath -o /out/agent-summary .

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build --chown=65532:65532 /out/agent-summary /agent-summary
USER 65532:65532
ENTRYPOINT ["/agent-summary"]
