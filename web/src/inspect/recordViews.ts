// Per-record view of a stage's before/after comparison. Every record of the
// stage input (and every record the stage created) becomes one RecordView
// whose fields are grouped by where they live (resource, scope, record) and
// carry the OTTL path used to reference them. Pure presentation over the
// diff and the recorded payloads: nothing is re-computed or invented.
import type { Change, Diff, OTTLReport } from '../api/types';
import { refEntity } from './grouped';
import { signalOf, type Signal } from './records';
import { signalRows, type SignalRow } from './signals';
import { diffValue, otlpValue } from './structuredDiff';

export type Level = 'resource' | 'scope' | 'record';
/** recorded: shown without a comparison, so nothing is claimed about changes. */
export type RecordState = 'edited' | 'dropped' | 'new' | 'unchanged' | 'recorded';

export const LEVELS: Level[] = ['resource', 'scope', 'record'];
export const LEVEL_LABELS: Record<Level, string> = { resource: 'Resource', scope: 'Scope', record: 'Record' };
export const LEVEL_HELP: Record<Level, string> = {
  resource: 'The service or host that sent it',
  scope: 'The library that produced it',
  record: 'The record itself',
};

/**
 * Table headings for a list of records of one signal. The badge on a row is the
 * log severity, the span status or the metric type, so the heading says which.
 */
export function recordColumns(signal: Signal | undefined): { badge: string; record: string } {
  return signal === 'logs' ? { badge: 'Severity', record: 'Message' }
    : signal === 'traces' ? { badge: 'Status', record: 'Operation' }
      : signal === 'metrics' ? { badge: 'Type', record: 'Metric' }
        : { badge: 'Kind', record: 'Record' };
}

export interface FieldView {
  level: Level;
  /** The path to reference the field from OTTL. */
  path: string;
  op: 'added' | 'removed' | 'modified' | 'type_changed' | 'same';
  before?: string;
  after?: string;
  /** Value type, e.g. "str", or "int → str" when it changed. */
  type?: string;
  confidence?: string;
  /** Labels of the statements that changed it, when a replay is available. */
  by?: string;
}

export interface RecordView {
  id: string;
  /** The analysis entity, which journeys and the record focus use. */
  entity: string;
  state: RecordState;
  signal: Signal;
  badge: string;
  tone: SignalRow['tone'];
  title: string;
  service: string;
  resourceEntity: string;
  scopeEntity: string;
  /** The snapshot the record lives in. */
  side: 'before' | 'after';
  fields: FieldView[];
  /** Number of changed fields, shared resource and scope fields included. */
  changed: number;
  /** Levels that have a change. */
  levels: Level[];
  /** 'medium' when a change was matched by content with medium confidence. */
  confidence?: 'medium';
}

export interface RecordViews {
  records: RecordView[];
  /** Changes whose entity matches no recorded record. */
  unplaced: Change[];
}

type Json = Record<string, unknown>;
interface Plain { level: Level; name: string; value: string; type?: string }

const RESOURCE = /^resource\[\d+\]$/;
const SCOPE = /^resource\[\d+\]\.scope\[\d+\]$/;
const POSITION = /^resource\[(\d+)\](?:\.scope\[(\d+)\])?$/;
const CONTEXT: Record<Signal, string> = { logs: 'log', traces: 'span', metrics: 'metric' };
const KEYS: Record<Signal, [string, string]> = {
  logs: ['resourceLogs', 'scopeLogs'], traces: ['resourceSpans', 'scopeSpans'], metrics: ['resourceMetrics', 'scopeMetrics'],
};

const levelOf = (entity: string): Level => (RESOURCE.test(entity) ? 'resource' : SCOPE.test(entity) ? 'scope' : 'record');
const arr = (v: unknown): Json[] => (Array.isArray(v) ? (v as Json[]) : []);

/** ottlPath turns a diff path into the path OTTL uses for the same field. */
export function ottlPath(signal: Signal, level: Level, path: string, valueType?: string, point = false): string {
  if (level === 'resource') return `resource.${path}`;
  if (level === 'scope') return `instrumentation_scope.${path}`;
  if (signal === 'metrics') {
    if (path.startsWith('metric.')) return path;
    if (path === 'value') return `datapoint.value_${/^int/i.test(valueType ?? '') ? 'int' : 'double'}`;
    return `${point ? 'datapoint' : 'metric'}.${path}`;
  }
  return `${CONTEXT[signal]}.${path}`;
}

const TYPE_NAMES: Record<string, string> = {
  stringvalue: 'str', intvalue: 'int', doublevalue: 'double', boolvalue: 'bool', bytesvalue: 'bytes', arrayvalue: 'slice', kvlistvalue: 'map',
};
const typeName = (t?: string) => t?.toLowerCase();
const otlpType = (v: unknown): string | undefined => {
  const key = v && typeof v === 'object' ? Object.keys(v as object)[0] : undefined;
  return key ? TYPE_NAMES[key.toLowerCase()] : undefined;
};
const attributeName = (key: string) => `attributes[${JSON.stringify(key)}]`;

function changeField(signal: Signal, c: Change, point: boolean, by?: Map<string, string>): FieldView {
  const level = levelOf(c.entity);
  const type = c.op === 'type_changed' ? `${typeName(c.before_type)} → ${typeName(c.after_type)}` : typeName(c.after_type ?? c.before_type);
  return {
    level,
    path: ottlPath(signal, level, c.path, c.after_type ?? c.before_type, point),
    op: c.op as FieldView['op'],
    before: c.op === 'added' ? undefined : diffValue(c.before, c.before_type),
    after: c.op === 'removed' ? undefined : diffValue(c.after, c.after_type),
    type,
    confidence: c.confidence,
    by: by?.get(`${c.entity}\u0000${c.path}`),
  };
}

const attrs = (obj: unknown): { key: string; value: unknown }[] => {
  const list = (obj as { attributes?: unknown } | undefined)?.attributes;
  return (Array.isArray(list) ? list : []) as { key: string; value: unknown }[];
};

/** Fields of a record that exist whether or not they changed: its primary fields and attributes. */
function recordFields(row: SignalRow): Plain[] {
  const out: Plain[] = [];
  const raw = row.signal === 'metrics' ? (row.raw.metric as Json | undefined) : row.raw;
  const point = row.signal === 'metrics' ? (row.raw.point as Json | undefined) : undefined;
  if (row.signal === 'logs') {
    if (raw?.severityText) out.push({ level: 'record', name: 'severity_text', value: diffValue(String(raw.severityText)), type: 'str' });
    out.push({ level: 'record', name: 'body', value: otlpValue(raw?.body), type: otlpType(raw?.body) });
  } else if (row.signal === 'traces') {
    out.push({ level: 'record', name: 'name', value: diffValue(String(raw?.name ?? '')), type: 'str' });
  } else {
    out.push({ level: 'record', name: 'metric.name', value: diffValue(String(raw?.name ?? '')), type: 'str' });
    if (raw?.unit) out.push({ level: 'record', name: 'metric.unit', value: diffValue(String(raw.unit)), type: 'str' });
  }
  for (const a of attrs(row.signal === 'metrics' ? point : raw)) {
    out.push({ level: 'record', name: attributeName(a.key), value: otlpValue(a.value), type: otlpType(a.value) });
  }
  return out;
}

/** The resource or scope fields of a record, read from the payload it lives in with their OTLP types. */
function contextFields(payload: unknown, signal: Signal, entity: string, level: 'resource' | 'scope'): Plain[] {
  const m = POSITION.exec(entity);
  if (!m) return [];
  const [resourceKey, scopeKey] = KEYS[signal];
  const r = arr((payload as Json | undefined)?.[resourceKey])[Number(m[1])];
  if (level === 'resource') {
    return attrs(r?.resource).map((a) => ({ level, name: attributeName(a.key), value: otlpValue(a.value), type: otlpType(a.value) }));
  }
  const scope = arr(r?.[scopeKey])[Number(m[2])]?.scope as Json | undefined;
  const out: Plain[] = [];
  if (scope?.name) out.push({ level, name: 'name', value: diffValue(String(scope.name)), type: 'str' });
  if (scope?.version) out.push({ level, name: 'version', value: diffValue(String(scope.version)), type: 'str' });
  for (const a of attrs(scope)) out.push({ level, name: attributeName(a.key), value: otlpValue(a.value), type: otlpType(a.value) });
  return out;
}

/** statementLabels maps "entity\0path" of each change to the statements of a transform replay that made it. */
export function statementLabels(report?: OTTLReport): Map<string, string> {
  const labels = new Map<string, string[]>();
  const several = (report?.groups?.length ?? 0) > 1;
  for (const g of report?.groups ?? []) for (const s of g.statements ?? []) for (const c of s.changes ?? []) {
    const key = `${c.entity}\u0000${c.path}`;
    const label = several ? `${g.index}.${s.index}` : `S${s.index}`;
    labels.set(key, [...new Set([...(labels.get(key) ?? []), label])]);
  }
  return new Map([...labels].map(([k, v]) => [k, v.join(', ')]));
}

/**
 * recordViews lists one view per record: those of the stage input, in
 * order, then those the stage created. Without a diff nothing is claimed
 * about changes: the records of both snapshots are listed as recorded.
 */
export function recordViews(diff: Diff | undefined, before: unknown, after: unknown, report?: OTTLReport): RecordViews {
  const signal = signalOf(before) ?? signalOf(after);
  if (!signal) return { records: [], unplaced: diff?.changes ?? [] };
  const by = report ? statementLabels(report) : undefined;
  const removed = new Set((diff?.removed ?? []).map(refEntity));
  const created = new Set((diff?.added ?? []).map(refEntity));
  const perEntity = new Map<string, Change[]>();
  for (const c of diff?.changes ?? []) perEntity.set(c.entity, [...(perEntity.get(c.entity) ?? []), c]);
  const placed = new Set<string>();

  const view = (row: SignalRow, state: RecordState, payload: unknown, side: 'before' | 'after'): RecordView => {
    const point = row.entity !== row.recordEntity;
    const fields: FieldView[] = [];
    const context = [
      ...contextFields(payload, signal, row.resourceEntity, 'resource'),
      ...contextFields(payload, signal, row.scopeEntity, 'scope'),
      ...recordFields(row),
    ];
    if (state === 'dropped' || state === 'new') {
      const op = state === 'dropped' ? 'removed' : 'added';
      for (const f of context) fields.push({
        level: f.level, path: ottlPath(signal, f.level, f.name, f.type, point), op, type: f.type,
        [state === 'dropped' ? 'before' : 'after']: f.value,
      });
    } else {
      const entities = [row.resourceEntity, row.scopeEntity, row.entity, ...(point ? [row.recordEntity] : [])];
      entities.forEach((e) => placed.add(e));
      const changes = entities.flatMap((e) => perEntity.get(e) ?? []);
      const mapped = changes.map((c) => changeField(signal, c, point, by));
      const changedPaths = new Set(mapped.map((f) => f.path));
      const same = context
        .map((f): FieldView => ({ level: f.level, path: ottlPath(signal, f.level, f.name, f.type, point), op: 'same', before: f.value, after: f.value, type: f.type }))
        .filter((f) => !changedPaths.has(f.path));
      for (const level of LEVELS) fields.push(...same.filter((f) => f.level === level), ...mapped.filter((f) => f.level === level));
    }
    const changedFields = state === 'edited' || state === 'unchanged' ? fields.filter((f) => f.op !== 'same') : [];
    // Only a content match of medium confidence is worth the reader's attention.
    const confidence = changedFields.some((f) => f.confidence === 'medium') ? 'medium' : undefined;
    return {
      id: `${side}:${row.id}`, entity: row.entity, state, signal, badge: row.badge, tone: row.tone, title: row.title, service: row.service,
      resourceEntity: row.resourceEntity, scopeEntity: row.scopeEntity, side, fields,
      changed: changedFields.length, levels: LEVELS.filter((l) => changedFields.some((f) => f.level === l)), confidence,
    };
  };

  const records: RecordView[] = [];
  if (!diff) {
    for (const row of signalRows(before)) records.push(view(row, 'recorded', before, 'before'));
    for (const row of signalRows(after)) records.push(view(row, 'recorded', after, 'after'));
    return { records, unplaced: [] };
  }
  for (const row of signalRows(before)) {
    if (removed.has(row.entity) || removed.has(row.recordEntity)) { records.push(view(row, 'dropped', before, 'before')); continue; }
    const v = view(row, 'unchanged', before, 'before');
    records.push(v.changed ? { ...v, state: 'edited' } : v);
  }
  for (const row of signalRows(after)) if (created.has(row.entity) || created.has(row.recordEntity)) records.push(view(row, 'new', after, 'after'));
  return { records, unplaced: (diff.changes ?? []).filter((c) => !placed.has(c.entity)) };
}
