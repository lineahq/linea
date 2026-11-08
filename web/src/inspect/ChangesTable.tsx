import { useMemo, useState } from 'react';
import {
  createColumnHelper,
  createSortedRowModel,
  flexRender,
  rowSortingFeature,
  sortFn_alphanumeric,
  sortFn_text,
  tableFeatures,
  useTable,
  type SortingState,
} from '@tanstack/react-table';
import type { Change } from '../api/types';
import { refLabel } from './filtering';
import { pageOf } from './pages';
import { Pagination } from './Pagination';

const features = tableFeatures({
  rowSortingFeature,
  sortedRowModel: createSortedRowModel(),
  sortFns: { alphanumeric: sortFn_alphanumeric, text: sortFn_text },
});
const col = createColumnHelper<typeof features, Change>();

const show = (v: unknown) => (v === undefined ? '' : typeof v === 'string' ? v : JSON.stringify(v));

/**
 * ChangesTable lists the field changes a stage made, sortable, filterable by
 * record, 25 at a time. A record reads by its name from names (its body, span
 * or metric name) over its position, or by its position alone.
 */
export function ChangesTable({
  changes,
  record,
  names,
  onRecord,
}: {
  changes: Change[];
  record?: string;
  names?: ReadonlyMap<string, string>;
  onRecord(entity: string | undefined): void;
}) {
  const [sorting, setSorting] = useState<SortingState>([]);
  const [page, setPage] = useState(0);
  const data = useMemo(() => (record ? changes.filter((c) => c.entity === record) : changes), [changes, record]);
  const columns = useMemo(
    () => col.columns([
      col.accessor('entity', {
        header: 'Record',
        cell: (c) => {
          const entity = c.getValue();
          const name = names?.get(entity);
          return (
            <>
              <button type="button" className="link" onClick={() => onRecord(entity)} title={`${entity}: open this record on Changes`}>
                {name || refLabel(entity)}
              </button>
              {name && <div className="muted small">{refLabel(entity)}</div>}
            </>
          );
        },
      }),
      col.accessor('path', { header: 'Field', cell: (c) => <code>{c.getValue()}</code> }),
      col.accessor('op', { header: 'Change', cell: (c) => <span className={`op op-${c.getValue()}`}>{c.getValue().replace('_', ' ')}</span> }),
      col.accessor((c) => show(c.before), {
        id: 'before',
        header: 'Before',
        cell: (c) => <code className="val">{c.getValue()}</code>,
      }),
      col.accessor((c) => show(c.after), {
        id: 'after',
        header: 'After',
        cell: (c) => <code className="val">{c.getValue()}</code>,
      }),
      col.accessor((c) => c.confidence ?? 'exact', { id: 'confidence', header: 'Match' }),
    ]),
    [names, onRecord],
  );
  const table = useTable({
    features,
    data,
    columns,
    state: { sorting },
    onSortingChange: setSorting,
  });
  if (!changes.length) return <p className="muted">This stage changed no fields.</p>;
  const rows = table.getRowModel().rows;
  const p = pageOf(rows, page);
  return (
    <>
    <table className="grid changes">
      <thead>
        {table.getHeaderGroups().map((g) => (
          <tr key={g.id}>
            {g.headers.map((h) => (
              <th
                key={h.id}
                onClick={h.column.getToggleSortingHandler()}
                aria-sort={h.column.getIsSorted() === 'asc' ? 'ascending' : h.column.getIsSorted() === 'desc' ? 'descending' : 'none'}
              >
                {flexRender(h.column.columnDef.header, h.getContext())}
                {{ asc: ' ▲', desc: ' ▼' }[h.column.getIsSorted() as string] ?? ''}
              </th>
            ))}
          </tr>
        ))}
      </thead>
      <tbody>
        {p.shown.map((r) => (
          <tr key={r.id} className={`op-row-${r.original.op}`}>
            {r.getAllCells().map((c) => (
              <td key={c.id}>{flexRender(c.column.columnDef.cell, c.getContext())}</td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
    <Pagination p={p} total={rows.length} noun="changes" setPage={setPage} />
    </>
  );
}
