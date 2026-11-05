import { useId, useMemo, useState } from 'react';
import type { Diff } from '../api/types';
import { distinctValues, filterOverview, OP_LABEL, type ChangeKind, type ChangesOverview as Overview, type FieldChange, type RecordChange } from './changeOverview';
import { signalOf } from './records';

const PAGE_SIZE = 25;
const FILTERS: [ChangeKind, string][] = [['all', 'All changes'], ['modified', 'Modified'], ['added', 'Added fields'], ['removed', 'Removed fields'], ['type_changed', 'Type changed'], ['dropped', 'Dropped records'], ['created', 'New records']];

interface Props {
  overview: Overview;
  diff: Diff;
  before: unknown;
  after?: unknown;
  record?: string;
  mode: 'fields' | 'records';
  onMode(mode: 'fields' | 'records'): void;
  onRecord(entity: string | undefined): void;
  onFollow?(entity: string): (() => void) | undefined;
  onCreatedFollow?(entity: string): (() => void) | undefined;
}

/** Summarize a whole stage input, then allow the reader to inspect repeated edits once. */
export function ChangesOverview({ overview, diff, before, after, record, mode, onMode, onRecord, onFollow, onCreatedFollow }: Props) {
  const [query, setQuery] = useState('');
  const [kind, setKind] = useState<ChangeKind>('all');
  const [page, setPage] = useState(0);
  const visible = useMemo(() => filterOverview(overview, query, kind), [overview, query, kind]);
  const signal = signalOf(before) ?? signalOf(after);
  const noun = signal === 'logs' ? 'log records' : signal === 'traces' ? 'spans' : signal === 'metrics' ? 'data points' : 'records';
  const edits = overview.fields.reduce((n, f) => n + f.edits.length, 0);
  const fields = new Set(overview.fields.map((f) => JSON.stringify([f.target, f.path]))).size;
  const nothing = overview.counts.all === 0;
  const incomplete = !!(diff.matching?.unmatched_before || diff.matching?.unmatched_after);
  const pages = Math.max(1, Math.ceil(visible.fields.length / PAGE_SIZE));
  const current = Math.min(page, pages - 1);
  const clear = () => { setQuery(''); setKind('all'); setPage(0); };

  return <div className="change-overview">
    <div className="change-title">
      <h3>What changed</h3>
      <span className="change-flow"><strong>{diff.before.items}</strong><span aria-hidden="true"> → </span><strong>{diff.after.items}</strong> {noun} <span className="muted">· whole input</span></span>
    </div>
    {record && <div className="change-selection"><span>Overview filtered to the selected record and its shared resource/scope fields.</span><button type="button" className="link" onClick={() => onRecord(undefined)}>Show whole input</button></div>}
    <div className="change-cards" aria-label="Change summary">
      <SummaryCard label="Field edits" value={edits} hint={`${fields} distinct field${fields === 1 ? '' : 's'}`} tone="modified" />
      <SummaryCard label="Records edited" value={overview.editedRecords} hint="Direct field changes" tone="modified" />
      <SummaryCard label="Records dropped" value={overview.counts.dropped} hint="Absent from the output" tone="removed" />
      <SummaryCard label="New records" value={overview.counts.created} hint="Added to the output" tone="added" />
    </div>
    {incomplete && <div className="change-matching" role="note">
      <strong>Matching incomplete</strong>
      <span>{diff.matching!.unmatched_before} input records · {diff.matching!.unmatched_after} output records could not be paired{record ? ' in the whole input' : ''}.</span>
      <span>Edits, drops or additions cannot be determined for these records.</span>
    </div>}
    {(overview.editedResources > 0 || overview.editedScopes > 0) && <p className="change-context muted">
      {overview.editedResources} resource{overview.editedResources === 1 ? '' : 's'} and {overview.editedScopes} scope{overview.editedScopes === 1 ? '' : 's'} edited. Shared fields are listed once, separately from direct record edits.
    </p>}
    {nothing && !incomplete && <p className={`change-empty ${diff.unresolved?.length ? 'note' : ''}`}>{diff.unresolved?.length || diff.identity === 'unresolved' ? 'No field or record changes could be established for this selection. See the matching limitations below.' : diff.before.items !== diff.after.items ? 'Record counts changed, but no field edits or record identities were reported.' : record ? 'No changes were recorded for this selection.' : 'This stage passed the telemetry through without recorded field edits, dropped records or new records.'}</p>}
    <div className="change-controls">
      <div className="change-mode" aria-label="Group changes">
        <button type="button" onClick={() => onMode(mode === 'fields' ? 'records' : 'fields')}>{mode === 'fields' ? 'Open resource / record details' : '← Back to field overview'}</button>
      </div>
      {mode === 'fields' && <label className="change-search"><span className="sr-only">Search changes</span><input type="search" value={query} placeholder="Search fields, values or records…" onChange={(e) => { setQuery(e.target.value); setPage(0); }} /></label>}
    </div>
    {mode === 'fields' && <>
      {!nothing && <div className="change-filters" aria-label="Filter changes">
        <label>Change type <select aria-label="Change type" value={kind} onChange={(e) => { setKind(e.target.value as ChangeKind); setPage(0); }}>
          {FILTERS.map(([key, label]) => <option key={key} value={key} disabled={key !== 'all' && overview.counts[key] === 0}>{label} · {overview.counts[key]}</option>)}
        </select></label>
        {(query || kind !== 'all') && <button type="button" className="link" onClick={clear}>Clear filters</button>}
      </div>}
      {(query || kind !== 'all') && <p className="change-result muted" role="status">{visible.fields.length} field group{visible.fields.length === 1 ? '' : 's'} · {visible.records.length} dropped/new record{visible.records.length === 1 ? '' : 's'} match</p>}
      {!nothing && !visible.fields.length && !visible.records.length && <p className="change-empty muted">No changes match these filters.</p>}
      {visible.fields.length > 0 && <>
        <div className="change-table-wrap"><table className="change-field-table">
          <caption className="sr-only">Field changes grouped across affected records</caption>
          <thead><tr><th>Field</th><th>Change</th><th>Before</th><th>After</th><th>Affected</th></tr></thead>
          <tbody>{visible.fields.slice(current * PAGE_SIZE, (current + 1) * PAGE_SIZE).map((f) => <FieldRow key={f.id} field={f} onRecord={onRecord} onFollow={onFollow} />)}</tbody>
        </table></div>
        {pages > 1 && <Pagination current={current} pages={pages} onPage={setPage} label="Field groups" />}
      </>}
      <RecordList records={visible.records.filter((r) => r.kind === 'dropped')} kind="dropped" onRecord={onRecord} onFollow={onFollow} />
      <RecordList records={visible.records.filter((r) => r.kind === 'created')} kind="created" onFollow={onCreatedFollow} />
    </>}
    {(diff.unresolved?.length ?? 0) > 0 && <details className="change-limitations" open={!incomplete}><summary>Matching limitations</summary>{diff.unresolved!.map((note, i) => <p key={i} className="note">{note}</p>)}</details>}
  </div>;
}

function SummaryCard({ label, value, hint, tone }: { label: string; value: number; hint: string; tone: string }) {
  return <div className={`change-card change-card-${value ? tone : 'neutral'}`}><span>{label}</span><strong>{value}</strong><small>{hint}</small></div>;
}

function Values({ field, side }: { field: FieldChange; side: 'before' | 'after' }) {
  const values = distinctValues(field.edits, side);
  return <div className="change-values">{values.length > 1 && <small className="muted">{values.length} distinct values</small>}{values.slice(0, 2).map((v, i) => <div key={i}><code title={v.text}>{v.text === '' ? '(empty string)' : v.text}</code>{field.op === 'type_changed' && v.type && <span className="change-type">{v.type}</span>}</div>)}{values.length > 2 && <small className="muted">+{values.length - 2} more · expand field</small>}</div>;
}

function FieldRow({ field: f, onRecord, onFollow }: { field: FieldChange } & Pick<Props, 'onRecord' | 'onFollow'>) {
  const [open, setOpen] = useState(false);
  const id = useId();
  const noun = f.target === 'resource' ? 'resource' : f.target === 'scope' ? 'scope' : 'record';
  const approximate = f.edits.some((e) => e.change.confidence === 'medium');
  return <>
    <tr className={`change-field change-op-${f.op}`}>
      <th scope="row"><button type="button" className="change-field-toggle" aria-expanded={open} aria-controls={open ? id : undefined} onClick={() => setOpen(!open)} title={f.path}><span aria-hidden="true">{open ? '▾' : '▸'}</span> {f.field}</button><small className="muted">{f.targetLabel}</small></th>
      <td><span className={`change-operation change-operation-${f.op}`}>{OP_LABEL[f.op] ?? f.op}</span></td>
      <td><Values field={f} side="before" /></td><td><Values field={f} side="after" /></td>
      <td className="change-affected"><strong>{f.entities}</strong> {noun}{f.entities === 1 ? '' : 's'}{f.target !== 'record' && <small className="muted">Shared by {f.sharedRecords} input records</small>}{approximate && <span className="conf conf-medium">Includes medium-confidence matches</span>}</td>
    </tr>
    {open && <tr><td colSpan={5} className="change-edit-cell"><EditDetails id={id} field={f} onRecord={onRecord} onFollow={onFollow} /></td></tr>}
  </>;
}

function EditDetails({ id, field, onRecord, onFollow }: { id: string; field: FieldChange } & Pick<Props, 'onRecord' | 'onFollow'>) {
  const [page, setPage] = useState(0);
  const pages = Math.max(1, Math.ceil(field.edits.length / PAGE_SIZE));
  const current = Math.min(page, pages - 1);
  return <div id={id} className="change-edit-details">
    <table className="change-edit-table"><caption>{field.field} · {field.edits.length} field edit{field.edits.length === 1 ? '' : 's'}</caption><thead><tr><th>{field.target === 'record' ? 'Record' : field.target === 'resource' ? 'Resource' : 'Scope'}</th><th>Before</th><th>After</th><th>Actions</th></tr></thead>
      <tbody>{field.edits.slice(current * PAGE_SIZE, (current + 1) * PAGE_SIZE).map((edit, i) => {
        const single = { ...field, edits: [edit] };
        const follow = field.target === 'record' ? onFollow?.(edit.change.entity) : undefined;
        return <tr key={i}><td><span title={edit.change.entity}>{edit.label}</span><small className="muted">{edit.context}</small>{edit.change.confidence && <span className={`conf conf-${edit.change.confidence}`}>{edit.change.confidence} match</span>}</td><td><Values field={single} side="before" /></td><td><Values field={single} side="after" /></td><td className="change-actions"><button type="button" onClick={() => onRecord(edit.change.entity)}>Select {field.target}</button>{follow && <button type="button" className="follow" onClick={follow}>Follow record</button>}</td></tr>;
      })}</tbody>
    </table>
    {pages > 1 && <Pagination current={current} pages={pages} onPage={setPage} label="Affected entities" />}
  </div>;
}

function RecordList({ records, kind, onRecord, onFollow }: { records: RecordChange[]; kind: RecordChange['kind'] } & Partial<Pick<Props, 'onRecord' | 'onFollow'>>) {
  const [page, setPage] = useState(0);
  const pages = Math.max(1, Math.ceil(records.length / PAGE_SIZE));
  const current = Math.min(page, pages - 1);
  if (!records.length) return null;
  return <section className={`change-record-list change-record-list-${kind}`} aria-label={kind === 'dropped' ? 'Dropped records' : 'New records'}>
    <h4>{kind === 'dropped' ? 'Dropped records' : 'New records'} <span>{records.length}</span></h4>
    <p className="muted">{kind === 'dropped' ? 'Present in the input; no counterpart in the output.' : 'Present in the output; no counterpart was matched in the input.'}</p>
    <ul>{records.slice(current * PAGE_SIZE, (current + 1) * PAGE_SIZE).map((r, i) => {
      const follow = onFollow?.(r.entity);
      return <li key={`${r.entity}:${i}`}><div><strong title={r.entity}>{r.label}</strong><small className="muted">{r.service}</small>{r.attributes.length > 0 && <div className="signal-tags">{r.attributes.slice(0, 3).map(([k, v], j) => <span key={j} title={`${k} = ${v}`}>{k} = {v}</span>)}</div>}</div><span className="change-actions">{onRecord && <button type="button" onClick={() => onRecord(r.entity)}>Select record</button>}{follow && <button type="button" className="follow" onClick={follow}>Follow record</button>}</span></li>;
    })}</ul>
    {pages > 1 && <Pagination current={current} pages={pages} onPage={setPage} label={kind === 'dropped' ? 'Dropped records' : 'New records'} />}
  </section>;
}

function Pagination({ current, pages, onPage, label }: { current: number; pages: number; onPage(n: number): void; label: string }) {
  return <div className="change-pagination" aria-label={label}><button type="button" disabled={current === 0} onClick={() => onPage(current - 1)}>Previous page</button><span>Page {current + 1} of {pages}</span><button type="button" disabled={current === pages - 1} onClick={() => onPage(current + 1)}>Next page</button></div>;
}
