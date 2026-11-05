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

## Ignored

| Rule | Differences |
|---|---|
| `log.attributes["absent"]` | 0 |
