import { useEffect, useState } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import { runPlan } from '../input/quickInput';
import { runListQuery } from '../runs/runList';
import { runCountText } from '../runs/runPages';
import { useStartRun } from '../runs/useStartRun';
import type { RunView } from '../runs/useRun';
import { RuntimeSelect } from '../runtimes/RuntimeSelect';
import { Icon } from '../ui/Icon';
import { updateWorkspace, type WorkspaceState } from '../workspace/store';
import { CompareMenu, MoreMenu } from './InvestigateActions';
import { elapsedText, runConfigName, runMeta, saveState } from './headModel';
import { KeepButton } from './KeepButton';
import { ReadinessChip } from './ReadinessChip';
import { runLabel } from './runFacts';
import { SaveIdent } from './SaveIdent';
import { SaveMenu } from './SaveMenu';
import { useDenial } from '../ui/session';
import type { DebuggerState, ProblemFix, Readiness } from './state';

export interface WorkbenchHeadProps {
  state: Exclude<DebuggerState, 'start'>;
  ws: WorkspaceState;
  readiness: Readiness;
  /** The draft can be submitted now (valid, and the document can run). */
  canRun: boolean;
  runId?: string;
  view: RunView;
  /** The drafts differ from the sources of the shown run. */
  draftDiffers: boolean;
  /** A run started from the header: show it. */
  onRun(id: string): void;
  onFix(fix: ProblemFix): void;
  onOpenRuns(): void;
  /** Leave the run for editing the next experiment. */
  onEdit(): void;
  onCompare(id: string): void;
  onAddAssertions(): void;
  /** The shown run was deleted. */
  onDeleted(): void;
  /** Open the start state (resume, recent work, open something else). */
  onStart(): void;
}

/**
 * WorkbenchHead is the debugger's header: what is shown (the open
 * configuration and scenario with their save states, or a recorded run), the run list and Save, and the actions of the working state.
 * Prepare: readiness, the runtime and Run. Running: progress and Cancel.
 * Investigate: Compare, Add assertions, More and Edit and rerun.
 */
export function WorkbenchHead(p: WorkbenchHeadProps) {
  const investigating = p.state === 'investigate' || p.state === 'compare';
  const run = p.view.run;
  const runs = useQuery(runListQuery);
  const configs = useQuery({ queryKey: ['artifacts', 'configuration'], queryFn: () => api.artifacts('configuration'), retry: false });
  const scenarios = useQuery({ queryKey: ['artifacts', 'scenario'], queryFn: () => api.artifacts('scenario'), retry: false });
  const finishedOk = run?.state === 'done' && run.report?.result?.status === 'completed';

  return (
    <header className="od-wbhead">
      <button type="button" className="od-btn od-btn--ghost od-btn--sm od-btn--icon" aria-label="Start page" title="Start page" onClick={p.onStart}>
        <Icon name="chevron-left" size={16} />
      </button>
      <div className="od-ident">
        {investigating && p.runId ? (
          <>
            <span className="od-ident__marker od-ident__marker--run">
              <Icon name="run-marker" size={12} />
              Run {runLabel(p.runId)}
            </span>
            <span className="od-ident__name">{runConfigName(run)}</span>
            {run && <span className="od-ident__meta">{runMeta(run)}</span>}
          </>
        ) : (
          <>
            <SaveIdent icon="file-code" what="Configuration" state={saveState(p.ws, 'configuration', configs.data?.items)} />
            {p.ws.scenarioYAML.trim() && (
              <SaveIdent icon="flask-conical" what="Scenario" state={saveState(p.ws, 'scenario', scenarios.data?.items)} />
            )}
          </>
        )}
      </div>
      <span style={{ flex: 1 }} />
      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        <button type="button" className="od-btn" data-runs-trigger="" onClick={p.onOpenRuns}>
          <Icon name="history" />
          <span>Runs</span>
          {runs.data && <span className="od-btn__count">{runCountText(runs.data)}</span>}
        </button>
        {investigating ? (
          <InvestigateControls {...p} finishedOk={finishedOk} />
        ) : (
          <PrepareControls {...p} />
        )}
      </div>
    </header>
  );
}

function PrepareControls(p: WorkbenchHeadProps) {
  const running = p.state === 'running';
  const start = useStartRun(p.ws, p.onRun);
  const cancel = useMutation({ mutationFn: (id: string) => api.cancelRun(id) });
  const mode = runPlan(p.ws.scenarioYAML).mode;
  const blocked = !p.canRun;
  const denied = useDenial('run.submit');
  return (
    <>
      <SaveMenu ws={p.ws} />
      <span className="od-vsep" style={{ height: 24, alignSelf: 'center' }} />
      {running && p.runId ? <Elapsed since={p.view.run?.started ?? p.view.run?.created} /> : <ReadinessChip readiness={p.readiness} onFix={p.onFix} />}
      {!running && (
        <label className="od-check" style={{ fontSize: 12 }} title="Also run an uninstrumented reference Collector and compare outputs">
          <input type="checkbox" checked={p.ws.compare} onChange={(e) => updateWorkspace({ compare: e.target.checked })} />
          <span>Compare</span>
        </label>
      )}
      <div className="od-runctl">
        {/* The one runtime picker; locked while a run is in progress. */}
        <div className="od-runctl__select" {...(running ? { inert: true, title: 'The runtime is locked while the run is in progress' } : {})}>
          <RuntimeSelect selected={p.ws.runtime ?? ''} />
        </div>
        {running && p.runId ? (
          <button type="button" className="od-btn od-btn--lg" disabled={cancel.isPending || !!denied} title={denied} onClick={() => cancel.mutate(p.runId!)}>
            <Icon name="square" size={14} />
            <span>Cancel</span>
          </button>
        ) : (
          <button
            type="button"
            className="od-btn od-btn--primary od-btn--lg"
            disabled={!!denied || blocked || start.isPending}
            title={denied ?? (blocked ? 'Resolve the problems listed beside Run first' : mode === 'quick' ? 'Run the configuration on the scenario’s input' : 'Run the configuration on every input, then check the assertions')}
            onClick={() => start.mutate({})}
          >
            <Icon name="play" size={14} />
            <span>Run</span>
          </button>
        )}
      </div>
      {(start.error || cancel.error) && (
        <span className="od-field__error" role="alert">{(start.error ?? cancel.error)!.message}</span>
      )}
    </>
  );
}

function InvestigateControls(p: WorkbenchHeadProps & { finishedOk: boolean }) {
  const start = useStartRun(p.ws, p.onRun);
  const run = p.view.run;
  const rerun = p.draftDiffers;
  const denied = useDenial('run.submit');
  return (
    <>
      {p.runId && <CompareMenu runId={p.runId} run={run} onPick={p.onCompare} />}
      {p.finishedOk && (
        <button type="button" className="od-btn" onClick={p.onAddAssertions}>
          <Icon name="flask-conical" />
          <span>Add assertions</span>
        </button>
      )}
      {run && <KeepButton run={run} />}
      {run && <MoreMenu run={run} onDeleted={p.onDeleted} />}
      {rerun ? (
        <button
          type="button"
          className="od-btn od-btn--primary od-btn--lg"
          disabled={start.isPending || !!denied}
          title={denied ?? (p.canRun ? 'Run your configuration and scenario, which have changed since this run' : 'Your changes cannot run yet: edit them to see why')}
          onClick={() => (p.canRun ? start.mutate({}) : p.onEdit())}
        >
          <Icon name="play" size={14} />
          <span>Run with my changes</span>
        </button>
      ) : (
        <button type="button" className="od-btn od-btn--primary od-btn--lg" onClick={p.onEdit}>
          <Icon name="pencil" />
          <span>Edit and rerun</span>
        </button>
      )}
      {start.error && <span className="od-field__error" role="alert">{start.error.message}</span>}
    </>
  );
}

/** Elapsed shows how long the run has been going. */
function Elapsed({ since }: { since?: string }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 200);
    return () => clearInterval(t);
  }, []);
  const from = since ? new Date(since).getTime() : now;
  return (
    <span className="od-ready" style={{ color: 'var(--accent-fg)' }} role="status">
      <Icon name="loader-circle" size={14} />
      Running · {elapsedText(now - from)}
    </span>
  );
}
