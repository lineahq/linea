import type { Page } from './pages';

/** Pagination moves through the pages of a list; nothing for a single page. */
export function Pagination<T>({ p, total, noun, setPage }: { p: Page<T>; total: number; noun: string; setPage(page: number): void }) {
  if (p.pages <= 1) return null;
  return (
    <div className="signal-pagination">
      <span>{p.start + 1}–{p.start + p.shown.length} of {total}</span>
      <button type="button" disabled={p.page === 0} onClick={() => setPage(p.page - 1)}>Previous {noun}</button>
      <span>Page {p.page + 1} of {p.pages}</span>
      <button type="button" disabled={p.page + 1 >= p.pages} onClick={() => setPage(p.page + 1)}>Next {noun}</button>
    </div>
  );
}
