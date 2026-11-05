# The official OpAMP supervisor with the official contrib Collector binary,
# both unmodified (deploy/compose/fleet/compose.supervised.yaml, scripts/fleet/smoke-supervisor.sh).
FROM otel/opentelemetry-collector-contrib:0.163.2@sha256:7f22d2a6748110ffb6a59bc0df448bccbf501cdb667d610ede4c61d3863d5849 AS collector
FROM otel/opentelemetry-collector-opampsupervisor:0.163.2@sha256:a227772b9848a04a0ce3e488b76949dc8d993cbd7af5ab008a99a7411628fb8f
COPY --from=collector /otelcol-contrib /otelcol-contrib
