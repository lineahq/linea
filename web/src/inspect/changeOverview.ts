import type { Change, Diff } from '../api/types';
import { attributeKey, display, groupChanges, refEntity } from './grouped';
import { signalOf } from './records';
import { signalRows, type Fields } from './signals';

export type ChangeKind = 'all' | 'added' | 'removed' | 'modified' | 'type_changed' | 'dropped' | 'created';
export type Target = 'resource' | 'scope' | 'record';

export interface OverviewEdit {
  change: Change;
  label: string;
  context: string;
  sharedRecords: number;
}

export interface FieldChange {
  id: string;
  path: string;
  field: string;
  target: Target;
  targetLabel: string;
  op: string;
  edits: OverviewEdit[];
  entities: number;
  /** Input records sharing a changed resource/scope; not direct record edits. */
  sharedRecords: number;
}

export interface RecordChange {
  entity: string;
  label: string;
  service: string;
  attributes: Fields;
  kind: 'dropped' | 'created';
}

export interface ChangesOverview {
  fields: FieldChange[];
  records: RecordChange[];
  editedRecords: number;
  editedResources: number;
  editedScopes: number;
  counts: Record<ChangeKind, number>;
}

export const OP_LABEL: Record<string, string> = { added: 'Added', removed: 'Removed', modified: 'Modified', type_changed: 'Type changed' };

/** Keep the actual path in the tooltip; use readable field names in the overview. */
export function fieldLabel(path: string): string {
  const key = attributeKey(path);
  if (key !== undefined) {
    // Attribute keys can themselves contain dots, quotes and brackets.
    const prefix = `attributes[${JSON.stringify(key)}]`;
    return `${key}${path.startsWith(prefix) ? path.slice(prefix.length) : ''}`;
  }
  return path.split('.').map((part) => part.replaceAll('_', ' ')).join(' · ');
}

/** Exact entity/ancestor matching; indexes in an unrelated resource never match. */
export function includesEntity(selection: string | undefined, entity: string): boolean {
  return !selection || entity === selection || entity.startsWith(`${selection}.`);
}

function targetOf(entity: string): Target {
  return /^resource\[\d+\]$/.test(entity) ? 'resource' : /^resource\[\d+\]\.scope\[\d+\]$/.test(entity) ? 'scope' : 'record';
}

/** Group repeated edits by field and operation, keeping varying values in details. */
export function changeOverview(diff: Diff | undefined, before: unknown, after: unknown, record?: string): ChangesOverview {
  const beforeRows = signalRows(before), afterRows = signalRows(after);
  const inputRecords = beforeRows.filter((r) => r.signal !== 'metrics' || r.entity !== r.recordEntity);
  const g = groupChanges(diff, before);
  const labels = new Map<string, { label: string; context: string }>();
  for (const r of g.resources) {
    if (r.resource) labels.set(r.resource.entity, { label: r.label, context: 'Shared resource' });
    for (const s of r.scopes) {
      if (s.scope) labels.set(s.scope.entity, { label: s.scope.label, context: r.label });
    }
  }
  for (const row of beforeRows) {
    labels.set(row.entity, { label: row.title, context: row.signal === 'metrics' ? `${row.service} · ${row.value}${row.unit && row.unit !== '1' ? ` ${row.unit}` : ''}` : row.service });
    labels.set(row.recordEntity, { label: row.title, context: row.service });
  }
  const selectedRows = record ? beforeRows.filter((r) => includesEntity(record, r.entity) || r.resourceEntity === record || r.scopeEntity === record) : [];
  // Resource/scope edits remain relevant when a contained record is selected.
  const relevant = (entity: string) => includesEntity(record, entity) || (!!record && selectedRows.some((r) => r.entity === entity || r.recordEntity === entity || r.resourceEntity === entity || r.scopeEntity === entity));
  const changes = (diff?.changes ?? []).filter((c) => relevant(c.entity));
  const signal = signalOf(before) ?? signalOf(after);
  const noun = signal === 'logs' ? 'Log' : signal === 'traces' ? 'Span' : signal === 'metrics' ? 'Data point' : 'Record';
  const groups = new Map<string, FieldChange>();
  const counts: Record<ChangeKind, number> = { all: 0, added: 0, removed: 0, modified: 0, type_changed: 0, dropped: 0, created: 0 };
  for (const c of changes) {
    const target = targetOf(c.entity);
    const id = JSON.stringify([target, c.path, c.op]);
    let field = groups.get(id);
    if (!field) {
      field = { id, path: c.path, field: fieldLabel(c.path), target,
        targetLabel: `${target === 'resource' ? 'Resource' : target === 'scope' ? 'Scope' : noun} ${attributeKey(c.path) === undefined ? 'field' : 'attribute'}`,
        op: c.op, edits: [], entities: 0, sharedRecords: 0 };
      groups.set(id, field);
    }
    field.edits.push({ change: c, sharedRecords: target === 'record' ? 0 : inputRecords.filter((r) => r.resourceEntity === c.entity || r.scopeEntity === c.entity).length, ...(labels.get(c.entity) ?? { label: c.entity, context: '' }) });
    if (c.op in counts && c.op !== 'all') counts[c.op as ChangeKind]++;
  }
  for (const field of groups.values()) {
    const entities = new Set(field.edits.map((e) => e.change.entity));
    field.entities = entities.size;
    field.sharedRecords = [...new Map(field.edits.map((e) => [e.change.entity, e.sharedRecords])).values()].reduce((a, b) => a + b, 0);
  }
  const records: RecordChange[] = [];
  for (const kind of ['dropped', 'created'] as const) {
    const refs = kind === 'dropped' ? diff?.removed : diff?.added;
    const rows = kind === 'dropped' ? beforeRows : afterRows;
    for (const ref of refs ?? []) {
      const entity = refEntity(ref);
      // Created positions belong to the output; never equate them with an
      // input record simply because their numeric indexes happen to match.
      if (record && (kind === 'created' || !relevant(entity))) continue;
      const row = rows.find((r) => r.entity === entity);
      records.push({ entity, label: row?.title || ref.name || entity, service: row?.service ?? '', attributes: row?.attributes ?? [], kind });
      counts[kind]++;
    }
  }
  counts.all = changes.length + records.length;
  const unique = (target: Target) => new Set(changes.filter((c) => targetOf(c.entity) === target).map((c) => c.entity)).size;
  return { fields: [...groups.values()], records, editedRecords: unique('record'), editedResources: unique('resource'), editedScopes: unique('scope'), counts };
}

export function filterOverview(overview: ChangesOverview, query: string, kind: ChangeKind): Pick<ChangesOverview, 'fields' | 'records'> {
  const terms = query.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const matches = (text: string) => terms.every((t) => text.toLowerCase().includes(t));
  const fields = overview.fields.flatMap((f) => {
    if (kind !== 'all' && kind !== f.op) return [];
    // Keep only matching edits so a search never hides which values matched.
    const edits = f.edits.filter((e) => matches([f.field, f.path, f.targetLabel, OP_LABEL[f.op], e.label, e.context, display(e.change.before), display(e.change.after)].join(' ')));
    return edits.length ? [{ ...f, edits, entities: new Set(edits.map((e) => e.change.entity)).size, sharedRecords: [...new Map(edits.map((e) => [e.change.entity, e.sharedRecords])).values()].reduce((a, b) => a + b, 0) }] : [];
  });
  const records = overview.records.filter((r) => (kind === 'all' || kind === r.kind) && matches([r.label, r.service, r.kind, ...r.attributes.flat()].join(' ')));
  return { fields, records };
}

export interface ChangeValue { text: string; type?: string }

/** Include pdata types in distinct-value keys: integer 5 and string "5" differ. */
export function distinctValues(edits: OverviewEdit[], side: 'before' | 'after'): ChangeValue[] {
  const values = new Map<string, ChangeValue>();
  for (const { change } of edits) {
    const absent = side === 'before' ? change.op === 'added' : change.op === 'removed';
    const type = change[side === 'before' ? 'before_type' : 'after_type'];
    const value = { text: absent ? 'Not present' : change[side] === undefined ? 'Not recorded' : display(change[side]), type: absent ? undefined : type };
    values.set(JSON.stringify([absent, type, change[side]]), value);
  }
  return [...values.values()];
}
