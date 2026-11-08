import { useMemo } from 'react';
import { useQueries, useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import { runListQuery } from './runList';
import { firstPageRuns, withRuns } from './runPages';

/**
 * useComparedRuns is the run list's first page plus any compared run older
 * than it, read on its own as the list shows it (its summary, not its
 * report, which can be large), so a comparison link keeps working once newer
 * runs push a run off the first page. `pending` holds while such a run is
 * being read; a run that cannot be read is simply missing from `list`.
 */
export function useComparedRuns(ids: readonly (string | undefined)[]) {
  const runs = useQuery(runListQuery);
  const first = useMemo(() => firstPageRuns(runs.data), [runs.data]);
  const older = useQueries({
    queries: ids.map((id) => ({
      // Under ['runs'], so invalidating the run list refreshes these too.
      queryKey: ['runs', 'listing', id],
      queryFn: () => api.runListing(id!),
      enabled: !!runs.data && !!id && !first.some((r) => r.id === id),
      retry: false,
    })),
  });
  const extra = older.map((q) => q.data);
  const list = useMemo(() => withRuns(first, extra), [first, ...extra]); // eslint-disable-line react-hooks/exhaustive-deps
  return { runs, list, pending: older.some((q) => q.isLoading) };
}
