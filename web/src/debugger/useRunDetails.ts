import { useQueries } from '@tanstack/react-query';
import { api } from '../api/client';
import type { Run } from '../api/types';
import { isFinished } from './runFacts';

/** The runs on screen with their reports where read, and those whose report could not be read. */
export interface RunDetails {
  runs: Map<string, Run>;
  unreadable: Set<string>;
}

/**
 * useRunDetails reads the runs a view shows, with their reports, because the
 * run list carries none. It shares the `['run', id]` query with `useRun`, so a
 * run opened in the debugger is already read, and a finished run never goes
 * stale. A run that is still queued or running is not read: the list's own
 * state is all there is to show of it. Callers pass only the runs on screen
 * (a drawer's recent runs, one page of the list).
 */
export function useRunDetails(runs: Run[]): RunDetails {
  const results = useQueries({
    queries: runs.map((r) => ({
      queryKey: ['run', r.id],
      queryFn: () => api.run(r.id),
      enabled: isFinished(r),
      staleTime: (q: { state: { data?: Run } }) => (q.state.data && isFinished(q.state.data) ? Infinity : 0),
      retry: false,
    })),
  });
  const out = new Map<string, Run>();
  const unreadable = new Set<string>();
  runs.forEach((r, i) => {
    const data = results[i]?.data;
    if (results[i]?.isError) unreadable.add(r.id);
    // The list's state is newer than a cached copy of a run that was still live.
    out.set(r.id, data && isFinished(data) ? data : r);
  });
  return { runs: out, unreadable };
}
