import { infiniteQueryOptions } from '@tanstack/react-query';
import { api, type RunPage, type RunQuery } from '../api/client';

/**
 * The run list refreshes this often while the page is visible, so runs
 * created elsewhere (the CLI, an agent over MCP) appear without a reload.
 */
export const RUN_LIST_POLL_MS = 5000;

/**
 * The full run list stops polling once this many pages are loaded: a refresh
 * re-reads every loaded page, and someone who paged this far back is reading
 * history, not watching for new runs. Invalidation and window focus still
 * refresh it.
 */
export const RUN_LIST_POLL_MAX_PAGES = 4;

/** runListPollInterval is the poll interval of the paged run list holding `pages` pages. */
export function runListPollInterval(pages: number): number | false {
  return pages > RUN_LIST_POLL_MAX_PAGES ? false : RUN_LIST_POLL_MS;
}

/**
 * runListQuery reads the newest page of runs (data is a `RunPage`; use
 * `firstPageRuns`). Drawers, pickers, comparisons and the start view use it.
 * Hidden pages do not poll.
 */
export const runListQuery = {
  queryKey: ['runs'],
  queryFn: (): Promise<RunPage> => api.listRuns(),
  refetchInterval: RUN_LIST_POLL_MS,
  refetchIntervalInBackground: false,
};

/**
 * runPagesQuery reads the runs the server's filters match, a page at a time
 * (`useInfiniteQuery`). A page's cursor is only valid with the filters it
 * came from, so the filters are part of the key. The key starts with
 * `['runs']`, so invalidating the run list refreshes it too. While polling,
 * TanStack re-reads every loaded page from the first, which keeps deleted and
 * finished runs correct on all of them.
 */
export function runPagesQuery(filter: RunQuery) {
  const { scenario, runtime, result } = filter;
  const base: RunQuery = { ...(scenario && { scenario }), ...(runtime && { runtime }), ...(result && { result }) };
  return infiniteQueryOptions({
    queryKey: ['runs', 'pages', base],
    queryFn: ({ pageParam }) => api.listRuns(pageParam ? { ...base, cursor: pageParam } : base),
    initialPageParam: '',
    getNextPageParam: (last: RunPage) => last.next_cursor || undefined,
    refetchInterval: (query) => runListPollInterval(query.state.data?.pages.length ?? 0),
    refetchIntervalInBackground: false,
  });
}
