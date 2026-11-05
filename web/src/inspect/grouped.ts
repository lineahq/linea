// Groups a stage diff by what it touched: each resource, the scopes in it
// and the records in those, so a reader sees which resources and attributes
// a stage changed, dropped or created rather than a flat list of paths.
import type { Change, Diff } from '../api/types';
import { listRecords, signalOf, type RecordEntry } from './records';
import { decodeValue, displayValue, signalRows } from './signals';

type Json = Record<string, unknown>;
type Ref = NonNullable<Diff['removed']>[number];

/** Attribute is a key and its displayed value. */
export interface Attribute {
  key: string;
  value: string;
}

/** EntityChanges are the changes on one resource, scope or record. */
export interface EntityChanges {
  entity: string;
  label: string;
  changes: Change[];
  /** Attributes of the entity in the stage input that no change touched. */
  unchanged: Attribute[];
}

export interface RecordGroup extends EntityChanges {
  /** The stage emitted no counterpart of this record. */
  dropped?: boolean;
}

export interface ScopeGroup {
  index: number;
  scope?: EntityChanges;
  records: RecordGroup[];
}

export interface ResourceGroup {
  index: number;
  label: string;
  resource?: EntityChanges;
  scopes: ScopeGroup[];
}

export interface ChangeSummary {
  resources: number;
  scopes: number;
  records: number;
  added: number;
  removed: number;
  modified: number;
  dropped: number;
  created: number;
}

export interface GroupedChanges {
  summary: ChangeSummary;
  resources: ResourceGroup[];
  /** Records the stage emitted that match nothing in its input. */
  created: Ref[];
}

const arr = (v: unknown): Json[] => (Array.isArray(v) ? (v as Json[]) : []);

const RESOURCE = /^resource\[(\d+)\]$/;
const SCOPE = /^resource\[(\d+)\]\.scope\[(\d+)\]$/;
const POSITION = /^resource\[(\d+)\]\.scope\[(\d+)\]\./;
const ATTRIBUTE = /^attributes\[("(?:[^"\\]|\\.)*")\]/;

/** refEntity names a removed or added record the way changes name entities. */
export function refEntity(r: Ref): string {
  return r.span_id ? `span:${r.trace_id ?? ''}:${r.span_id}` : (r.ref ?? r.name);
}

/** attributeKey is the top-level attribute a change path is under, if any. */
export function attributeKey(path: string): string | undefined {
  const m = ATTRIBUTE.exec(path);
  if (!m) return undefined;
  try {
    return JSON.parse(m[1]) as string;
  } catch {
    return undefined;
  }
}

/** anyValue displays an OTLP JSON AnyValue. */
export function anyValue(v: unknown): string {
  return v === undefined ? '' : displayValue(decodeValue(v));
}

function attributes(obj: unknown): Attribute[] {
  return arr((obj as Json | undefined)?.attributes).map((a) => ({ key: String(a.key), value: anyValue(a.value) }));
}

function unchanged(obj: unknown, changes: Change[]): Attribute[] {
  const touched = new Set(changes.map((c) => attributeKey(c.path)).filter((k) => k !== undefined));
  return attributes(obj).filter((a) => !touched.has(a.key));
}

/** resourceLabel names a resource by its service.name, else its position. */
export function resourceLabel(resource: unknown, index: number): string {
  const name = attributes(resource).find((a) => a.key === 'service.name')?.value;
  return name ? `service.name = ${name}` : `resource #${index}`;
}

/**
 * groupChanges arranges a diff under the resources and scopes of the stage
 * input (before), which is what change entities and removed refs index.
 * Spans are placed by looking their ID up in before.
 */
export function groupChanges(diff: Diff | undefined, before: unknown): GroupedChanges {
  const changes = diff?.changes ?? [];
  const removed = diff?.removed ?? [];
  const signal = signalOf(before);
  const resourcesJson = signal ? arr((before as Json)[{ traces: 'resourceSpans', logs: 'resourceLogs', metrics: 'resourceMetrics' }[signal]]) : [];
  const scopesKey = signal ? { traces: 'scopeSpans', logs: 'scopeLogs', metrics: 'scopeMetrics' }[signal] : '';
  const records = listRecords(before);
  // Metric diffs identify individual data points, while the legacy picker
  // lists whole metrics. Include both so point edits have labels and context.
  if (signal === 'metrics') for (const row of signalRows(before)) {
    const m = /^resource\[(\d+)\]\.scope\[(\d+)\]\.metric\[\d+\]\.data_point\[(\d+)\]$/.exec(row.entity);
    if (m) records.push({ entity: row.entity, label: `${row.title} · ${row.value}`, resource: Number(m[1]), scope: Number(m[2]), index: Number(m[3]), value: row.raw.point as Json });
  }
  const byEntity = new Map<string, RecordEntry>(records.map((r) => [r.entity, r]));

  const resources = new Map<number, ResourceGroup>();
  const resourceAt = (i: number): ResourceGroup => {
    let g = resources.get(i);
    if (!g) {
      g = { index: i, label: resourceLabel(resourcesJson[i]?.resource, i), scopes: [] };
      resources.set(i, g);
    }
    return g;
  };
  const scopeAt = (ri: number, si: number): ScopeGroup => {
    const r = resourceAt(ri);
    let g = r.scopes.find((s) => s.index === si);
    if (!g) {
      g = { index: si, records: [] };
      r.scopes.push(g);
    }
    return g;
  };
  const position = (entity: string): [number, number] | undefined => {
    const rec = byEntity.get(entity);
    if (rec) return [rec.resource, rec.scope];
    const m = POSITION.exec(entity);
    return m ? [Number(m[1]), Number(m[2])] : undefined;
  };

  const perEntity = new Map<string, Change[]>();
  for (const c of changes) perEntity.set(c.entity, [...(perEntity.get(c.entity) ?? []), c]);

  const summary: ChangeSummary = { resources: 0, scopes: 0, records: 0, added: 0, removed: 0, modified: 0, dropped: removed.length, created: diff?.added?.length ?? 0 };
  for (const c of changes) {
    if (c.op === 'added') summary.added++;
    else if (c.op === 'removed') summary.removed++;
    else summary.modified++;
  }

  for (const [entity, cs] of perEntity) {
    let m: RegExpExecArray | null;
    if ((m = RESOURCE.exec(entity))) {
      const i = Number(m[1]);
      resourceAt(i).resource = { entity, label: 'resource', changes: cs, unchanged: unchanged(resourcesJson[i]?.resource, cs) };
      summary.resources++;
    } else if ((m = SCOPE.exec(entity))) {
      const [ri, si] = [Number(m[1]), Number(m[2])];
      const scope = arr(resourcesJson[ri]?.[scopesKey])[si]?.scope as Json | undefined;
      scopeAt(ri, si).scope = { entity, label: String(scope?.name ?? '') || `scope #${si}`, changes: cs, unchanged: unchanged(scope, cs) };
      summary.scopes++;
    } else {
      const rec = byEntity.get(entity);
      const g: RecordGroup = { entity, label: rec?.label ?? entity, changes: cs, unchanged: unchanged(rec?.value, cs) };
      summary.records++;
      const pos = position(entity);
      scopeAt(pos?.[0] ?? -1, pos?.[1] ?? -1).records.push(g);
    }
  }
  for (const r of removed) {
    const entity = refEntity(r);
    const rec = byEntity.get(entity);
    const g: RecordGroup = { entity, label: r.name || rec?.label || entity, changes: [], unchanged: attributes(rec?.value), dropped: true };
    const pos = position(entity);
    scopeAt(pos?.[0] ?? -1, pos?.[1] ?? -1).records.push(g);
  }

  const order = (a: { index: number }, b: { index: number }) => a.index - b.index;
  const grouped = [...resources.values()].sort(order);
  for (const r of grouped) {
    r.scopes.sort(order);
    for (const s of r.scopes) s.records.sort((a, b) => (byEntity.get(a.entity)?.index ?? 0) - (byEntity.get(b.entity)?.index ?? 0));
  }
  return { summary, resources: grouped, created: diff?.added ?? [] };
}

/** display renders a change value. */
export function display(v: unknown): string {
  return v === undefined ? '' : typeof v === 'string' ? v : JSON.stringify(v);
}
