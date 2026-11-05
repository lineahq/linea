import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';
import type { Run } from '../api/types';
import { isKept, keepLabel, keepProblem, keepTitle, withKept } from '../runs/keep';
import { Icon } from '../ui/Icon';
import { useDenial } from '../ui/session';

/**
 * KeepButton marks the shown run as kept, so retention and prune never remove
 * it, or lets retention remove it again. It shows the run's
 * state, `run.kept`, and a click reverses it. The returned mark goes into the
 * cached run, and the run lists refresh. The run's owner or an admin may
 * change it: the interface knows only that run.delete is needed, so a refusal
 * from the server shows inline.
 */
export function KeepButton({ run }: { run: Run }) {
  const qc = useQueryClient();
  const denied = useDenial('run.delete');
  const kept = isKept(run);
  const toggle = useMutation({
    mutationFn: () => (kept ? api.unkeepRun(run.id) : api.keepRun(run.id)),
    onSuccess: (returned) => {
      qc.setQueryData<Run>(['run', run.id], (cached) => (cached ? withKept(cached, returned) : cached));
      void qc.invalidateQueries({ queryKey: ['runs'] });
    },
  });
  return (
    <>
      <button
        type="button"
        className={`od-btn${kept ? ' od-btn--primary' : ''}`}
        aria-pressed={kept}
        disabled={toggle.isPending || !!denied}
        title={denied ?? keepTitle(run)}
        onClick={() => {
          toggle.reset();
          toggle.mutate();
        }}
      >
        <Icon name="bookmark" />
        <span>{keepLabel(run)}</span>
      </button>
      {toggle.error && <span className="od-field__error" role="alert">{keepProblem(toggle.error)}</span>}
    </>
  );
}
