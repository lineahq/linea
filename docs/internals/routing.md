# Routing connector

Verified against contrib `connector/routingconnector` `v0.162.0` (2026-10-11).
Code: `internal/debugger/routing/`, `internal/debugger/analysis/routing.go`.

## What the connector does

- The table is evaluated rule by rule, in order, on what is left of the
  payload. A rule's `condition` is wrapped as `route() where <condition>`.
- The OTTL **context** decides the unit of routing: `resource` (all records
  of a resource move together), `log`, `span`, `metric`, `datapoint`, or
  `otelcol`/`request` (client metadata). With no `context` the connector
  infers it from the paths of the condition and falls back to `resource`.
- `action: move` (the default) takes the matching items out of the payload,
  so later rules never see them. `action: copy` sends a copy and the original
  continues to the later rules and the default.
- Whatever no rule moved goes to `default_pipelines`. Two rules with the same
  context and statement are a duplicate: the second is ignored with a
  warning, so it never routes anything.
- `error_mode: ignore` treats a failing condition as no match; `propagate`
  returns the error and nothing is delivered.
- Items are not modified, only regrouped, so records keep their content.

## Replay and verification

`routing.Explain` runs the **real** connector (`routingconnector.NewFactory`)
on a clone of the recorded input, with a table rewritten so that rule *i*
delivers to a pipeline `rule<i>` and the default pipelines to `default`.
`connector.NewLogsRouter` (and the traces and metrics routers) give it one
`consumertest` sink per name. Matching, move and copy semantics are the
connector's own, so nothing about them is re-implemented.

- **Attribution.** Each sink's records are assigned to the input records with
  the same content, resource and scope (`telemetry.ContextFingerprints`).
  Routing decides by content, so identical records are interchangeable.
  A record's rules are the sinks that received it; `default` is true when the
  default sink did.
- **Verification.** Per configured pipeline, the replay must deliver the same
  multiset of records as the run recorded (`stage` routes, one payload per
  delivery): `replay_verified`. Otherwise `replay_differs` and no record is
  attributed. A rule that reads client metadata (`context: request` or
  `otelcol`, or `otelcol.` in the text) makes the input `unsupported`: the
  run does not record the request. An error the run returned is
  `replay_verified` only when the replay also errors.
- **Not recorded.** The value a condition read (for example the severity of
  one record) is not reported; the report names the rule and its text.
- Report schema 0.5 added `routing` (`analysis.Routing`: `rules`,
  `default_pipelines`, `records[ref, rules, default]`, `verification`,
  `notes`).
