// Pure helpers over the run list's pages (GET /api/v1/runs returns {runs, next_cursor}).
import type { RunPage } from '../api/client';
import type { Run } from '../api/types';

/** dedupeRuns drops repeated run ids, keeping the first occurrence. */
export function dedupeRuns(runs: readonly Run[]): Run[] {
  const seen = new Set<string>();
  const out: Run[] = [];
  for (const run of runs) {
    if (seen.has(run.id)) continue;
    seen.add(run.id);
    out.push(run);
  }
  return out;
}

/**
 * flattenRunPages joins loaded pages, newest first. A run can appear on two
 * pages when newer runs arrive between the requests, so repeats are dropped
 * by id and the first occurrence stays.
 */
export function flattenRunPages(pages: readonly RunPage[] | undefined): Run[] {
  return dedupeRuns((pages ?? []).flatMap((p) => p.runs ?? []));
}

/** firstPageRuns is the newest runs a page of the list holds; empty before it loads. */
export function firstPageRuns(page: RunPage | undefined): Run[] {
  return page?.runs ?? [];
}

/** hasMoreRuns: the page says more, older runs follow it. */
export function hasMoreRuns(page: RunPage | undefined): boolean {
  return !!page?.next_cursor;
}

/**
 * runCountText counts the runs of a page for a badge: "50+" when older runs
 * follow, since only the first page was read.
 */
export function runCountText(page: RunPage | undefined): string {
  const n = page?.runs?.length ?? 0;
  return hasMoreRuns(page) ? `${n}+` : String(n);
}

/**
 * runsCountText words how many runs the list holds: "12 runs", "50+ runs"
 * (older ones follow), and with a filter "1 run matches" or "50+ runs match".
 */
export function runsCountText(count: number, more: boolean, filtered: boolean): string {
  const n = `${count}${more ? '+' : ''}`;
  const one = count === 1 && !more;
  if (!filtered) return `${n} run${one ? '' : 's'}`;
  return `${n} run${one ? ' matches' : 's match'}`;
}

/**
 * withRuns adds runs read one by one, such as a compared run older than the
 * first page, to a newest-first list and keeps it newest first. The sort is
 * stable, so runs created at the same instant keep the list's own order,
 * which is admission order.
 */
export function withRuns(list: readonly Run[], extra: readonly (Run | undefined)[]): Run[] {
  const added = extra.filter((r): r is Run => !!r && !list.some((l) => l.id === r.id));
  if (added.length === 0) return list as Run[];
  return dedupeRuns([...list, ...added]).sort((a, b) => Date.parse(b.created) - Date.parse(a.created));
}
