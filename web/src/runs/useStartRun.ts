import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';
import { updateWorkspace, type WorkspaceState } from '../workspace/store';
import { runFields } from '../input/quickInput';
import { runtimeField } from '../runtimes/runtimes';

/**
 * useStartRun queues a run of the workspace's document (see runFields), or
 * of one of its inputs alone. The configuration can be overridden, so an
 * edit applied to the editor in the same event can run before the store
 * update has re-rendered.
 */
export function useStartRun(ws: WorkspaceState, onRun: (id: string) => void) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { configYAML?: string; input?: number } = {}) =>
      api.createRun({
        config_yaml: v.configYAML ?? ws.configYAML,
        compare: ws.compare,
        ...runtimeField(ws.runtime),
        ...runFields(ws, v.input),
      }),
    onSuccess: (r) => {
      qc.setQueryData(['run', r.id], r);
      updateWorkspace({ lastRunId: r.id });
      void qc.invalidateQueries({ queryKey: ['runs'] });
      onRun(r.id);
    },
  });
}
