import type { RunView } from '../runs/useRun';
import { Icon } from '../ui/Icon';
import { progressFacts } from './headModel';
import { runLabel } from './runFacts';

/**
 * RunProgress is the bottom pane while a run is in progress: what the run has
 * recorded so far and what editing now means. Only facts the run reports are
 * shown; the Collector declares no steps, so none is invented.
 */
export function RunProgress({ runId, view }: { runId: string; view: RunView }) {
  const run = view.run;
  const queued = run?.state === 'queued' || run?.state === 'assigned' || (view.live && view.events.length === 0);
  const facts = progressFacts(view.events);
  const snapshot = run ? new Date(run.created).toLocaleTimeString() : undefined;
  return (
    <section className="od-pane" aria-label="Run progress">
      <div className="od-card__head">
        <span className="t-subheading">Run {runLabel(runId)} in progress</span>
        <span className="od-chip od-chip--running">
          <Icon name="loader-circle" size={12} />
          {queued ? 'Queued' : 'Running'}
        </span>
      </div>
      <div className="od-pane__body" style={{ padding: '12px 14px' }} role="status" aria-live="polite">
        <dl className="od-kv">
          <dt>Recorded so far</dt>
          <dd>{facts.events} event{facts.events === 1 ? '' : 's'}</dd>
          <dt>Reporting</dt>
          <dd>{facts.components} component{facts.components === 1 ? '' : 's'}</dd>
        </dl>
        <p className="od-muted" style={{ margin: '10px 0 0', fontSize: 12, lineHeight: '17px' }}>
          The run uses a snapshot of the draft{snapshot ? ` taken at ${snapshot}` : ''}. Edits you make now apply to the next run. Leaving this page keeps the run going.
        </p>
      </div>
    </section>
  );
}
