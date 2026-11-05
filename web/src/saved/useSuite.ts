import { useQuery } from '@tanstack/react-query';
import { api, ApiError } from '../api/client';
import type { SuiteStatus } from '../api/types';

/** How often a suite that has not finished is read again. */
export const SUITE_POLL_MS = 2000;

/** suiteStatusQuery reads a suite's status and follows it until every member has finished. */
export const suiteStatusQuery = (id: string) => ({
  queryKey: ['suite', id] as const,
  queryFn: () => api.suite(id),
  retry: false,
  refetchInterval: (q: { state: { data: SuiteStatus | undefined } }) => (q.state.data?.done ? false : SUITE_POLL_MS),
  refetchIntervalInBackground: false,
});

/** useSuiteStatus follows one suite. */
export function useSuiteStatus(id: string) {
  return useQuery(suiteStatusQuery(id));
}

/** useSuiteReport reads the test report once the suite is done (the report of an unfinished suite is a 409). */
export function useSuiteReport(id: string, done: boolean) {
  return useQuery({ queryKey: ['suite-report', id], queryFn: () => api.suiteReport(id), enabled: done, retry: false, staleTime: Infinity });
}

/** isMissing: the suite is unknown or was deleted (404). */
export function isMissing(error: unknown): boolean {
  return error instanceof ApiError && error.status === 404;
}
