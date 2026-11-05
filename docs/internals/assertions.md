# Assertions and the test command

Verified against Collector `v0.162.0` (2026-10-09). Code:
`internal/debugger/scenario/assertion.go`, `internal/debugger/assertion/`,
`internal/debugger/engine/test.go`, `cmd/linea/test.go`. Example:
`testdata/multisignal/tests/scrubbing.yaml`.

## Schema (scenario version 1, `assertions`)

```yaml
assertions:
  - name: log credentials are scrubbed      # optional; defaults to assertions[i]
    target:
      exporter: otlp/backend                # or processor: filter/health
      pipeline: logs                        # processor only; needed when it runs in
                                            # several pipelines of the signal
      signal: logs                          # required
    count: {equals: 1}                      # or min / max (inclusive)
    dropped: {equals: 1}                    # processor only: received - emitted
    attributes:                             # record attributes, every record
      present: [enduser.id]
      absent: [user.password]
      equals: {http.route: /login}          # scalars; the type must match too
      types: {enduser.id: string}           # string bool int double bytes slice map
    resource_attributes:                    # same shape, every resource with records
      equals: {deployment.environment: test}
```

Load-time validation is structural and strict (unknown keys, empty bounds,
`equals` combined with `min`/`max`, a key both present and absent, non-scalar
`equals` values). Whether the target exists is decided at evaluation time
against the resolved topology, since it depends on the configuration.

Quick runs may supply this list separately through `QuickInput.Assertions`
(`engine.Request.AssertionsYAML`), verified against Collector `v0.162.0`
(2026-10-12). `scenario.ParseAssertions` accepts only `assertions` and an
optional version (omitted, 0 or 1), requires at least one assertion and applies
the same per-assertion validation as scenarios. The exact text is captured in
bundle schema 3 and hashed in `provenance.assertions`; evaluation uses the
existing `assertion.Evaluate` path. Empty source means no assertions and no
outcome. Quick setup continues to reject assertions. The HTTP v1 run request
(`assertions_yaml`), MCP and statement tests accept the separate source; the
web client and the CLI `run` command do not send it.

## Targets

- **Exporter**: the recorded `runresult.Output` of an exporter, i.e. all
  pipelines of the signal that export to it (fan-in is summed). Captured and
  stand-in exporters are recorded instead of sending; `debug` and `nop` run
  for real and what reaches them is recorded first.
- **Endpoint** (`target: {endpoint: {component: extension/livecapture, path:
  delivery}, signal: logs}`): what a simulated client endpoint
 of a component that runs for real received, from the report's
  `endpoint_outputs`, with the same checks as an exporter (`dropped` and
  `pipeline` are processor-only). `component` is a topology ref
  (`<kind>/<name>`) and `path` where the client sits in its configuration,
  as `simulated_endpoints` names it. A component the configuration lacks, or
  an endpoint the run did not simulate, is an `error` naming the endpoints
  it did simulate. Stand-in values in the component's own settings, or
  stand-ins upstream of a processor or connector, make passes and failures
  `inconclusive`, as for the other targets. What a component sends to its
  endpoints follows no pipeline, so a failed count has no per-stage trail.
- **Processor**: the `stage.output` snapshots of one processor *instance*
  (component, pipeline, signal). A processor shared by pipelines runs once
  per pipeline, so the pipeline must be named when ambiguous.
- `dropped` is `stage.input` items minus `stage.output` items. Stage errors
  (rejected input) count as dropped; the diagnostic mentions them.

Locations in diagnostics are the record position in the exporter output, or
`output seq N` plus the position in that snapshot.

## Outcomes

| Outcome | When |
| --- | --- |
| `pass` | condition holds and the run `completed` |
| `fail` | condition violated by what was observed |
| `inconclusive` | the run did not complete and nothing observed proves a violation, or no records reached the target (attribute conditions would hold vacuously) |
| `error` | the target is not in the configuration, not recorded (an endpoint the run did not simulate) or ambiguous; the scenario failed to load or run; or it declares no assertions |

A partial run never passes, but an observed violation is still a
failure: an attribute that leaked, or a count already above its maximum (more
data can only raise counts). A count below its minimum on a partial run is
inconclusive. `types` and `equals` also fail when the key is absent.

Severity order: error > fail > inconclusive > pass. An assertion is the worst
of its checks, a scenario the worst of its assertions, a test run the worst of
its scenarios.

**Stand-ins.** When the data reaching the target passed a stand-in
processor or connector (topology mode `stand_in`), or a component whose
settings use a reference's stand-in string (`stand_in_values`), every check
that would pass or fail is `inconclusive`. The message names those
components and keeps the observed value: what a stand-in did is not what the
real component would have done. `topology.Graph.Upstream` walks the target's
pipeline and, through connectors, the pipelines that feed it. Stand-in
receivers and exporters change no data and do not count. An exporter target
in mode `stand_in` is captured like any other.

Failing count checks show records per stage for each pipeline feeding the
target (`logs: in 2 -> resource 2 -> ... -> filter/health 1 -> ...`), which
points at the stage that changed the count.

## Test command and reports

`linea test --config c.yaml (--scenario s.yaml ... | --tests dir)
[--report junit.xml|report.json] [--format text|json]`

- `--tests` runs `*.yaml` / `*.yml` in the directory (not recursive), in
  name order. Each scenario is an independent run.
- Exit codes: 0 pass, 1 error, 3 fail, 4 inconclusive (2 stays reserved for a
  reference mismatch under `run --compare`).
- `engine.Test` returns `engine.TestReport` (`test_schema` 0.1), encoded
  with `engine.Encode` for JSON. `run` and the API include `assertions` in
  the run report when the scenario declares them, without changing the exit
  code of `run`.
- JUnit: one `testsuite` per scenario (properties: config, scenario,
  outcome, run_id, run_status), one `testcase` per assertion. A scenario that
  could not run is one `setup` case with `<error>`. Inconclusive is a
  `<failure type="inconclusive">`, never `<skipped>`, because CI systems
  treat skipped tests as passing.
