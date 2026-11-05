import { api } from '../../api/client';
import type { RuntimeBuildPage } from '../../api/types';
import { finished } from '../runtimes';

/**
 * buildsQuery reads the newest page of runtime builds (the caller's own, or
 * all with runtime.manage); while one has not finished it is read every 3 s,
 * so the rail badge, the Start view and the state chips follow it. A server
 * that cannot build answers with an error, which shows no badge.
 */
export const buildsQuery = {
  queryKey: ['runtime-builds', 'active'],
  queryFn: () => api.runtimeBuilds(),
  retry: false,
  refetchInterval: (q: { state: { data?: RuntimeBuildPage } }) => (q.state.data?.builds?.some((b) => !finished(b.state)) ? 3000 : false),
  refetchIntervalInBackground: false,
} as const;
