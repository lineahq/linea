import { createContext, useEffect, useId, useMemo, useState, type CSSProperties, type ReactNode } from 'react';

/**
 * Column headings for a list whose rows have separate cells. `badge` and
 * `service` and `result` are optional columns; the record column always exists.
 */
export interface RecordColumns {
  badge?: string;
  record: string;
  service?: string;
  result?: string;
}

/** Tells a row which optional cells the surrounding list lays out as columns. */
export const ColumnsContext = createContext<RecordColumns | undefined>(undefined);

export interface RecordFilter<T> {
  id: string;
  label: string;
  match(item: T): boolean;
}

export interface RowState {
  open: boolean;
  selected: boolean;
  panelId: string;
  toggle(): void;
}

interface Props<T> {
  items: T[];
  itemId(item: T): string;
  /** The first filter is the default and should match everything. */
  filters: RecordFilter<T>[];
  /** Completes "Select a record to …". */
  hint: string;
  label: string;
  /** Show a heading row and give the row's badge and service their own columns. */
  columns?: RecordColumns;
  renderRow(item: T, state: RowState): ReactNode;
  renderDetail(item: T): ReactNode;
  /** An externally selected record, for example from a journey. */
  focus?: string;
  focusMatches?(item: T, focus: string): boolean;
  onClearFocus?(): void;
  defaultOpen?: boolean;
  pageSize?: number;
  /** Completes "Previous …" and "Next …" on the pagination buttons. */
  pageNoun?: string;
  empty?: ReactNode;
}

/** One grid shared by the heading and every row, so the headings sit over their cells. */
function columnStyle(c: RecordColumns): CSSProperties {
  const cols = ['24px', ...(c.badge ? ['76px'] : []), 'minmax(0, 1fr)', ...(c.service ? ['minmax(70px, 150px)'] : []), ...(c.result ? ['minmax(96px, 150px)'] : []), '18px'];
  return { '--rv-cols': cols.join(' ') } as CSSProperties;
}

/**
 * RecordList is the frame of every telemetry view: filter pills with counts,
 * a list of records where one is open at a time, and pagination. The
 * component supplies the row and the evidence; the frame never changes.
 */
export function RecordList<T>(p: Props<T>) {
  const { items, itemId, filters, focus, focusMatches } = p;
  const pageSize = p.pageSize ?? 25;
  const listId = useId();
  const focused = useMemo(() => (focus && focusMatches ? items.find((i) => focusMatches(i, focus)) : undefined), [items, focus, focusMatches]);
  const [filterId, setFilterId] = useState(filters[0].id);
  const [open, setOpen] = useState<string | undefined>(() => focused ? itemId(focused) : p.defaultOpen && items.length ? itemId(items[0]) : undefined);
  const [page, setPage] = useState<number>();
  const focusedId = focused ? itemId(focused) : undefined;
  useEffect(() => {
    if (focusedId === undefined) return;
    setOpen(focusedId);
    setFilterId(filters[0].id);
    setPage(undefined);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [focusedId]);

  const counts = useMemo(() => filters.map((f) => items.filter((i) => f.match(i)).length), [filters, items]);
  const filter = filters.find((f) => f.id === filterId) ?? filters[0];
  const visible = useMemo(() => items.filter((i) => filter.match(i)), [items, filter]);
  const pages = Math.max(1, Math.ceil(visible.length / pageSize));
  const openAt = open === undefined ? -1 : visible.findIndex((i) => itemId(i) === open);
  const current = Math.min(page ?? Math.max(0, Math.floor(openAt / pageSize)), pages - 1);
  const shown = visible.slice(current * pageSize, (current + 1) * pageSize);
  const pills = filters.filter((_f, i) => i === 0 || counts[i] > 0);

  return (
    <div className="rv-frame" role="region" aria-label={p.label}>
      <div className="rv-bar">
        {pills.length > 1 && <div className="rv-filters" role="group" aria-label="Filter records">
          {pills.map((f) => (
            <button key={f.id} type="button" aria-pressed={f.id === filter.id} onClick={() => { setFilterId(f.id); setPage(undefined); }}>
              {f.label}<span className="rv-count">{counts[filters.indexOf(f)]}</span>
            </button>
          ))}
        </div>}
        <span className="rv-hint">
          {focus && focused && p.onClearFocus
            ? <button type="button" className="link" onClick={p.onClearFocus}>Show all records</button>
            : items.length > 0 && `Select a record to ${p.hint}`}
        </span>
      </div>
      {!shown.length && <div className="rv-empty">{p.empty ?? 'No records match this filter.'}</div>}
      <ColumnsContext.Provider value={p.columns}>
        <div id={listId} className={p.columns ? 'rv-tabular' : undefined} style={p.columns ? columnStyle(p.columns) : undefined}>
          {p.columns && shown.length > 0 && <div className="rv-head" aria-hidden="true">
            <span />
            {p.columns.badge && <span>{p.columns.badge}</span>}
            <span>{p.columns.record}</span>
            {p.columns.service && <span>{p.columns.service}</span>}
            {p.columns.result && <span className="rv-head-result">{p.columns.result}</span>}
            <span />
          </div>}
          {shown.map((item) => {
            const id = itemId(item);
            const isOpen = open === id;
            const panelId = `${listId}-${shown.indexOf(item)}`;
            return (
              <div key={id} className="rv-item">
                {p.renderRow(item, { open: isOpen, selected: !!focus && !!focusMatches?.(item, focus), panelId, toggle: () => setOpen(isOpen ? undefined : id) })}
                {isOpen && <div className="rv-panel" id={panelId}>{p.renderDetail(item)}</div>}
              </div>
            );
          })}
        </div>
      </ColumnsContext.Provider>
      {pages > 1 && <div className="signal-pagination">
        <span>{current * pageSize + 1}–{Math.min((current + 1) * pageSize, visible.length)} of {visible.length}</span>
        <button type="button" disabled={current === 0} onClick={() => setPage(current - 1)}>Previous {p.pageNoun ?? 'records'}</button>
        <span>Page {current + 1} of {pages}</span>
        <button type="button" disabled={current + 1 >= pages} onClick={() => setPage(current + 1)}>Next {p.pageNoun ?? 'records'}</button>
      </div>}
    </div>
  );
}
