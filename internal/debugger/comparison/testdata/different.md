# Run comparison: different

| | Before | After |
|---|---|---|
| Run | `before-run` | `after-run` |
| Mode | — | — |
| End | `done` | `done` |
| Execution | `completed` | `completed` |
| Complete | yes | yes |
| Assertions | `pass` | `fail` |

Inputs identical: yes.

## Exporters

| Exporter | Signal | Before | After | Result |
|---|---|---|---|---|
| `debug` | `logs` | 3 | 4 | different records |
| `otlp/backend` | `logs` | 3 | 4 | different records |

### Field changes at `debug` (`logs`)

| Path | Op | Records | Before | After |
|---|---|---|---|---|
| `log.severity_text` | modified | 3 | `WARNING` | `warning` |

### Field changes at `otlp/backend` (`logs`)

| Path | Op | Records | Before | After |
|---|---|---|---|---|
| `log.severity_text` | modified | 3 | `WARNING` | `warning` |

## Injected records

4 aligned, 4 changed, 0 unaligned, 0 uncertain.

| Input | Record | Before | After | Diverges at | Field changes |
|---|---|---|---|---|---|
| 1 | `resource[0].scope[0].log_record[0]` | delivered to `debug (logs)`<br>delivered to `otlp/backend (logs)` | delivered to `debug (logs)`<br>delivered to `otlp/backend (logs)` | `filter/health (logs)` | 2 |
| 1 | `resource[0].scope[0].log_record[1]` | delivered to `debug (logs)`<br>delivered to `otlp/backend (logs)` | delivered to `debug (logs)`<br>delivered to `otlp/backend (logs)` | `filter/health (logs)` | 2 |
| 1 | `resource[0].scope[0].log_record[2]` | `dropped at filter/health (logs)` | delivered to `debug (logs)`<br>delivered to `otlp/backend (logs)` | `filter/health (logs)` | 0 |
| 1 | `resource[0].scope[0].log_record[3]` | delivered to `debug (logs)`<br>delivered to `otlp/backend (logs)` | delivered to `debug (logs)`<br>delivered to `otlp/backend (logs)` | `filter/health (logs)` | 2 |

## Assertions

| Assertion | Before | After |
|---|---|---|
| `the health check is dropped` | `pass` | `fail` |
| `no password leaves` | `pass` | `fail` |

## Recommendations

1 resolved, 2 new, 0 persist.

| Status | Rule | Pipeline | Components | Subject | Benefit before | Benefit after |
|---|---|---|---|---|---|---|
| resolved | `filter.move_earlier` | `logs` | `processor/filter/health, processor/resource, processor/attributes/scrub` | — | 2 `records_not_processed` | — |
| new | `config.unused_component` | — | `processor/filter/health` | — | — | — |
| new | `ottl.statement_no_change` | `logs` | `processor/transform/severity` | `set(log.severity_text, ConvertCase(log.severity_text, "lower"))` | — | 4 `evaluations_without_effect` |

## Ignored

| Rule | Differences |
|---|---|
| `log.attributes["absent"]` | 0 |
