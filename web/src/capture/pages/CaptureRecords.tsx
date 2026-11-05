import { Fragment, useEffect, useMemo, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../api/client';
import type { CaptureRecordsPage, CaptureSession, CaptureStream } from '../../api/types';
import { SignalDetails } from '../../inspect/SignalExplorer';
import { Icon } from '../../ui/Icon';
import { isReceiving, targetLabel } from '../detailView';
import { clockTime, formatCount } from '../format';
import {
  asSelection,
  clip,
  filterActive,
  filterRecords,
  NO_FILTER,
  recordKinds,
  SAMPLES,
  sampleRecords,
  selectionWarning,
  summarizeRecord,
  type RecordFilter,
  type SampleId,
  type ScannedRecord,
} from '../recordView';
import { useCaptureScan } from '../useCaptureScan';

type Item = NonNullable<CaptureRecordsPage['items']>[number];
type Pick = 'all' | SampleId;

const PAGE = 25;

/**
 * The records a capture received. Selection is held by the page so the "Use
 * records" panel can read it. Without a filter the list pages through the
 * server and refreshes itself only while records can still arrive, on its
 * first page, and with nothing selected (a refresh must not move records
 * under a selection). A filter, "select all" or a sample reads the whole
 * capture first, so they cover every record, not the page on screen.
 */
export function CaptureRecords({
  session,
  selected,
  onSelected,
  onWatermark,
  guide = true,
}: {
  session: CaptureSession;
  selected: Record<number, CaptureStream>;
  onSelected: (next: Record<number, CaptureStream>) => void;
  onWatermark: (watermark: number) => void;
  /** Say what to do next once the capture has finished (the capture page, not the input picker). */
  guide?: boolean;
}) {
  const client = useQueryClient();
  const [cursors, setCursors] = useState(['']);
  const cursor = cursors[cursors.length - 1];
  const [open, setOpen] = useState<number>();
  const [filter, setFilter] = useState<RecordFilter>(NO_FILTER);
  const [localPage, setLocalPage] = useState(0);
  const [pick, setPick] = useState<Pick>();
  const [picked, setPicked] = useState('');
  const [kindsWanted, setKindsWanted] = useState(false);
  const count = Object.keys(selected).length;
  const filtering = filterActive(filter);

  const records = useQuery({
    queryKey: ['capture-records', session.id, cursor],
    queryFn: () => api.captureRecords(session.id, cursor),
    enabled: session.data.enabled && !filtering,
    refetchInterval: isReceiving(session.data) && cursors.length === 1 && count === 0 ? 2000 : false,
    refetchIntervalInBackground: false,
  });
  const scan = useCaptureScan(session, filtering || !!pick || kindsWanted);
  const matches = useMemo(() => (scan.data ? filterRecords(scan.data.records, filter) : []), [scan.data, filter]);
  const kinds = useMemo(() => recordKinds(scan.data?.records ?? []), [scan.data]);

  const items: Item[] = filtering
    ? matches.slice(localPage * PAGE, (localPage + 1) * PAGE).map((r) => scan.data!.items.get(r.sequence)!)
    : records.data?.items ?? [];
  const watermark = Math.max(records.data?.watermark ?? 0, scan.data?.watermark ?? 0);
  useEffect(() => {
    if (watermark) onWatermark(watermark);
  }, [watermark, onWatermark]);

  // A requested pick applies once the whole capture has been read.
  useEffect(() => {
    if (!pick || !scan.data) return;
    const from = filtering ? matches : scan.data.records;
    const chosen = pick === 'all' ? from : sampleRecords(from, pick);
    onSelected(asSelection(chosen));
    setPicked(pick === 'all' ? `Selected all ${formatCount(chosen.length)}${filtering ? ' matching' : ''} records.` : `Selected ${formatCount(chosen.length)} records: ${SAMPLES.find((s) => s.id === pick)!.label.toLowerCase()}${filtering ? ' of the matching records' : ''}.`);
    setPick(undefined);
  }, [pick, scan.data, filtering, matches, onSelected]);

  const selectedBytes = useMemo(() => {
    if (!scan.data) return undefined;
    const bytes = new Map(scan.data.records.map((r: ScannedRecord) => [r.sequence, r.bytes]));
    return Object.keys(selected).reduce((n, s) => n + (bytes.get(Number(s)) ?? 0), 0);
  }, [scan.data, selected]);
  const warning = selectionWarning(count, selectedBytes);

  const label = (stream: CaptureStream) => {
    const target = session.targets?.find((t) => t.collector_id === stream.collector_id);
    return target ? targetLabel(target) : stream.collector_id || 'Unknown Collector';
  };
  const allOnPage = items.length > 0 && items.every((i) => selected[i.sequence]);
  function toggle(item: Item) {
    const next = { ...selected };
    if (next[item.sequence]) delete next[item.sequence];
    else next[item.sequence] = item.stream;
    setPicked('');
    onSelected(next);
  }
  function togglePage() {
    const next = { ...selected };
    for (const i of items) {
      if (allOnPage) delete next[i.sequence];
      else next[i.sequence] = i.stream;
    }
    setPicked('');
    onSelected(next);
  }
  function changeFilter(next: RecordFilter) {
    setFilter(next);
    setLocalPage(0);
    setOpen(undefined);
  }
  function refresh() {
    setCursors(['']);
    onSelected({});
    setOpen(undefined);
    setPicked('');
    void client.resetQueries({ queryKey: ['capture-records', session.id] });
    void client.resetQueries({ queryKey: ['capture-scan', session.id] });
  }

  const total = filtering ? matches.length : session.data.received_records;
  const scanning = (filtering || !!pick) && scan.isFetching;
  const pages = Math.max(1, Math.ceil(matches.length / PAGE));
  const hasRecords = session.data.received_records > 0;
  const nextSteps = guide && session.terminal && hasRecords && count === 0 && !filtering;

  return (
    <section className="od-card" style={{ overflow: 'hidden' }} aria-label="Received records">
      <div className="od-card__head" style={{ flexWrap: 'wrap', gap: 8 }}>
        <h2 className="od-card__title" style={{ margin: 0 }}>Records</h2>
        <span className="od-subtle" style={{ fontSize: 12 }}>{isReceiving(session.data) ? 'arriving live' : 'received'}</span>
        <span style={{ flex: 1 }} />
        <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={refresh}>
          <Icon name="rotate-ccw" size={14} />
          <span>{count ? 'Clear selection and refresh' : 'Refresh'}</span>
        </button>
      </div>

      {nextSteps && (
        <div className="od-callout od-callout--info od-callout--bar" role="note" aria-label="Next steps">
          <Icon name="info" size={16} />
          <div className="od-callout__main">
            <span className="od-callout__title">Next: turn these records into a scenario</span>
            <div className="od-callout__body">
              {session.data.memory_only
                ? 'This capture is held in memory only and is lost when the debugger stops, so use its records soon. '
                : 'The server already stores the capture until you delete it, so there is nothing to save. '}
              Select the records to replay (all, the ones a filter finds, or a sample), then create a scenario from them and run it against a configuration.
            </div>
          </div>
          <div className="od-callout__actions">
            <button type="button" className="od-btn od-btn--sm" disabled={scanning} onClick={() => setPick('spread')}>Select a sample of 100</button>
            <button type="button" className="od-btn od-btn--sm" disabled={scanning} onClick={() => setPick('all')}>Select all {formatCount(session.data.received_records)}</button>
          </div>
        </div>
      )}

      {session.data.enabled && hasRecords && (
        <div role="search" aria-label="Filter records" style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 8, padding: '8px 14px', borderBottom: '1px solid var(--border-subtle)' }}>
          <input
            type="search"
            className="od-input"
            style={{ flex: '1 1 240px' }}
            placeholder="Filter all records: body, attributes, resource…"
            aria-label="Filter all records"
            value={filter.text}
            onChange={(e) => changeFilter({ ...filter, text: e.target.value })}
          />
          <select className="od-select" aria-label="Kind" value={filter.kind} onChange={(e) => changeFilter({ ...filter, kind: e.target.value })} onFocus={() => setKindsWanted(true)}>
            <option value="">All kinds</option>
            {kinds.map((k) => <option key={k.kind} value={k.kind}>{k.kind} ({formatCount(k.count)})</option>)}
            {filter.kind && !kinds.some((k) => k.kind === filter.kind) && <option value={filter.kind}>{filter.kind}</option>}
          </select>
          <button type="button" className="od-btn od-btn--sm" disabled={scanning || (filtering && !!scan.data && !matches.length)} onClick={() => setPick('all')}>
            {filtering && scan.data ? `Select ${formatCount(matches.length)} matching` : `Select all ${formatCount(total)}`}
          </button>
          <select
            className="od-select"
            aria-label="Select a sample"
            value=""
            disabled={scanning}
            onChange={(e) => e.target.value && setPick(e.target.value as SampleId)}
          >
            <option value="">Sample…</option>
            {SAMPLES.map((s) => <option key={s.id} value={s.id}>{s.label}</option>)}
          </select>
        </div>
      )}
      {scanning && <p role="status" className="od-muted" style={{ padding: '8px 14px', margin: 0 }}>Reading the whole capture… {formatCount(scan.loaded)} of {formatCount(session.data.received_records)} records</p>}
      {scan.error && (filtering || pick) && (
        <div className="od-callout od-callout--fail od-callout--bar" role="alert">
          <Icon name="circle-x" size={16} />
          <div className="od-callout__main">
            <span className="od-callout__title">Could not read the whole capture</span>
            <div className="od-callout__body">{scan.error.message}</div>
          </div>
          <div className="od-callout__actions"><button type="button" className="od-btn od-btn--sm" onClick={() => void scan.refetch()}>Try again</button></div>
        </div>
      )}
      {(picked || warning) && count > 0 && (
        <div className={`od-callout od-callout--bar ${warning ? 'od-callout--warn' : 'od-callout--info'}`} role="status">
          <Icon name={warning ? 'triangle-alert' : 'circle-check'} size={16} />
          <div className="od-callout__main">
            {picked && <span className="od-callout__title">{picked}</span>}
            {warning && <div className="od-callout__body">{warning}</div>}
          </div>
          {warning && <div className="od-callout__actions"><button type="button" className="od-btn od-btn--sm" disabled={scanning} onClick={() => setPick('spread')}>Take a sample of 100 instead</button></div>}
        </div>
      )}

      {!session.data.enabled && (
        <div className="od-empty"><div className="od-empty__title">No data ingress</div><div className="od-empty__body">Capture data ingress was not enabled for this capture, so no records can be received.</div></div>
      )}
      {session.data.enabled && !filtering && records.isPending && <p role="status" className="od-muted" style={{ padding: '10px 14px', margin: 0 }}>Loading records…</p>}
      {records.error && !filtering && (
        <div className="od-callout od-callout--fail od-callout--bar" role="alert">
          <Icon name="circle-x" size={16} />
          <div className="od-callout__main">
            <span className="od-callout__title">Could not read the records</span>
            <div className="od-callout__body">{records.error.message}. They may have expired or been deleted.</div>
          </div>
          <div className="od-callout__actions"><button type="button" className="od-btn od-btn--sm" onClick={() => void records.refetch()}>Try again</button></div>
        </div>
      )}
      {!filtering && records.data && !items.length && (
        <div className="od-empty">
          <div className="od-empty__title">{session.terminal ? 'No telemetry was received' : 'Waiting for telemetry'}</div>
          <div className="od-empty__body">{session.terminal ? 'Try another capture while telemetry is flowing through these Collectors.' : 'Matching records appear here as the Collectors deliver them.'}</div>
        </div>
      )}
      {filtering && scan.data && !matches.length && (
        <div className="od-empty">
          <div className="od-empty__title">No record matches</div>
          <div className="od-empty__body">None of the {formatCount(scan.data.records.length)} records matches this filter. <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={() => changeFilter(NO_FILTER)}>Clear the filter</button></div>
        </div>
      )}
      {items.length > 0 && (
        <table className="od-table">
          <thead>
            <tr>
              <th style={{ width: 28 }}>
                <input type="checkbox" aria-label={`Select the ${items.length} records on this page`} checked={allOnPage} onChange={togglePage} />
              </th>
              <th>Kind</th>
              <th>Body</th>
              <th>Data</th>
              <th>Collector</th>
              <th>Capture point</th>
              <th>Received</th>
              <th style={{ width: 28 }}><span className="sr-only">Inspect</span></th>
            </tr>
          </thead>
          <tbody>
            {items.map((item) => {
              const s = summarizeRecord(item.payload, item.sequence);
              const expanded = open === item.sequence;
              const flip = () => setOpen(expanded ? undefined : item.sequence);
              return (
                <Fragment key={item.sequence}>
                  <tr aria-selected={!!selected[item.sequence]} onClick={flip} style={{ cursor: 'pointer' }} className={expanded ? 'is-open' : undefined}>
                    <td onClick={(e) => e.stopPropagation()}><input type="checkbox" aria-label={`Select record ${item.sequence}`} checked={!!selected[item.sequence]} onChange={() => toggle(item)} /></td>
                    <td><span className={`od-chip${s.tone === 'error' ? ' od-chip--fail' : s.tone === 'warn' ? ' od-chip--warn' : ''}`}>{s.kind}</span></td>
                    <td className="mono" style={{ maxWidth: 420, overflow: 'hidden', textOverflow: 'ellipsis' }}>{item.oversize ? 'Payload omitted: over the page byte budget' : clip(s.text)}</td>
                    <td>{item.sensitivity === 'raw' ? <span className="od-chip od-chip--warn" title="Taken at a raw point: stored unredacted">Raw</span> : <span className="od-subtle">Redacted</span>}</td>
                    <td className="mono">{label(item.stream)}</td>
                    <td className="mono">{item.stream.pipeline} · {item.stream.point}</td>
                    <td className="od-subtle">{clockTime(item.received_at)}</td>
                    <td>
                      <button type="button" className="od-btn od-btn--ghost od-btn--sm" aria-expanded={expanded} aria-label={`${expanded ? 'Hide' : 'Inspect'} record ${item.sequence}`} onClick={(e) => { e.stopPropagation(); flip(); }}>
                        <Icon name={expanded ? 'chevron-down' : 'chevron-right'} size={14} />
                      </button>
                    </td>
                  </tr>
                  {expanded && (
                    <tr>
                      <td colSpan={8}>
                        {item.oversize
                          ? <p className="od-muted" style={{ margin: 0 }}>This record exceeds the page byte budget, so its payload is omitted. Select it to use it as an input.</p>
                          : <SignalDetails payload={item.payload} label={`Capture record ${item.sequence}`} />}
                      </td>
                    </tr>
                  )}
                </Fragment>
              );
            })}
          </tbody>
        </table>
      )}
      {!filtering && (cursors.length > 1 || records.data?.next_cursor) && (
        <div className="od-pager">
          <span>Page {cursors.length} · {PAGE} per page</span>
          <span style={{ display: 'flex', gap: 4 }}>
            <button type="button" className="od-btn od-btn--ghost od-btn--sm" disabled={cursors.length === 1} onClick={() => { setCursors((p) => p.slice(0, -1)); setOpen(undefined); }}>Previous records</button>
            <button type="button" className="od-btn od-btn--ghost od-btn--sm" disabled={!records.data?.next_cursor} onClick={() => { const n = records.data?.next_cursor; if (n) { setCursors((p) => [...p, n]); setOpen(undefined); } }}>Next records</button>
          </span>
        </div>
      )}
      {filtering && matches.length > PAGE && (
        <div className="od-pager">
          <span>{formatCount(matches.length)} matching · page {localPage + 1} of {pages}</span>
          <span style={{ display: 'flex', gap: 4 }}>
            <button type="button" className="od-btn od-btn--ghost od-btn--sm" disabled={localPage === 0} onClick={() => { setLocalPage((p) => p - 1); setOpen(undefined); }}>Previous records</button>
            <button type="button" className="od-btn od-btn--ghost od-btn--sm" disabled={localPage >= pages - 1} onClick={() => { setLocalPage((p) => p + 1); setOpen(undefined); }}>Next records</button>
          </span>
        </div>
      )}
    </section>
  );
}
