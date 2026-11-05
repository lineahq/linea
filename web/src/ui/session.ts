import { useQuery } from '@tanstack/react-query';
import { api, setCSRFToken } from '../api/client';
import { denial, type Held, type Permission } from './access';

/**
 * sessionQuery reads who is signed in and what they may do. It
 * keeps the web session's CSRF token for the requests that follow.
 */
export const sessionQuery = {
  queryKey: ['session'],
  queryFn: async () => {
    const s = await api.session();
    setCSRFToken(s.csrf_token);
    return s;
  },
  staleTime: 60_000,
  retry: false,
} as const;

/**
 * useHeld is the caller's permissions as far as the interface knows them.
 * Only the shell fetches the session; elsewhere it is read from the cache,
 * and unknown until then, which offers everything.
 */
export function useHeld(): Held {
  return useQuery({ ...sessionQuery, enabled: false }).data?.permissions;
}

/**
 * useDenial is why an action needing perm is disabled, or undefined when
 * the caller may take it.
 */
export function useDenial(perm: Permission): string | undefined {
  return denial(useHeld(), perm);
}
