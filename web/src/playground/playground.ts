import type { OTTLEvaluation, ReportRuntime } from '../api/types';
import { reportRuntime } from '../runtimes/runtimes';

/**
 * The OTTL playground evaluates statements on a pasted OTLP JSON payload
 * through `POST /ottl/evaluate`, without a run, a scenario or the open
 * configuration. The page writes the smallest Collector configuration that
 * holds one transform processor with the statements.
 */
export const PROCESSOR = 'transform/playground';

export type Signal = 'logs' | 'traces' | 'metrics';

const SIGNALS: { key: string; signal: Signal; statements: string }[] = [
  { key: 'resourceLogs', signal: 'logs', statements: 'log_statements' },
  { key: 'resourceSpans', signal: 'traces', statements: 'trace_statements' },
  { key: 'resourceMetrics', signal: 'metrics', statements: 'metric_statements' },
];

export type ErrorMode = 'propagate' | 'ignore' | 'silent';
export const ERROR_MODES: ErrorMode[] = ['propagate', 'ignore', 'silent'];

export type InputCheck = { ok: true; signal: Signal; value: unknown } | { ok: false; message: string };

/** checkInput parses the payload and names its signal from its top-level key. */
export function checkInput(text: string): InputCheck {
  if (!text.trim()) return { ok: false, message: 'Paste an OTLP JSON payload, or pick an example.' };
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch (e) {
    return { ok: false, message: `The input is not JSON: ${e instanceof Error ? e.message : String(e)}` };
  }
  const found = value && typeof value === 'object' && !Array.isArray(value) ? SIGNALS.filter((s) => s.key in value) : [];
  if (found.length !== 1) {
    return { ok: false, message: 'The input must be OTLP JSON with one of resourceLogs, resourceSpans or resourceMetrics.' };
  }
  return { ok: true, signal: found[0].signal, value };
}

/**
 * statementLines lists the statements, one per line; blank lines and lines
 * starting with # are skipped. Each keeps its 1-based line in the editor.
 */
export function statementLines(text: string): { line: number; text: string }[] {
  return text
    .split(/\r?\n/)
    .map((t, i) => ({ line: i + 1, text: t.trim() }))
    .filter((s) => s.text && !s.text.startsWith('#'));
}

/**
 * processorYAML is the transform processor block, ready to paste under
 * `processors:`. Statements use the flat form, whose context the processor
 * infers from their paths (log., span., resource., ...). A JSON string is a
 * valid YAML double-quoted scalar, so any statement text round-trips.
 */
export function processorYAML(signal: Signal, statements: string[], errorMode: ErrorMode): string {
  const key = SIGNALS.find((s) => s.signal === signal)!.statements;
  return [
    `${PROCESSOR}:`,
    `  error_mode: ${errorMode}`,
    `  ${key}:`,
    ...statements.map((s) => `    - ${JSON.stringify(s)}`),
    '',
  ].join('\n');
}

/** playgroundConfig wraps the processor in one pipeline of the input's signal. */
export function playgroundConfig(signal: Signal, statements: string[], errorMode: ErrorMode): string {
  const processor = processorYAML(signal, statements, errorMode)
    .split('\n')
    .map((l) => (l ? `  ${l}` : l))
    .join('\n');
  return `receivers:
  otlp:
    protocols:
      http: {}
processors:
${processor}exporters:
  debug: {}
service:
  pipelines:
    ${signal}:
      receivers: [otlp]
      processors: [${PROCESSOR}]
      exporters: [debug]
`;
}

export interface StatementResult {
  /** The key the record view names the statement by in its By column. */
  key: string;
  line: number;
  text: string;
  matched: number;
  notMatched: number;
  unchanged: number;
  changes: number;
  errors: { entity: string; message: string }[];
}

/**
 * statementResults pairs each written statement with its replay counts. The
 * flat form is one group, so statement i of group 0 is the i-th line, which
 * the record view keys as S<i>.
 */
export function statementResults(lines: { line: number; text: string }[], ev: OTTLEvaluation | undefined): StatementResult[] {
  const group = ev?.replay?.groups?.find((g) => g.index === 0);
  return lines.flatMap((l, i) => {
    const st = group?.statements?.find((s) => s.index === i);
    if (!st) return [];
    return [{
      key: `S${i}`,
      line: l.line,
      text: l.text,
      matched: st.matched,
      notMatched: st.not_matched,
      unchanged: st.matched_without_change ?? 0,
      changes: st.changes?.length ?? 0,
      errors: (st.errors ?? []).map((e) => ({ entity: e.entity, message: e.message })),
    }];
  });
}

const RECORD_NOUN: Record<Signal, [string, string]> = { logs: ['log record', 'log records'], traces: ['span', 'spans'], metrics: ['data point', 'data points'] };

/**
 * recordSummary says in one line what the statements did to the input
 * records, from the record view's states: "2 log records: 1 changed,
 * 1 unchanged". Only states that occur are named.
 */
export function recordSummary(signal: Signal, states: string[]): string {
  const count = (s: string) => states.filter((x) => x === s).length;
  const input = states.length - count('new');
  const [one, many] = RECORD_NOUN[signal];
  const parts = ([['edited', 'changed'], ['dropped', 'dropped'], ['new', 'new'], ['unchanged', 'unchanged']] as const)
    .map(([state, word]) => [count(state), word] as const)
    .filter(([n]) => n > 0)
    .map(([n, word]) => `${n} ${word}`);
  return `${input} ${input === 1 ? one : many}${parts.length ? `: ${parts.join(', ')}` : ''}`;
}

const attr = (key: string, value: string) => ({ key, value: { stringValue: value } });
const resource = (service: string) => ({ attributes: [attr('service.name', service), attr('deployment.environment', 'staging')] });

/** EXAMPLES are small payloads, with statements, for each signal. */
export const EXAMPLES: { signal: Signal; label: string; input: unknown; statements: string }[] = [
  {
    signal: 'logs',
    label: 'Logs',
    input: {
      resourceLogs: [{
        resource: resource('checkout'),
        scopeLogs: [{
          scope: { name: 'checkout' },
          logRecords: [
            { timeUnixNano: '1700000000000000000', severityText: 'ERROR', body: { stringValue: 'payment failed for user=alice' }, attributes: [attr('http.route', '/pay')] },
            { timeUnixNano: '1700000001000000000', severityText: 'INFO', body: { stringValue: 'order placed' }, attributes: [attr('http.route', '/order')] },
          ],
        }],
      }],
    },
    statements: [
      '# One statement per line; the context is inferred from the paths.',
      'set(log.attributes["alert"], true) where log.severity_text == "ERROR"',
      'replace_pattern(log.body, "user=\\\\w+", "user=***")',
      'set(resource.attributes["team"], "payments")',
    ].join('\n'),
  },
  {
    signal: 'traces',
    label: 'Traces',
    input: {
      resourceSpans: [{
        resource: resource('frontend'),
        scopeSpans: [{
          scope: { name: 'frontend' },
          spans: [
            { traceId: '5b8efff798038103d269b633813fc60c', spanId: 'eee19b7ec3c1b174', name: 'GET /api/cart', kind: 2, startTimeUnixNano: '1700000000000000000', endTimeUnixNano: '1700000000250000000', attributes: [attr('http.request.method', 'GET'), attr('url.full', 'https://shop.example/api/cart?token=abc')], status: {} },
            { traceId: '5b8efff798038103d269b633813fc60c', spanId: 'eee19b7ec3c1b175', parentSpanId: 'eee19b7ec3c1b174', name: 'SELECT cart', kind: 3, startTimeUnixNano: '1700000000010000000', endTimeUnixNano: '1700000000200000000', attributes: [attr('db.system', 'postgresql')], status: { code: 2, message: 'timeout' } },
          ],
        }],
      }],
    },
    statements: [
      'replace_pattern(span.attributes["url.full"], "token=[^&]+", "token=***")',
      'set(span.attributes["slow"], true) where span.end_time_unix_nano - span.start_time_unix_nano > 100000000',
      'set(span.name, Concat(["db", span.name], ": ")) where span.attributes["db.system"] != nil',
    ].join('\n'),
  },
  {
    signal: 'metrics',
    label: 'Metrics',
    input: {
      resourceMetrics: [{
        resource: resource('cart'),
        scopeMetrics: [{
          scope: { name: 'cart' },
          metrics: [
            { name: 'http.server.request.duration', unit: 's', sum: { aggregationTemporality: 2, isMonotonic: true, dataPoints: [{ asDouble: 12, timeUnixNano: '1700000000000000000', attributes: [attr('http.route', '/cart')] }] } },
            { name: 'process.memory.usage', unit: 'By', gauge: { dataPoints: [{ asInt: '52428800', timeUnixNano: '1700000000000000000' }] } },
          ],
        }],
      }],
    },
    statements: [
      'set(metric.description, "Requests handled") where metric.name == "http.server.request.duration"',
      'set(datapoint.attributes["host"], "cart-1")',
    ].join('\n'),
  },
];

/** exampleText is an example's input as the editor shows it. */
export function exampleText(e: (typeof EXAMPLES)[number]): string {
  return `${JSON.stringify(e.input, null, 2)}\n`;
}

/**
 * replacesWork says whether loading an example would replace text the user
 * wrote: anything that is neither empty nor one of the examples.
 */
export function replacesWork(input: string, statements: string): boolean {
  const known = (t: string, pick: (e: (typeof EXAMPLES)[number]) => string) => !t.trim() || EXAMPLES.some((e) => pick(e).trim() === t.trim());
  return !known(input, exampleText) || !known(statements, (e) => e.statements);
}

export interface EvaluatedOn {
  /** The result line: where the preview ran, and that it is not a test. */
  text: string;
  unverified: boolean;
}

/**
 * evaluatedOn words the runtime that answered a preview ("Built-in ·
 * Collector 0.162.0"), which may be the team's default and not the one
 * selected beside Run. A response without a runtime falls back to the
 * selected name, or to the default runtime when none is selected.
 */
export function evaluatedOn(runtime: ReportRuntime | undefined, selected: string | undefined): EvaluatedOn {
  const facts = reportRuntime(runtime);
  const where = facts?.label ?? (selected ? `runtime ${selected}` : 'the default runtime');
  return { text: `Evaluated on ${where}, without a run. A preview, never a test.`, unverified: !!facts?.unverified };
}
