import { Fragment, useId, useMemo, useState } from 'react';
import { signalOf } from './records';
import { filterSignalRows, requestRows, signalRows, type Fields, type SignalFilters, type SignalRow } from './signals';

interface Props {
  payload: unknown;
  /**
   * Several payloads of one signal, in order, shown as one list with each
   * record's request; replaces payload.
   */
  requests?: readonly unknown[];
  /** The request the record is in, when requests are given. */
  recordRequest?: number;
  label?: string;
  record?: string;
  onRecord?(entity: string | undefined): void;
  /** Clear external focus without introducing a second record-selection UI. */
  onClearRecord?(): void;
  /** request is the record's request, from 1, when requests are given. */
  onFollow?(entity: string, request?: number): (() => void) | undefined;
  emptyMessage?: string;
  /** Plain browsing: search and record details, without a secondary filter toolbar. */
  simple?: boolean;
}

const PAGE_SIZE = 25;

/** A reusable, read-only backend-style view of the existing OTLP snapshots. */
export function SignalExplorer({ payload, requests, recordRequest, label = 'Telemetry', record, onRecord, onClearRecord, onFollow, emptyMessage, simple = false }: Props) {
  const signal = signalOf(requests ? requests[0] : payload);
  const rows = useMemo(() => (requests ? requestRows(requests) : signalRows(payload)), [requests, payload]);
  const many = (requests?.length ?? 0) > 1;
  const [request, setRequest] = useState(0);
  const [query, setQuery] = useState('');
  const [service, setService] = useState('');
  const [category, setCategory] = useState('');
  const [sort, setSort] = useState<SignalFilters['sort']>('recorded');
  const [page, setPage] = useState(0);
  const [expanded, setExpanded] = useState<string>();
  const [showFilters, setShowFilters] = useState(false);
  const id = useId();
  const services = useMemo(() => [...new Set(rows.map((r) => r.service))].sort(), [rows]);
  const categories = useMemo(() => [...new Set(rows.map((r) => r.category))].sort(), [rows]);
  const matches = useMemo(() => filterSignalRows(rows, { query, service, category, sort, record, request: record && recordRequest ? recordRequest : request || undefined }),
    [rows, query, service, category, sort, record, recordRequest, request]);
  const pages = Math.max(1, Math.ceil(matches.length / PAGE_SIZE));
  const currentPage = Math.min(page, pages - 1);
  const start = currentPage * PAGE_SIZE;
  const noun = signal === 'logs' ? 'log records' : signal === 'traces' ? 'spans' : rows.some((r) => r.entity === r.recordEntity) ? 'metric entries' : 'data points';
  const categoryLabel = signal === 'logs' ? 'Severity' : signal === 'traces' ? 'Status' : 'Metric type';
  const clear = () => { setQuery(''); setService(''); setCategory(''); setRequest(0); setPage(0); };
  const columns = (signal === 'logs' ? 4 : 5) + (many ? 1 : 0);

  return (
    <section className="signal-explorer" aria-label={label}>
      <div className="signal-heading">
        <strong>{label}</strong>
        {signal && <span className={`signal-kind signal-${signal}`}>{signal === 'traces' ? 'Traces' : signal === 'logs' ? 'Logs' : 'Metrics'}</span>}
        <span className="muted">{rows.length} {noun}{many && ` in ${requests!.length} requests`} · {services.length} service{services.length === 1 ? '' : 's'}</span>
      </div>
      {!signal || !rows.length ? (
        <p className="signal-empty muted">{emptyMessage ?? (payload === undefined ? 'No telemetry observed here.' : signal ? 'This payload contains no records.' : 'Choose JSON to edit or inspect an OTLP payload.')}</p>
      ) : (
        <>
          <div className="signal-toolbar">
            <label className="signal-search">
              <span className="sr-only">Search {label}</span>
              <input type="search" placeholder="Search messages, IDs, attributes…" value={query} onChange={(e) => { setQuery(e.target.value); setPage(0); }} />
            </label>
            {many && <label><span className="sr-only">Request in {label}</span>
              <select value={request} onChange={(e) => { setRequest(Number(e.target.value)); setPage(0); }}>
                <option value={0}>All requests</option>
                {requests!.map((_, i) => <option key={i} value={i + 1}>Request {i + 1} · {rows.filter((r) => r.request === i + 1).length} {noun}</option>)}
              </select>
            </label>}
            {!simple && <button type="button" aria-expanded={showFilters} aria-controls={`${id}-filters`} onClick={() => setShowFilters(!showFilters)}>Filters & order{service || category || sort !== 'recorded' ? ' · active' : ''}</button>}
          </div>
          <div className="signal-toolbar" id={`${id}-filters`} hidden={simple || !showFilters}>
            <label><span className="sr-only">Service in {label}</span>
              <select value={service} onChange={(e) => { setService(e.target.value); setPage(0); }}>
                <option value="">All services</option>
                {service && !services.includes(service) && <option value={service}>{service} (not in this payload)</option>}
                {services.map((s) => <option key={s}>{s}</option>)}
              </select>
            </label>
            <label><span className="sr-only">{categoryLabel} in {label}</span>
              <select value={category} onChange={(e) => { setCategory(e.target.value); setPage(0); }}>
                <option value="">{signal === 'logs' ? 'All severities' : signal === 'traces' ? 'All statuses' : 'All metric types'}</option>
                {category && !categories.includes(category) && <option value={category}>{category} (not in this payload)</option>}
                {categories.map((c) => <option key={c}>{c}</option>)}
              </select>
            </label>
            <label><span className="sr-only">Order in {label}</span>
              <select value={sort} onChange={(e) => { setSort(e.target.value as SignalFilters['sort']); setPage(0); }}>
                <option value="recorded">Recorded order</option><option value="newest">Newest first</option><option value="oldest">Oldest first</option>
              </select>
            </label>
          </div>
          <div className="signal-results" role="status">
            <span>{matches.length} of {rows.length} {noun}{record ? ' · selected record' : ''}</span>
            {(query || service || category || request > 0) && <button type="button" className="link" onClick={clear}>Clear filters</button>}
            {record && (onRecord || onClearRecord) && <button type="button" className="link" onClick={() => onClearRecord ? onClearRecord() : onRecord?.(undefined)}>Show all records</button>}
            <span className="muted">Times in UTC</span>
          </div>
          {!matches.length ? <p className="signal-empty muted">{record && !rows.some((r) => r.entity === record || r.recordEntity === record || r.recordEntity.startsWith(`${record}.`)) ? 'The selected record is not present in this payload.' : 'No records match this selection. Try clearing the filters.'}</p> : (
            <div className="signal-table-wrap">
              <table className="signal-table">
                <caption className="sr-only">{label} {noun}</caption>
                <thead><tr>
                  {many && <th className="signal-request">Request</th>}
                  <th>Time (UTC)</th><th>{categoryLabel}</th><th>Service</th>
                  <th>{signal === 'logs' ? 'Message' : signal === 'traces' ? 'Operation' : 'Metric'}</th>
                  {signal !== 'logs' && <th>{signal === 'traces' ? 'Duration' : 'Value'}</th>}
                </tr></thead>
                <tbody>{matches.slice(start, start + PAGE_SIZE).map((r, i) => {
                  const open = expanded === r.id;
                  const detailId = `${id}-detail-${i}`;
                  return <Fragment key={r.id}>
                    <tr className={open ? 'signal-row expanded' : 'signal-row'}>
                      {many && <td className="signal-request">{r.request}</td>}
                      <td className="signal-time" title={r.time}>{r.time === '—' ? '—' : <><span>{r.time.split('T')[0]}</span><span>{r.time.split('T')[1]?.slice(0, 12)}</span></>}</td>
                      <td><span className={`signal-badge tone-${r.tone}`}>{r.badge}</span></td>
                      <td className="signal-service">{r.service}</td>
                      <td className="signal-message"><button type="button" className="signal-expand" aria-expanded={open} aria-controls={open ? detailId : undefined} onClick={() => setExpanded(open ? undefined : r.id)}><span aria-hidden="true">{open ? '▾' : '▸'}</span> {r.title}</button>
                        {r.attributes.length > 0 && <div className="signal-tags">{r.attributes.slice(0, 3).map(([k, v], j) => <span key={j} title={`${k} = ${v}`}><span className="muted">{k}</span> = {v}</span>)}{r.attributes.length > 3 && <span className="muted">+{r.attributes.length - 3}</span>}</div>}
                      </td>
                      {signal !== 'logs' && <td className="signal-value">{r.value}{r.unit && r.unit !== '1' && <span className="muted"> {r.unit}</span>}</td>}
                    </tr>
                    {open && <tr><td colSpan={columns} className="signal-detail-cell"><div id={detailId} className="signal-detail">
                      <div className="signal-detail-actions">
                        <strong>{signal === 'logs' ? 'Log details' : signal === 'traces' ? 'Span details' : 'Data point details'}</strong>
                        {onRecord && <button type="button" onClick={() => onRecord(record === r.entity || record === r.recordEntity ? undefined : r.recordEntity)}>{record === r.entity || record === r.recordEntity ? 'Show all records' : 'Select record in inspector'}</button>}
                        <FollowAction entity={r.entity} request={r.request} onFollow={onFollow} />
                      </div>
                      <div className="signal-fields-grid">
                        <FieldList title="Fields" fields={many ? [['Request', `${r.request} of ${requests!.length}`], ...r.fields] : r.fields} />
                        <FieldList title="Attributes" fields={r.attributes} />
                        <FieldList title="Resource" fields={r.resource} />
                        <FieldList title="Instrumentation scope" fields={r.scope} />
                      </div>
                      <details className="signal-original"><summary>Original JSON</summary><pre className="code">{JSON.stringify(r.raw, null, 2)}</pre></details>
                    </div></td></tr>}
                  </Fragment>;
                })}</tbody>
              </table>
            </div>
          )}
          {pages > 1 && <div className="signal-pagination">
            <span>{start + 1}–{Math.min(start + PAGE_SIZE, matches.length)} of {matches.length}</span>
            <button type="button" disabled={currentPage === 0} onClick={() => setPage(currentPage - 1)}>Previous page</button>
            <span>Page {currentPage + 1} of {pages}</span>
            <button type="button" disabled={currentPage === pages - 1} onClick={() => setPage(currentPage + 1)}>Next page</button>
          </div>}
        </>
      )}
    </section>
  );
}

function FieldList({ title, fields }: { title: string; fields: Fields }) {
  return <section className="signal-fields"><h4>{title}</h4>{fields.length ? <dl>{fields.map(([k, v], i) => <Fragment key={i}><dt>{k}</dt><dd>{v}</dd></Fragment>)}</dl> : <p className="muted">None</p>}</section>;
}

function FollowAction({ entity, request, onFollow }: Pick<Props, 'onFollow'> & { entity: SignalRow['entity']; request?: number }) {
  const follow = onFollow?.(entity, request);
  return follow ? <button type="button" className="follow" onClick={follow}>Follow record</button> : null;
}

const DETAIL_LIMIT = 25;

/**
 * SignalDetails shows a payload's records already opened: no search or list
 * to click through first. For one received record, where a list of one would
 * only add a click.
 */
export function SignalDetails({ payload, label = 'Telemetry' }: { payload: unknown; label?: string }) {
  const signal = signalOf(payload);
  const rows = useMemo(() => signalRows(payload), [payload]);
  const noun = signal === 'logs' ? 'Log' : signal === 'traces' ? 'Span' : 'Data point';
  if (!signal || !rows.length) return <p className="signal-empty muted">{signal ? 'This payload contains no records.' : 'This payload is not OTLP telemetry.'}</p>;
  return (
    <section className="signal-explorer signal-details" aria-label={label}>
      {rows.slice(0, DETAIL_LIMIT).map((r) => (
        <div key={r.id} className="signal-detail">
          <div className="signal-detail-actions">
            <span className={`signal-badge tone-${r.tone}`}>{r.badge}</span>
            <strong>{rows.length > 1 ? `${noun}: ${r.title}` : r.title}</strong>
            {signal !== 'logs' && r.value && <span>{r.value}{r.unit && r.unit !== '1' && <span className="muted"> {r.unit}</span>}</span>}
            <span className="muted">{r.service} · {r.time === '—' ? 'no time' : `${r.time.replace('T', ' ').slice(0, 23)} UTC`}</span>
          </div>
          <div className="signal-fields-grid">
            <FieldList title="Fields" fields={r.fields} />
            <FieldList title="Attributes" fields={r.attributes} />
            <FieldList title="Resource" fields={r.resource} />
            <FieldList title="Instrumentation scope" fields={r.scope} />
          </div>
          <details className="signal-original"><summary>Original JSON</summary><pre className="code">{JSON.stringify(r.raw, null, 2)}</pre></details>
        </div>
      ))}
      {rows.length > DETAIL_LIMIT && <p className="muted">Showing the first {DETAIL_LIMIT} of {rows.length} {noun.toLowerCase()}s in this record.</p>}
    </section>
  );
}
