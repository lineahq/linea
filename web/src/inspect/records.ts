// Records of an OTLP JSON payload, named the way the stage analysis names
// diff entities (internal/debugger/analysis/diff.go): spans by "span:<trace>:<span>",
// log records and metrics by their index path in the snapshot,
// "resource[i].scope[j].log_record[k]" or "resource[i].scope[j].metric[k]".

type Json = Record<string, unknown>;

export type Signal = 'traces' | 'logs' | 'metrics';

/** Field names of the OTLP JSON encoding per signal. */
const LAYOUT: Record<Signal, { resources: string; scopes: string; items: string; entity: string }> = {
  traces: { resources: 'resourceSpans', scopes: 'scopeSpans', items: 'spans', entity: 'span' },
  logs: { resources: 'resourceLogs', scopes: 'scopeLogs', items: 'logRecords', entity: 'log_record' },
  metrics: { resources: 'resourceMetrics', scopes: 'scopeMetrics', items: 'metrics', entity: 'metric' },
};

export interface RecordEntry {
  entity: string;
  label: string;
  resource: number;
  scope: number;
  index: number;
  value: Json;
}

const arr = (v: unknown): Json[] => (Array.isArray(v) ? (v as Json[]) : []);

/** signalOf detects the signal of an OTLP JSON payload. */
export function signalOf(payload: unknown): Signal | undefined {
  if (!payload || typeof payload !== 'object') return undefined;
  for (const s of Object.keys(LAYOUT) as Signal[]) if (LAYOUT[s].resources in (payload as Json)) return s;
  return undefined;
}

function anyValue(v: unknown): string {
  if (!v || typeof v !== 'object') return '';
  const [k, x] = Object.entries(v as Json)[0] ?? [];
  if (k === undefined) return '';
  return typeof x === 'object' ? JSON.stringify(x) : String(x);
}

/** listRecords lists every span, log record or metric with its entity name. */
export function listRecords(payload: unknown): RecordEntry[] {
  const signal = signalOf(payload);
  if (!signal) return [];
  const l = LAYOUT[signal];
  const out: RecordEntry[] = [];
  arr((payload as Json)[l.resources]).forEach((r, ri) =>
    arr(r[l.scopes]).forEach((s, si) =>
      arr(s[l.items]).forEach((item, ii) => {
        const entity =
          signal === 'traces'
            ? `span:${item.traceId ?? ''}:${item.spanId ?? ''}`
            : `resource[${ri}].scope[${si}].${l.entity}[${ii}]`;
        const label =
          signal === 'traces'
            ? String(item.name ?? '')
            : signal === 'logs'
              ? anyValue(item.body) || String(item.severityText ?? '')
              : String(item.name ?? '');
        out.push({ entity, label, resource: ri, scope: si, index: ii, value: item });
      }),
    ),
  );
  return out;
}

/**
 * counterpart finds the record in after that corresponds to a record of
 * before. Spans keep their IDs. Log records and metrics are named by their
 * position in before, so the after record is the one at the same position
 * once the removed records ahead of it in its scope are skipped; this holds
 * for processors that keep record order, which is what the analysis assumes
 * when it reports a positional match.
 */
export function counterpart(
  before: RecordEntry[],
  after: RecordEntry[],
  entity: string,
  removed: Set<string>,
): RecordEntry | undefined {
  if (entity.startsWith('span:')) return after.find((r) => r.entity === entity);
  const b = before.find((r) => r.entity === entity);
  if (!b || removed.has(entity)) return undefined;
  const sameScope = before.filter((r) => r.resource === b.resource && r.scope === b.scope);
  const skipped = sameScope.filter((r) => r.index < b.index && removed.has(r.entity)).length;
  return after.find((r) => r.resource === b.resource && r.scope === b.scope && r.index === b.index - skipped);
}

/** pretty formats JSON for display and line diffs. */
export function pretty(v: unknown): string {
  return v === undefined ? '' : JSON.stringify(v, null, 2);
}
