import { useMutation, useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import { updateWorkspace, type WorkspaceState } from '../workspace/store';
import type { RunView } from './useRun';
import { stateLabel } from './RunSummary';
import { useStartRun } from './useStartRun';
import { runBlocker, runPlan } from '../input/quickInput';
import { runListQuery } from './runList';
import { firstPageRuns } from './runPages';
import { canDelete, DELETE_WARNING } from './deletion';
import { useDeleteRun } from './useDeleteRun';

interface Props {
  ws: WorkspaceState;
  configValid: boolean;
  view: RunView;
  runId?: string;
  onRun(id: string | undefined): void;
}

/**
 * RunControls starts, cancels, deletes and switches between runs of the
 * workspace. Deleting a finished run asks for an inline confirmation first.
 */
export function RunControls({ ws, configValid, view, runId, onRun }: Props) {
  const runs = useQuery(runListQuery);
  const runList = firstPageRuns(runs.data);
  const start = useStartRun(ws, onRun);
  const cancel = useMutation({ mutationFn: (id: string) => api.cancelRun(id) });
  const { asking: deleting, ask, cancel: keep, remove } = useDeleteRun(() => onRun(undefined));
  const blocker = runBlocker(ws, configValid);
  const mode = runPlan(ws.scenarioYAML).mode;
  const label = runId ? stateLabel(view) : undefined;

  return (
    <div className="run-controls">
      <button
        type="button"
        className="primary"
        disabled={!!blocker || start.isPending}
        title={blocker ?? (mode === 'quick' ? 'Run the input through the configuration (a quick run)' : 'Run every input, then check the assertions (a scenario run)')}
        onClick={() => start.mutate({})}
      >
        {mode === 'quick' ? '▶ Run input' : '▶ Run scenario'}
      </button>
      <label title="Also run an uninstrumented reference Collector and compare outputs">
        <input type="checkbox" checked={ws.compare} onChange={(e) => updateWorkspace({ compare: e.target.checked })} /> Compare
      </label>
      {view.live && runId && (
        <button type="button" onClick={() => cancel.mutate(runId)} disabled={cancel.isPending}>
          Cancel
        </button>
      )}
      {label && <span className={`status status-${label.cls}`}>{label.text}</span>}
      {runId && canDelete(view.run) && deleting !== runId && (
        <button type="button" onClick={() => ask(runId)} title="Delete this run and its recorded evidence">
          Delete
        </button>
      )}
      {runId && deleting === runId && (
        <div className="confirm-inline" role="alertdialog" aria-label="Delete run">
          <span>{DELETE_WARNING}</span>
          <button type="button" className="primary" disabled={remove.isPending} onClick={() => remove.mutate(runId)}>Delete run</button>
          <button type="button" onClick={keep}>Keep</button>
        </div>
      )}
      <select
        aria-label="Run"
        value={runId ?? ''}
        onChange={(e) => onRun(e.target.value || undefined)}
        disabled={!runList.length}
      >
        <option value="">{runList.length ? 'No run selected' : 'No runs yet'}</option>
        {runList.map((r) => (
          <option key={r.id} value={r.id}>
            {new Date(r.created).toLocaleTimeString()} · {r.state} · {r.id.slice(0, 8)}
          </option>
        ))}
      </select>
      {start.error && <span className="error">{start.error.message}</span>}
      {remove.error && <span className="error">{remove.error.message}</span>}
    </div>
  );
}
