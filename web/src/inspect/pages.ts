/** PAGE_SIZE is how many records, rows or links a view renders at once. */
export const PAGE_SIZE = 25;

/** Page is one page of a list: the items to render and where they sit. */
export interface Page<T> {
  shown: T[];
  page: number;
  pages: number;
  start: number;
}

/**
 * pageOf cuts a list into the page a view renders, clamping the requested
 * page to the last one so a list that shrank still shows its end.
 */
export function pageOf<T>(items: readonly T[], page: number, size = PAGE_SIZE): Page<T> {
  const pages = Math.max(1, Math.ceil(items.length / size));
  const current = Math.min(Math.max(0, page), pages - 1);
  const start = current * size;
  return { shown: items.slice(start, start + size), page: current, pages, start };
}

/**
 * capOptions bounds the options of a picker: at most `limit` of them, and
 * how many were left out, so the picker can say where to find the rest.
 */
export function capOptions<T>(items: readonly T[], limit = 200): { shown: T[]; hidden: number } {
  return { shown: items.slice(0, limit), hidden: Math.max(0, items.length - limit) };
}
