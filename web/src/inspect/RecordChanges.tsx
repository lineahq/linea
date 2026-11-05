import { useMemo, useState } from 'react';
import type { Diff, OTTLReport } from '../api/types';
import { FieldTable } from './FieldTable';
import { RawJSONDiff } from './RawJSONDiff';
import { RecordList, type RecordFilter } from './RecordList';
import { Pill, RecordRow, type GlyphTone } from './RecordRow';
import { LEVEL_LABELS, recordColumns, recordViews, type RecordState, type RecordView } from './recordViews';
import { SignalExplorer } from './SignalExplorer';

type FollowRecord = (entity: string, side: 'before' | 'after') => (() => void) | undefined;

interface Props {
  diff?: Diff;
  before: unknown;
  after?: unknown;
  onFollow?: FollowRecord;
  /** The record selected from elsewhere, for example a journey. */
  record?: string;
  onClearRecord?(): void;
  afterMissingReason?: string;
  /** The transform replay for this input, which adds a statement to each change. */
  report?: OTTLReport;
  onOpenStatements?(): void;
}

const GLYPHS: Record<RecordState, [string, GlyphTone]> = { edited: ['~', 'mod'], dropped: ['−', 'drop'], new: ['+', 'keep'], unchanged: ['·', 'none'], recorded: ['·', 'none'] };
const FILTERS: [RecordState | 'all', string][] = [['all', 'All'], ['edited', 'Changed'], ['dropped', 'Dropped'], ['new', 'New'], ['unchanged', 'Unchanged']];
const filters: RecordFilter<RecordView>[] = FILTERS.map(([id, label]) => ({ id, label, match: (r) => id === 'all' || r.state === id }));
const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? '' : 's'}`;
const focusMatches = (r: RecordView, focus: string) => r.entity === focus || r.entity.startsWith(`${focus}.`);

/** The levels in a sentence: "resource and record". */
function where(levels: RecordView['levels']): string {
  const names = levels.map((l) => LEVEL_LABELS[l].toLowerCase());
  return names.length < 2 ? names.join('') : `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}`;
}

/**
 * RecordChanges answers what a stage did to each record. Every record of the
 * stage input is one row; a row opens to its fields grouped by where they
 * live, with before and after values and the OTTL path of each field.
 */
export function RecordChanges({ diff, before, after, onFollow, record, onClearRecord, afterMissingReason, report, onOpenStatements }: Props) {
  const views = useMemo(() => recordViews(diff, before, after, report), [diff, before, after, report]);
  const sharedBy = useMemo(() => {
    const count = new Map<string, number>();
    for (const r of views?.records ?? []) for (const e of [r.resourceEntity, r.scopeEntity]) count.set(e, (count.get(e) ?? 0) + 1);
    return count;
  }, [views]);
  // Matching totals include explicitly reported additions and removals. Only
  // the remainder is uncertain; a confirmed drop must not also be called unknown.
  const unmatchedBefore = Math.max(0, (diff?.matching?.unmatched_before ?? 0) - (diff?.removed?.length ?? 0));
  const unmatchedAfter = Math.max(0, (diff?.matching?.unmatched_after ?? 0) - (diff?.added?.length ?? 0));
  const incomplete = unmatchedBefore > 0 || unmatchedAfter > 0;

  if (!diff) {
    const side = (which: 'before' | 'after') => views.records.filter((r) => r.side === which);
    return <div className="rv-changes">
      <p className="rv-notice">No before/after comparison is available for this input. Its output may have been buffered or could not be attributed.</p>
      <h3 className="rv-section">Input</h3>
      <RecordedList label="Input records" items={side('before')} record={record} onClearRecord={onClearRecord} onFollow={onFollow} empty="This input contained no records." />
      <h3 className="rv-section">Output</h3>
      <RecordedList label="Output records" items={side('after')} onFollow={onFollow} empty={afterMissingReason ?? 'No output was recorded for this input.'} />
      <RawJSONDiff before={before} after={after} afterMissingReason={afterMissingReason} />
    </div>;
  }
  return <div className="rv-changes">
    {incomplete && <p className="rv-notice">Some records could not be matched: {unmatchedBefore} before, {unmatchedAfter} after. Their changes are unknown, not drops or additions.</p>}
    {diff.identity === 'unresolved' && !incomplete && !diff.unresolved?.length && <p className="rv-notice">Record identity could not be resolved. Only established changes are shown.</p>}
    {(diff.unresolved ?? []).map((note, i) => <p key={i} className="rv-notice">{note}</p>)}
    {views.unplaced.length > 0 && <p className="rv-notice">{plural(views.unplaced.length, 'change')} could not be placed on a recorded record. See the raw JSON diff.</p>}
    <RecordList
      items={views.records}
      itemId={(r) => r.id}
      filters={filters}
      label="Records and what changed"
      columns={{ ...recordColumns(views.records[0]?.signal), service: 'Service', result: 'Change' }}
      hint="see what changed and where it lives"
      pageNoun="changes"
      defaultOpen={!record}
      focus={record}
      focusMatches={focusMatches}
      onClearFocus={onClearRecord}
      empty={views.records.length ? undefined : 'This input contained no records.'}
      renderRow={(r, s) => {
        const [glyph, tone] = GLYPHS[r.state];
        return <RecordRow {...s} onToggle={s.toggle} glyph={glyph} glyphTone={tone} badge={r.badge} badgeTone={r.tone} title={r.title} service={r.service}
          dim={r.state === 'dropped'} dataRecord={r.entity}
          why={r.state === 'edited' ? `Changed in ${where(r.levels)}${r.confidence ? ` · ${r.confidence}-confidence match` : ''}`
            : r.state === 'dropped' ? 'Removed by this processor' : r.state === 'new' ? 'Created by this processor' : incomplete ? 'No change found' : 'No change'}
          pills={r.state === 'edited' ? <Pill tone="mod">{plural(r.changed, 'change')}</Pill>
            : r.state === 'dropped' ? <Pill tone="drop">dropped</Pill> : r.state === 'new' ? <Pill tone="keep">new</Pill> : <Pill>{incomplete ? 'no change found' : 'unchanged'}</Pill>} />;
      }}
      renderDetail={(r) => <RecordDetail r={r} sharedBy={sharedBy} report={report} onFollow={onFollow} onOpenStatements={onOpenStatements} />}
    />
    {after !== undefined && <details className="rv-recorded"><summary>Recorded output</summary><SignalExplorer simple payload={after} label="Output" /></details>}
    <RawJSONDiff before={before} after={after} afterMissingReason={afterMissingReason} />
  </div>;
}

const recordedFilters: RecordFilter<RecordView>[] = [{ id: 'all', label: 'All', match: () => true }];

/** RecordedList lists records of one snapshot without claiming anything about changes. */
function RecordedList({ label, items, record, onClearRecord, onFollow, empty }: {
  label: string; items: RecordView[]; record?: string; onClearRecord?(): void; onFollow?: FollowRecord; empty: string;
}) {
  return <RecordList items={items} itemId={(r) => r.id} filters={recordedFilters} label={label} columns={{ ...recordColumns(items[0]?.signal), service: 'Service' }} hint="see its fields" pageNoun="records" empty={empty}
    focus={record} focusMatches={focusMatches} onClearFocus={onClearRecord}
    renderRow={(r, s) => <RecordRow {...s} onToggle={s.toggle} glyph="·" glyphTone="none" badge={r.badge} badgeTone={r.tone} title={r.title} service={r.service} dataRecord={r.entity} />}
    renderDetail={(r) => {
      const follow = onFollow?.(r.entity, r.side);
      return <>
        <FieldTable fields={r.fields} showAll single />
        {follow && <div><button type="button" className="rv-follow" onClick={follow} aria-label={`Follow record: ${r.title}`}>Follow record <span aria-hidden="true">→</span></button></div>}
      </>;
    }} />;
}

function RecordDetail({ r, sharedBy, report, onFollow, onOpenStatements }: {
  r: RecordView; sharedBy: Map<string, number>; report?: OTTLReport; onFollow?: FollowRecord; onOpenStatements?(): void;
}) {
  const [all, setAll] = useState(false);
  const follow = onFollow?.(r.entity, r.side);
  const touched = new Set(r.fields.flatMap((f) => f.by?.split(', ') ?? []));
  const flat = (report?.groups ?? []).flatMap((g) => (g.statements ?? []).map((s) => ({ label: (report?.groups?.length ?? 0) > 1 ? `${g.index}.${s.index}` : `S${s.index}`, text: s.text })));
  const editable = r.state === 'edited' || r.state === 'unchanged';
  const hidden = r.fields.filter((f) => f.op === 'same').length;
  return <>
    {r.state === 'unchanged' && !all
      ? <p className="rv-foot">This processor left the record as it arrived.</p>
      : <FieldTable fields={r.fields} showAll={all || !editable} withBy={!!report}
        shared={{ resource: sharedBy.get(r.resourceEntity), scope: sharedBy.get(r.scopeEntity) }} />}
    {editable && hidden > 0 && <button type="button" className="rv-more" onClick={() => setAll(!all)}>{all ? 'Show changed fields only' : `Show all ${r.fields.length} fields`}</button>}
    {report && flat.length > 0 && r.state === 'edited' && <div className="rv-statements">
      <h4>Statements{onOpenStatements && <button type="button" className="link" onClick={onOpenStatements}>Edit statements</button>}</h4>
      <ul>{flat.map((s) => <li key={s.label} className={touched.has(s.label) ? 'rv-applied' : 'rv-off'}>
        <span aria-hidden="true">{touched.has(s.label) ? '✓' : '–'}</span><span className="rv-sn">{s.label}</span><code>{s.text}</code>
        <span className="sr-only">{touched.has(s.label) ? 'changed this record' : 'did not change this record'}</span></li>)}</ul>
    </div>}
    {follow && <div><button type="button" className="rv-follow" onClick={follow} aria-label={`Follow record: ${r.title}`}>Follow record <span aria-hidden="true">→</span></button></div>}
  </>;
}
