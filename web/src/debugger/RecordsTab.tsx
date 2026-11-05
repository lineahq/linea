import { useMemo, useState } from 'react';
import type { WorkspaceSearch } from '../router';
import type { RunView } from '../runs/useRun';
import { JourneyPanel } from '../inspect/JourneyPanel';
import { pageOf } from '../inspect/pages';
import { Icon } from '../ui/Icon';
import { FATE_ICONS, FILTER_LABELS, filterCounts, filterRows, recordRows, stopNode, type RecordFilter } from './recordList';

/**
 * RecordsTab is the Records tab of the Investigate left pane: one row per
 * record journey with where it ended. Choosing a row follows the record
 * (`trace` in the URL), which dims the graph off its path and opens the
 * component where it stopped; its hop-by-hop journey is listed below.
 */
export function RecordsTab({ view, search, setSearch }: { view: RunView; search: WorkspaceSearch; setSearch(patch: Partial<WorkspaceSearch>): void }) {
  const lineage = view.run?.report?.lineage;
  const rows = useMemo(() => recordRows(lineage), [lineage]);
  const [filter, setFilter] = useState<RecordFilter>('all');
  const [query, setQuery] = useState('');
  const shown = useMemo(() => filterRows(rows, filter, query), [rows, filter, query]);
  // Open on the page that holds the followed record.
  const [page, setPage] = useState(() => Math.max(0, Math.floor(shown.findIndex((r) => r.id === search.trace) / 25)));
  if (view.live) return <div className="od-muted" style={{ padding: 14 }} role="status">Records are available when the run finishes.</div>;
  if (!lineage?.length) return <div className="od-muted" style={{ padding: 14 }} role="status">This run has no records to follow.</div>;

  const p = pageOf(shown, page);
  const follow = (id: string) => {
    const l = lineage.find((x) => x.id === id);
    const node = l && stopNode(l, view.events);
    setSearch({ trace: id, ...(node ? { node } : {}), input: undefined, record: undefined });
  };
  const followed = search.trace && lineage.some((l) => l.id === search.trace);

  return (
    <div>
      <div style={{ padding: '10px 12px', display: 'flex', flexDirection: 'column', gap: 8, borderBottom: '1px solid var(--border-subtle)' }}>
        <div className="od-search">
          <Icon name="search" size={14} />
          <input
            type="search"
            aria-label="Search records"
            placeholder="Name, signal, exporter, stage"
            value={query}
            onChange={(e) => { setQuery(e.target.value); setPage(0); }}
          />
        </div>
        <div role="group" aria-label="Filter records by fate" style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
          {filterCounts(rows).map((c) => (
            <button
              key={c.filter}
              type="button"
              aria-pressed={filter === c.filter}
              className={`od-chip od-chip--plain${filter === c.filter ? ' od-chip--accent' : ''}`}
              style={{ cursor: 'pointer' }}
              onClick={() => { setFilter(c.filter); setPage(0); }}
            >
              {FILTER_LABELS[c.filter]} {c.count}
            </button>
          ))}
        </div>
      </div>
      <ul role="listbox" aria-label="Record journeys" style={{ listStyle: 'none', margin: 0, padding: 0 }}>
        {p.shown.map((r) => (
          <li
            key={r.id}
            role="option"
            tabIndex={0}
            className="od-rec"
            aria-selected={r.id === search.trace}
            style={{ gridTemplateColumns: '16px minmax(0, 1fr)', cursor: 'pointer' }}
            onClick={() => follow(r.id)}
            onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); follow(r.id); } }}
          >
            <span className={`od-dot${r.fate === 'dropped' ? ' od-dot--dropped' : ''}`} />
            <span style={{ minWidth: 0 }}>
              <span className="od-rec__body" style={{ display: 'block' }} title={r.name}>{r.aggregated ? 'Σ ' : ''}{r.name}</span>
              <span className="od-rec__fate"><Icon name={FATE_ICONS[r.fate]} size={12} />{r.text}</span>
            </span>
          </li>
        ))}
      </ul>
      {!shown.length && <div className="od-muted" style={{ padding: 14, fontSize: 12.5 }}>No record journeys match.</div>}
      <div className="od-pager">
        <span>{shown.length ? `${p.start + 1}–${p.start + p.shown.length} of ${shown.length}` : `0 of ${rows.length}`}</span>
        {p.pages > 1 && (
          <span style={{ display: 'inline-flex', gap: 6, alignItems: 'center' }}>
            <button type="button" className="od-btn od-btn--ghost od-btn--sm" disabled={p.page === 0} onClick={() => setPage(p.page - 1)}>Previous records</button>
            <span>Page {p.page + 1} of {p.pages}</span>
            <button type="button" className="od-btn od-btn--ghost od-btn--sm" disabled={p.page + 1 >= p.pages} onClick={() => setPage(p.page + 1)}>Next records</button>
          </span>
        )}
      </div>
      {followed && (
        <section aria-label="Followed record" style={{ borderTop: '1px solid var(--border-subtle)' }}>
          <JourneyPanel lineage={lineage} events={view.events} live={view.live} search={search} setSearch={setSearch} />
        </section>
      )}
    </div>
  );
}
