import { useState, type ReactNode } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import type { RunInputs, RunSource } from '../api/types';
import { runLabel } from '../debugger/runFacts';
import { getWorkspace, updateWorkspace } from '../workspace/store';
import { runDraft, scenarioFromRunDraft, type RunDraft } from '../workspace/drafts';
import { useReplaceDrafts } from '../workspace/useReplaceDrafts';
import { Icon } from '../ui/Icon';

function Source({ name, source }: { name: string; source?: RunSource }) {
  if (!source) return null;
  return (
    <>
      <dt>{name}</dt>
      <dd>
        {source.label ? <code>{source.label}</code> : <span className="muted">inline text</span>}
        {source.artifact && <span className="muted"> · saved revision <code>{source.artifact.revision_id.slice(0, 8)}</code></span>}
        <span className="muted" title={`sha256 ${source.sha256}`}> · sha256 {source.sha256.slice(0, 12)}</span>
      </dd>
    </>
  );
}

/**
 * RunInputsPanel shows the sources a run was admitted with and opens them as
 * drafts, or creates a self-contained scenario from the run in the scenario
 * draft. Unsaved drafts are replaced only after an inline confirmation; the
 * other run mode's draft is never touched.
 */
export function RunInputsPanel({ runId, rows }: { runId: string; /** More facts (dt/dd pairs) for the Sources list, e.g. the runtime. */ rows?: ReactNode }) {
  const q = useQuery({ queryKey: ['run-inputs', runId], queryFn: () => api.runInputs(runId), staleTime: Infinity, retry: false });
  const replacer = useReplaceDrafts({ compact: true });
  const [notes, setNotes] = useState<string[]>();
  const derive = useMutation({ mutationFn: () => api.scenarioFromRun(runId) });

  if (q.isPending) return <p className="od-muted" style={{ fontSize: 12 }} role="status">Loading the run's sources…</p>;
  if (q.error) return <p className="od-muted" style={{ fontSize: 12 }} role="status">The run's sources are unavailable: {q.error.message}</p>;
  const inputs: RunInputs = q.data;
  const quick = inputs.mode === 'quick';
  const files = inputs.files ?? [];
  const env = inputs.required_environment ?? [];

  // The draft is computed again when it is applied, so it fits the workspace as it is then.
  const offer = (what: string, make: () => RunDraft) =>
    replacer.request(make().drafts, what, () => {
      const d = make();
      updateWorkspace(d.patch);
      setNotes(d.notes);
    });
  const use = () => offer(`Run ${runLabel(runId)}’s inputs`, () => runDraft(getWorkspace(), inputs));
  const createScenario = () =>
    derive.mutate(undefined, {
      onSuccess: (sc) => offer(`The scenario from run ${runLabel(runId)}`, () => scenarioFromRunDraft(getWorkspace(), sc, inputs.config.text)),
    });

  return (
    <section className="run-inputs" aria-label="Run inputs" style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
      <h2 className="od-section-label">Sources</h2>
      <dl className="od-kv">
        <dt>Ran from</dt>
        <dd>{quick ? 'One input, no scenario file' : 'A scenario file'}</dd>
        <Source name="Configuration" source={inputs.config} />
        <Source name={quick ? 'Input' : 'Scenario'} source={quick ? inputs.fixture : inputs.scenario} />
        <Source name="Mocks" source={inputs.setup} />
        <Source name="Assertions" source={inputs.assertions} />
        {inputs.receiver && (
          <>
            <dt>Receiver</dt>
            <dd>{inputs.receiver}{inputs.client_address && <span className="muted"> from {inputs.client_address}</span>}</dd>
          </>
        )}
        {files.length > 0 && (
          <>
            <dt>Files read</dt>
            <dd>{files.map((f) => <code key={f}>{f} </code>)}</dd>
          </>
        )}
        {env.length > 0 && (
          <>
            <dt>Environment</dt>
            <dd>{env.join(', ')} <span className="muted">· names only</span></dd>
          </>
        )}
        {rows}
      </dl>
      {replacer.asking ? (
        replacer.confirm
      ) : (
        <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
          <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={use} title="Open this run's configuration, inputs, mocks and assertions in the editors">
            <Icon name="pencil" size={14} /><span>Open the run's configuration and scenario</span>
          </button>
          <button
            type="button"
            className="od-btn od-btn--ghost od-btn--sm"
            disabled={derive.isPending}
            onClick={createScenario}
            title="Put a self-contained scenario of this run, with its captured inputs inlined, in the scenario editor"
          >
            <Icon name="file-plus" size={14} /><span>Create scenario from run</span>
          </button>
        </div>
      )}
      {derive.error && <p className="error small" role="alert">{derive.error.message}</p>}
      {notes?.map((n, i) => <p key={i} className="note small" role="status">{n}</p>)}
    </section>
  );
}
