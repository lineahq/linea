import { signalOf, type Signal } from './records';

type ObjectValue = Record<string, unknown>;
export type Fields = [string, string][];

export interface SignalRow {
  id: string;
  /** The analysis entity, also used by the record picker and journeys. */
  entity: string;
  /** Metric-level selection used by the existing diff inspector. */
  recordEntity: string;
  resourceEntity: string;
  scopeEntity: string;
  signal: Signal;
  title: string;
  service: string;
  time: string;
  timestamp?: bigint;
  category: string;
  badge: string;
  tone: 'neutral' | 'good' | 'warn' | 'bad';
  value: string;
  unit: string;
  fields: Fields;
  attributes: Fields;
  resource: Fields;
  scope: Fields;
  raw: ObjectValue;
  search: string;
  /** The request it came in, from 1, when rows join several payloads. */
  request?: number;
}

const object = (v: unknown): ObjectValue => v !== null && typeof v === 'object' && !Array.isArray(v) ? v as ObjectValue : {};
const array = (v: unknown): unknown[] => Array.isArray(v) ? v : [];
const text = (v: unknown): string => v === undefined || v === null ? '' : String(v);

/** Decode OTLP AnyValue without coercing int64 strings to imprecise numbers. */
export function decodeValue(v: unknown): unknown {
  const a = object(v);
  for (const key of ['stringValue', 'boolValue', 'intValue', 'doubleValue', 'bytesValue']) {
    if (key in a) return a[key];
  }
  if ('arrayValue' in a) return array(object(a.arrayValue).values).map(decodeValue);
  if ('kvlistValue' in a) return Object.fromEntries(array(object(a.kvlistValue).values).map((item) => {
    const entry = object(item);
    return [text(entry.key), decodeValue(entry.value)];
  }));
  return v;
}

export function displayValue(v: unknown): string {
  return v === undefined || v === null ? '—' : typeof v === 'object' ? JSON.stringify(v) : String(v);
}

function attributes(v: unknown): Fields {
  return array(v).map((item) => {
    const a = object(item);
    return [text(a.key), displayValue(decodeValue(a.value))];
  });
}

function integer(v: unknown): bigint | undefined {
  if (typeof v === 'number' && (!Number.isSafeInteger(v) || v < 0)) return undefined;
  if (typeof v !== 'string' && typeof v !== 'number') return undefined;
  if (!/^\d+$/.test(String(v))) return undefined;
  return BigInt(v);
}

/** OTLP zero means an unset timestamp. All displayed timestamps are UTC. */
export function timestamp(v: unknown): bigint | undefined {
  const n = integer(v);
  return n === 0n ? undefined : n;
}

export function formatTimestamp(v: unknown): string {
  const n = timestamp(v);
  if (n === undefined) return '—';
  const date = new Date(Number(n / 1_000_000n));
  if (!Number.isFinite(date.getTime())) return '—';
  return `${date.toISOString().split('.')[0]}.${String(n % 1_000_000_000n).padStart(9, '0')}Z`;
}

export function formatDuration(start: unknown, end: unknown): string {
  const a = timestamp(start), b = timestamp(end);
  if (a === undefined || b === undefined || b < a) return '—';
  const n = b - a;
  if (n < 1_000n) return `${n} ns`;
  const [scale, unit] = n < 1_000_000n ? [1_000, 'µs'] : n < 1_000_000_000n ? [1_000_000, 'ms'] : [1_000_000_000, 's'];
  return `${(Number(n) / scale).toLocaleString('en-US', { maximumFractionDigits: 3 })} ${unit}`;
}

const SEVERITIES = ['TRACE', 'DEBUG', 'INFO', 'WARN', 'ERROR', 'FATAL'];
function severity(r: ObjectValue): string {
  const n = Number(r.severityNumber);
  if (Number.isInteger(n) && n >= 1 && n <= 24) return SEVERITIES[Math.floor((n - 1) / 4)];
  const s = text(r.severityText).toUpperCase();
  if (s === 'WARNING') return 'WARN';
  return s || 'UNSPECIFIED';
}

const POINT_TYPES = ['gauge', 'sum', 'histogram', 'exponentialHistogram', 'summary'] as const;
const METRIC_NAMES: Record<string, string> = { gauge: 'Gauge', sum: 'Sum', histogram: 'Histogram', exponentialHistogram: 'Exponential histogram', summary: 'Summary' };
function temporality(v: unknown): string {
  return v === 1 || v === 'AGGREGATION_TEMPORALITY_DELTA' ? 'Delta' : v === 2 || v === 'AGGREGATION_TEMPORALITY_CUMULATIVE' ? 'Cumulative' : 'Unspecified';
}

function pointValue(type: string, p: ObjectValue): string {
  if ((Number(p.flags) & 1) !== 0) return 'No recorded value';
  if (type === 'gauge' || type === 'sum') return displayValue(p.asInt ?? p.asDouble);
  const count = displayValue(p.count);
  return `${count} observations${p.sum === undefined ? '' : ` · sum ${displayValue(p.sum)}`}`;
}

function pointFields(type: string, p: ObjectValue): Fields {
  const fields: Fields = [['Start time (UTC)', formatTimestamp(p.startTimeUnixNano)]];
  for (const key of ['count', 'sum', 'min', 'max', 'flags', 'scale', 'zeroCount', 'zeroThreshold']) {
    if (key in p) fields.push([key, displayValue(p[key])]);
  }
  if (type === 'histogram') {
    const bounds = array(p.explicitBounds);
    array(p.bucketCounts).forEach((count, i) => {
      const lower = i === 0 ? '−∞' : displayValue(bounds[i - 1]);
      const upper = i === bounds.length ? '+∞' : displayValue(bounds[i]);
      fields.push([`Bucket (${lower}, ${upper}]`, displayValue(count)]);
    });
  }
  if (type === 'summary') array(p.quantileValues).forEach((v) => {
    const q = object(v);
    fields.push([`Quantile ${displayValue(q.quantile)}`, displayValue(q.value)]);
  });
  for (const side of ['positive', 'negative']) {
    if (side in p) {
      const b = object(p[side]);
      fields.push([`${side} bucket offset`, displayValue(b.offset ?? 0)], [`${side} bucket counts`, displayValue(b.bucketCounts)]);
    }
  }
  if (array(p.exemplars).length) fields.push(['Exemplars', displayValue(p.exemplars)]);
  return fields;
}

/** Presentation only: keep resource/scope context and the original analysis IDs. */
export function signalRows(payload: unknown): SignalRow[] {
  const signal = signalOf(payload);
  if (!signal) return [];
  const [resourceKey, scopeKey, itemKey] = signal === 'logs'
    ? ['resourceLogs', 'scopeLogs', 'logRecords']
    : signal === 'traces' ? ['resourceSpans', 'scopeSpans', 'spans'] : ['resourceMetrics', 'scopeMetrics', 'metrics'];
  const rows: SignalRow[] = [];
  array(object(payload)[resourceKey]).forEach((rv, ri) => {
    const r = object(rv);
    const resource = attributes(object(r.resource).attributes);
    const service = resource.find(([k]) => k === 'service.name')?.[1] ?? 'Unknown service';
    array(r[scopeKey]).forEach((sv, si) => {
      const s = object(sv), scopeInfo = object(s.scope);
      const scope: Fields = [
        ['Name', text(scopeInfo.name) || 'Unnamed scope'],
        ...(scopeInfo.version ? [['Version', text(scopeInfo.version)] as [string, string]] : []),
        ...attributes(scopeInfo.attributes),
      ];
      array(s[itemKey]).forEach((iv, ii) => {
        if (iv === null || typeof iv !== 'object' || Array.isArray(iv)) return;
        const item = object(iv);
        const entity = signal === 'traces' ? `span:${item.traceId ?? ''}:${item.spanId ?? ''}` : `resource[${ri}].scope[${si}].${signal === 'logs' ? 'log_record' : 'metric'}[${ii}]`;
        const base = { signal, entity, recordEntity: entity, resourceEntity: `resource[${ri}]`, scopeEntity: `resource[${ri}].scope[${si}]`, service, resource, scope, unit: '', tone: 'neutral' as SignalRow['tone'] };
        const add = (row: Omit<SignalRow, 'search'>) => rows.push({ ...row, search: [row.title, row.service, row.time, row.badge, row.value, row.unit, ...row.fields.flat(), ...row.attributes.flat(), ...row.resource.flat(), ...row.scope.flat()].join(' ').toLowerCase() });
        const timeValue = timestamp(item.timeUnixNano) === undefined ? item.observedTimeUnixNano : item.timeUnixNano;
        if (signal === 'logs') {
          const category = severity(item);
          add({ ...base, id: entity, title: displayValue(decodeValue(item.body)),
            time: formatTimestamp(timeValue), timestamp: timestamp(timeValue), category,
            badge: text(item.severityText) || category,
            tone: category === 'ERROR' || category === 'FATAL' ? 'bad' : category === 'WARN' ? 'warn' : 'neutral',
            value: '', attributes: attributes(item.attributes), raw: item,
            fields: [['Severity', text(item.severityText) || category], ['Severity number', displayValue(item.severityNumber)],
              ['Recorded time (UTC)', formatTimestamp(item.timeUnixNano)], ['Observed time (UTC)', formatTimestamp(item.observedTimeUnixNano)],
              ['Trace ID', text(item.traceId) || '—'], ['Span ID', text(item.spanId) || '—'],
              ...(item.eventName ? [['Event name', text(item.eventName)] as [string, string]] : [])] });
        } else if (signal === 'traces') {
          const status = object(item.status);
          const category = status.code === 2 || status.code === 'STATUS_CODE_ERROR' ? 'Error' : status.code === 1 || status.code === 'STATUS_CODE_OK' ? 'OK' : 'Unset';
          const kinds = ['Unspecified', 'Internal', 'Server', 'Client', 'Producer', 'Consumer'];
          const kind = typeof item.kind === 'string' ? item.kind.replace('SPAN_KIND_', '') : kinds[Number(item.kind ?? 0)] ?? 'Unspecified';
          add({ ...base, id: `${entity}:${ri}:${si}:${ii}`, title: text(item.name) || 'Unnamed span',
            time: formatTimestamp(item.startTimeUnixNano), timestamp: timestamp(item.startTimeUnixNano),
            category, badge: category, tone: category === 'Error' ? 'bad' : category === 'OK' ? 'good' : 'neutral',
            value: formatDuration(item.startTimeUnixNano, item.endTimeUnixNano), attributes: attributes(item.attributes), raw: item,
            fields: [['Trace ID', text(item.traceId) || '—'], ['Span ID', text(item.spanId) || '—'], ['Parent span ID', text(item.parentSpanId) || '—'],
              ['Kind', kind], ['Status', category], ['Status message', text(status.message) || '—'],
              ['Start time (UTC)', formatTimestamp(item.startTimeUnixNano)], ['End time (UTC)', formatTimestamp(item.endTimeUnixNano)],
              ['Duration', formatDuration(item.startTimeUnixNano, item.endTimeUnixNano)],
              ...array(item.events).flatMap((v, i): Fields => {
                const e = object(v);
                return [[`Event ${i + 1}`, `${formatTimestamp(e.timeUnixNano)} · ${text(e.name)}`], ...attributes(e.attributes)];
              }),
              ...array(item.links).map((v, i): [string, string] => [`Link ${i + 1}`, displayValue(v)])] });
        } else {
          const type = POINT_TYPES.find((key) => key in item);
          const data = type ? object(item[type]) : {};
          const points = array(data.dataPoints);
          const category = type ? METRIC_NAMES[type] : 'Unknown';
          // Retain empty metrics too, clearly distinguished from a recorded point.
          (points.length ? points : [undefined]).forEach((pv, pi) => {
            const p = object(pv);
            const pointEntity = `${entity}.data_point[${pi}]`;
            add({ ...base, entity: pv === undefined ? entity : pointEntity, id: pointEntity, title: text(item.name) || 'Unnamed metric',
              time: formatTimestamp(p.timeUnixNano), timestamp: timestamp(p.timeUnixNano), category, badge: category,
              unit: text(item.unit), value: pv === undefined ? 'No data points' : pointValue(type ?? '', p),
              attributes: attributes(p.attributes), raw: { metric: item, point: pv },
              fields: [['Description', text(item.description) || '—'], ['Type', category], ['Unit', text(item.unit) || '—'],
                ...('aggregationTemporality' in data ? [['Temporality', temporality(data.aggregationTemporality)] as [string, string]] : []),
                ...('isMonotonic' in data ? [['Monotonic', displayValue(data.isMonotonic)] as [string, string]] : []),
                ...(pv === undefined ? [] : pointFields(type ?? '', p))] });
          });
        }
      });
    });
  });
  return rows;
}

/**
 * requestRows joins the rows of several payloads of one signal, in order,
 * each tagged with its request. Entities stay those of their own payload.
 */
export function requestRows(payloads: readonly unknown[]): SignalRow[] {
  return payloads.flatMap((p, i) => signalRows(p).map((r) => ({ ...r, id: `${i + 1}:${r.id}`, request: i + 1 })));
}

export interface SignalFilters {
  query: string; service: string; category: string; sort: 'recorded' | 'newest' | 'oldest'; record?: string;
  /** Only this request's rows; the record, when set, is looked up in it. */
  request?: number;
}

/** Plain text terms are ANDed; no backend query language is implied. */
export function filterSignalRows(rows: SignalRow[], filters: SignalFilters): SignalRow[] {
  const terms = filters.query.toLowerCase().trim().split(/\s+/).filter(Boolean);
  const filtered = rows.filter((r) => (!filters.request || r.request === filters.request) && (!filters.record || r.entity === filters.record || r.recordEntity === filters.record || r.recordEntity.startsWith(`${filters.record}.`))
    && (!filters.service || r.service === filters.service)
    && (!filters.category || r.category === filters.category)
    && terms.every((t) => r.search.includes(t)));
  if (filters.sort !== 'recorded') filtered.sort((a, b) => {
    if (a.timestamp === undefined) return b.timestamp === undefined ? 0 : 1;
    if (b.timestamp === undefined) return -1;
    const cmp = a.timestamp < b.timestamp ? -1 : a.timestamp > b.timestamp ? 1 : 0;
    return filters.sort === 'oldest' ? cmp : -cmp;
  });
  return filtered;
}
