import { useMemo, useState } from 'react';
import type { Aggregation, RecordLineage } from '../api/types';
import { aggregationRows, type CountedRecord, type PointRow } from './aggregation';
import { RecordList, type RecordFilter } from './RecordList';
import { Pill, RecordRow } from './RecordRow';
import { pageOf } from './pages';
import { Pagination } from './Pagination';

type Item = { kind: 'point'; id: string; point: PointRow } | { kind: 'uncounted'; id: string; record: CountedRecord };
const filters: RecordFilter<Item>[] = [
  { id: 'all', label: 'All', match: () => true },
  { id: 'point', label: 'Data points', match: (i) => i.kind === 'point' },
  { id: 'uncounted', label: 'Not counted', match: (i) => i.kind === 'uncounted' },
];
const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? '' : 's'}`;

/**
 * AggregationList shows what an aggregating connector (count) made of its
 * input in the shared record list: a row per emitted data point, which opens
 * to the records it counted, and a row per record nothing counted. The
 * records of a point are known only when the replay verified the output.
 */
export function AggregationList({ a, lineage, follow }: { a: Aggregation; lineage?: RecordLineage[]; follow(lineage: string): void }) {
  const items = useMemo<Item[]>(() => {
    const { points, uncounted } = aggregationRows(a, lineage);
    return [
      ...points.map((point): Item => ({ kind: 'point', id: `point:${point.ref}`, point })),
      ...uncounted.map((record): Item => ({ kind: 'uncounted', id: `uncounted:${record.ref}`, record })),
    ];
  }, [a, lineage]);
  return <RecordList
    items={items}
    itemId={(i) => i.id}
    filters={filters}
    label="Data points and the records they counted"
    hint="see the records it counted"
    pageNoun="rows"
    defaultOpen
    empty={items.length ? undefined : 'This connector emitted no data points for this input.'}
    renderRow={(i, s) => i.kind === 'point'
      ? <RecordRow {...s} onToggle={s.toggle} glyph="Σ" glyphTone="route" metric={i.point.metric}
        title="" service={i.point.attributes.map(([k, v]) => `${k}=${v}`).join(' · ')}
        why={i.point.records.length ? `Counted from ${plural(i.point.records.length, 'record')}` : 'The records it counted are not attributed'}
        pills={<Pill tone="route">{i.point.value}</Pill>} />
      : <RecordRow {...s} onToggle={s.toggle} glyph="·" glyphTone="none" dim title={i.record.name}
        why="Not counted by any metric" pills={<Pill>not counted</Pill>} />}
    renderDetail={(i) => i.kind === 'point'
      ? <>
        {i.point.records.length
          ? <div className="rv-group"><div className="rv-group-head"><b>Counted records</b></div><CountedRecords records={i.point.records} follow={follow} /></div>
          : <p className="rv-foot">The replay did not reproduce the emitted data points, so no records are attributed to this one.</p>}
        {i.point.lineage && <div><button type="button" className="rv-follow" onClick={() => follow(i.point.lineage!)} aria-label={`Follow data point: ${i.point.metric}`}>Follow data point <span aria-hidden="true">→</span></button></div>}
      </>
      : <>
        <p className="rv-foot">No data point of this connector counted this record.</p>
        {i.record.lineage && <div><button type="button" className="rv-follow" onClick={() => follow(i.record.lineage!)} aria-label={`Follow record: ${i.record.name}`}>Follow record <span aria-hidden="true">→</span></button></div>}
      </>} />;
}

/** CountedRecords links the records a data point counted, 25 at a time. */
function CountedRecords({ records, follow }: { records: CountedRecord[]; follow(lineage: string): void }) {
  const [page, setPage] = useState(0);
  const p = pageOf(records, page);
  return <>
    <div className="journey-sources">{p.shown.map((r) => r.lineage
      ? <button key={r.ref} type="button" className="link chip" title={`Follow this record · ${r.ref}`} onClick={() => follow(r.lineage!)}>{r.name}</button>
      : <code key={r.ref} className="small">{r.name}</code>)}</div>
    <Pagination p={p} total={records.length} noun="counted records" setPage={setPage} />
  </>;
}
