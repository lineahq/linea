import { useQueries, useQuery } from '@tanstack/react-query';
import { api } from '../api/client';

const snapshotQuery = (runId: string | undefined, ref: string | undefined) => ({
  queryKey: ['snapshot', runId, ref],
  queryFn: () => api.snapshot(runId!, ref!),
  enabled: !!runId && !!ref,
  staleTime: Infinity,
  gcTime: 5 * 60_000,
});

/** useSnapshot fetches a recorded payload; refs are content addressed, so it never goes stale. */
export function useSnapshot(runId: string | undefined, ref: string | undefined) {
  return useQuery(snapshotQuery(runId, ref));
}

/** useSnapshots fetches several recorded payloads; data is complete only when every one loaded. */
export function useSnapshots(runId: string | undefined, refs: readonly (string | undefined)[]) {
  return useQueries({
    queries: refs.map((ref) => snapshotQuery(runId, ref)),
    combine: (results) => ({
      data: results.every((r) => r.data !== undefined) ? results.map((r) => r.data) : undefined,
      isLoading: results.some((r) => r.isLoading),
      error: results.find((r) => r.error)?.error ?? null,
    }),
  });
}
