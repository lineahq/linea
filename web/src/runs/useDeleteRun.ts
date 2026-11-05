import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';

/**
 * useDeleteRun deletes a run after the caller's inline confirmation: `asking`
 * is the run whose confirmation is open. A deleted run leaves the caches and
 * `onDeleted` is called so the page stops showing it.
 */
export function useDeleteRun(onDeleted: () => void) {
  const qc = useQueryClient();
  const [asking, setAsking] = useState<string>();
  const remove = useMutation({
    mutationFn: (id: string) => api.deleteRun(id),
    onSuccess: (_, id) => {
      setAsking(undefined);
      qc.removeQueries({ queryKey: ['run', id] });
      void qc.invalidateQueries({ queryKey: ['runs'] });
      onDeleted();
    },
  });
  return {
    asking,
    ask: (id: string) => {
      remove.reset();
      setAsking(id);
    },
    cancel: () => {
      setAsking(undefined);
      remove.reset();
    },
    remove,
  };
}
