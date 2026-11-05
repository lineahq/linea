import { useEffect, useRef, useState } from 'react';
import { Link, useNavigate } from '@tanstack/react-router';
import { useMutation, useQuery } from '@tanstack/react-query';
import { api } from '../../api/client';
import type { CaptureSession } from '../../api/types';
import { scenarioSetup } from '../../input/quickInput';
import { receiverSignals } from '../../scenario/scenarioDoc';
import { Icon } from '../../ui/Icon';
import { useDebounced } from '../../useDebounced';
import { newScenarioPatch } from '../../workspace/drafts';
import { getWorkspace, updateWorkspace, updateWorkspaceSessionOnly, useWorkspace, type WorkspaceState } from '../../workspace/store';
import { useReplaceDrafts } from '../../workspace/useReplaceDrafts';
import { draftDrifted, draftSummary, keepsText, materializeFixtures } from '../addInputs';
import { stashCaptureHandoff } from '../handoff';
import { appendCaptureInputs } from '../inputs';
import { streamKey, streamLabel, type StreamSelection } from '../recordView';
import { InlineConfirm } from './InlineConfirm';

const COPY_WARNING = 'Saving a fixture keeps a separate copy of the selected records. It may contain personal data, and deleting the capture will not remove that copy.';

/**
 * "Use records": put the chosen records into a debugger scenario. They start
 * a new scenario for the open configuration (asking first when the open
 * scenario has unsaved edits), go to the current draft (appended, nothing
 * existing is replaced) or are handed to another configuration the owner
 * picks in the Debugger. Only record
 * identifiers cross to the Debugger, never payloads. In the Debugger, a
 * handed-off selection (`handedOff`) can only go to the current draft.
 */
export function UseRecordsDrawer({ session, groups, onClose, onAdded, handedOff = false }: {
  session: CaptureSession;
  groups: StreamSelection[];
  onClose: () => void;
  /** The records were appended to the draft. */
  onAdded?: () => void;
  handedOff?: boolean;
}) {
  const navigate = useNavigate();
  const ws = useWorkspace();
  const [dest, setDest] = useState<'new' | 'draft' | 'other'>(handedOff ? 'draft' : 'new');
  const [receivers, setReceivers] = useState<Record<string, string>>({});
  const [progress, setProgress] = useState('');
  const [label, setLabel] = useState('');
  const [confirmSave, setConfirmSave] = useState(false);
  const [handoffError, setHandoffError] = useState('');
  const [notice, setNotice] = useState('');
  const heading = useRef<HTMLHeadingElement>(null);
  const abort = useRef<AbortController>(undefined);
  useEffect(() => heading.current?.focus(), []);
  useEffect(() => () => abort.current?.abort(), []);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && onClose();
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [onClose]);

  // A new scenario starts without the open scenario's mocks and settings.
  const setupOf = (w: Pick<WorkspaceState, 'scenarioYAML'>) => (dest === 'new' ? '' : scenarioSetup(w.scenarioYAML));
  const setup = setupOf(ws);
  const config = useDebounced(ws.configYAML, 250);
  const debouncedSetup = useDebounced(setup, 250);
  const graph = useQuery({
    queryKey: ['capture-destinations', config, debouncedSetup],
    queryFn: ({ signal }) => api.graph({ config_yaml: config, setup_yaml: debouncedSetup }, signal),
    enabled: dest !== 'other' && !!config.trim(),
    retry: false,
  });
  const current = config === ws.configYAML && debouncedSetup === setup;
  const valid = current && !graph.isError && graph.data?.valid === true;
  const accepts = receiverSignals(valid ? graph.data?.topology : undefined);
  const choices = (g: StreamSelection) => [...accepts].filter(([, signals]) => signals.includes(g.stream.signal ?? '')).map(([id]) => id).sort();
  const receiverFor = (g: StreamSelection) => receivers[streamKey(g.stream)] ?? (choices(g).length === 1 ? choices(g)[0] : '');
  const chosen = groups.map((g) => ({ ...g, receiver: receiverFor(g) }));
  const summary = draftSummary(ws.scenarioYAML);
  const draftError = 'error' in summary ? summary.error : undefined;
  const mapped = chosen.every((g) => g.receiver && choices(g).includes(g.receiver));
  const ready = valid && (dest === 'new' || !draftError) && mapped;
  const total = groups.reduce((n, g) => n + g.sequences.length, 0);
  const raw = session.data.contains_raw;
  const where = ws.configPath || 'the unsaved configuration';

  const write = (patch: Partial<WorkspaceState>) => (raw ? updateWorkspaceSessionOnly(patch) : updateWorkspace(patch));
  const done = () => {
    onAdded?.();
    void navigate({ to: '/', search: { editor: 'inputs' } });
  };
  const replace = useReplaceDrafts({ after: done });

  const add = useMutation({
    mutationFn: async (): Promise<string | undefined> => {
      const controller = new AbortController();
      abort.current = controller;
      const prepared = { configYAML: ws.configYAML, setup };
      try {
        const fixtures = await materializeFixtures({
          sessionId: session.id,
          groups: chosen,
          configYAML: prepared.configYAML,
          setupYAML: prepared.setup,
          signal: controller.signal,
          fetchFixture: api.captureFixture,
          onProgress: (done, count) => setProgress(`Preparing input ${done + 1} of ${count}…`),
        });
        const latest = getWorkspace();
        if (draftDrifted({ configYAML: latest.configYAML, setup: setupOf(latest) }, prepared)) {
          throw new Error('The configuration or mocks changed while the inputs were being prepared. Review the receivers and try again.');
        }
        if (dest === 'new') return appendCaptureInputs('', fixtures);
        write({ scenarioYAML: appendCaptureInputs(latest.scenarioYAML, fixtures) });
        return undefined;
      } finally {
        setProgress('');
      }
    },
    onSuccess: (scenario) => {
      if (scenario === undefined) done();
      else replace.request(['scenario'], 'Creating a scenario from these records', () => write(newScenarioPatch(getWorkspace(), scenario)));
    },
  });

  const save = useMutation({
    mutationFn: () => {
      const g = chosen[0];
      return api.captureFixture(session.id, {
        selection: { stream: g.stream, sequences: g.sequences, watermark: g.watermark, receiver: g.receiver },
        config_yaml: ws.configYAML,
        setup_yaml: setup,
        save: true,
        ...(label.trim() ? { label: label.trim() } : {}),
      });
    },
    onSuccess: () => {
      setConfirmSave(false);
      setNotice('Fixture saved as a separate copy on the server.');
    },
    onError: () => setConfirmSave(false),
  });

  function handOff() {
    const stored = stashCaptureHandoff({
      captureId: session.id,
      containsRaw: raw,
      streams: groups.map((g) => ({ stream: g.stream, sequences: g.sequences, watermark: g.watermark })),
    });
    if (!stored) {
      setHandoffError('This browser could not keep the selection for the Debugger (storage is unavailable). Your selection here is unchanged; add the records to the open scenario instead.');
      return;
    }
    void navigate({ to: '/' });
  }

  const busy = add.isPending || save.isPending;
  return (
    <aside className="od-drawer" role="dialog" aria-label={`Use ${total} record${total === 1 ? '' : 's'} as inputs`} style={{ width: 460 }}>
      <div className="od-drawer__head">
        <h2 ref={heading} tabIndex={-1} style={{ margin: 0, fontSize: 15, flex: 1 }}>Use {total} record{total === 1 ? '' : 's'} as inputs</h2>
        <button type="button" className="od-btn od-btn--ghost od-btn--sm" aria-label="Close" onClick={onClose}><Icon name="x" size={14} /></button>
      </div>
      <div style={{ flex: 1, overflow: 'auto', padding: 16, display: 'flex', flexDirection: 'column', gap: 14 }}>
        {!handedOff && <fieldset style={{ border: 0, padding: 0, margin: 0, display: 'flex', flexDirection: 'column', gap: 8 }}>
          <legend style={{ fontSize: 12.5, fontWeight: 600, marginBottom: 6 }}>Into</legend>
          <label className={`od-radio-card${dest === 'new' ? ' od-radio-card--on' : ''}`}>
            <input type="radio" name="dest" checked={dest === 'new'} onChange={() => setDest('new')} />
            <span>
              <strong>New scenario · {where}</strong>
              <br />
              <span className="od-subtle">A scenario with only these records as inputs, ready to run. It replaces the open scenario{summary && !('error' in summary) && summary.inputs + summary.assertions > 0 ? ', and asks first if that has unsaved changes' : ''}.</span>
            </span>
          </label>
          <label className={`od-radio-card${dest === 'draft' ? ' od-radio-card--on' : ''}`}>
            <input type="radio" name="dest" checked={dest === 'draft'} onChange={() => setDest('draft')} />
            <span>
              <strong>Add to the open scenario</strong>
              <br />
              <span className="od-subtle">
                {draftError ? 'The scenario cannot be read.' : `Appends to the scenario. ${keepsText(summary as Exclude<typeof summary, { error: string }>)}`}
              </span>
            </span>
          </label>
          <label className={`od-radio-card${dest === 'other' ? ' od-radio-card--on' : ''}`}>
            <input type="radio" name="dest" checked={dest === 'other'} onChange={() => setDest('other')} />
            <span>
              <strong>Another configuration</strong>
              <br />
              <span className="od-subtle">Your selection is kept while you open or pick one in the Debugger.</span>
            </span>
          </label>
        </fieldset>}

        {dest !== 'other' && (
          <>
            {dest === 'draft' && draftError && (
              <div className="od-callout od-callout--fail" role="alert">
                <Icon name="circle-x" size={16} />
                <div className="od-callout__main">
                  <div className="od-callout__title">Your scenario YAML cannot be read</div>
                  <div className="od-callout__body">{draftError} Your draft is untouched.</div>
                  <div className="od-callout__actions"><Link to="/" search={{ editor: 'yaml' }} className="od-btn od-btn--sm">Fix it in the Debugger</Link></div>
                </div>
              </div>
            )}
            {!ws.configYAML.trim() && (
              <div className="od-callout od-callout--warn" role="status">
                <Icon name="triangle-alert" size={16} />
                <div className="od-callout__main">
                  <div className="od-callout__title">No configuration is loaded in this browser</div>
                  <div className="od-callout__body">Inputs need a receiver from a configuration. Open one in the Debugger, or send the records to another configuration.</div>
                  <div className="od-callout__actions"><Link to="/" search={{ editor: 'config' }} className="od-btn od-btn--sm">Open the Debugger</Link></div>
                </div>
              </div>
            )}
            {ws.configYAML.trim() && (!current || graph.isPending) && <p role="status" className="od-muted" style={{ margin: 0 }}>Checking the receivers in your configuration…</p>}
            {graph.error && (
              <div className="od-callout od-callout--fail" role="alert">
                <Icon name="circle-x" size={16} />
                <div className="od-callout__main">
                  <div className="od-callout__title">Could not check the receivers</div>
                  <div className="od-callout__body">{graph.error.message}</div>
                  <div className="od-callout__actions"><button type="button" className="od-btn od-btn--sm" onClick={() => void graph.refetch()}>Try again</button></div>
                </div>
              </div>
            )}
            {current && graph.data && !graph.data.valid && (
              <div className="od-callout od-callout--fail" role="alert">
                <Icon name="circle-x" size={16} />
                <div className="od-callout__main">
                  <div className="od-callout__title">The configuration or mocks cannot be used yet</div>
                  <ul className="od-callout__body" style={{ margin: 0, paddingLeft: 16 }}>
                    {(graph.data.diagnostics ?? []).filter((d) => d.severity === 'error').slice(0, 5).map((d, i) => <li key={i}>{d.message}</li>)}
                  </ul>
                  <div className="od-callout__actions"><Link to="/" search={{ editor: 'config' }} className="od-btn od-btn--sm">Fix it in the Debugger</Link></div>
                </div>
              </div>
            )}
            {valid && (
              <section aria-label="Receivers">
                <h3 style={{ margin: '0 0 6px', fontSize: 12.5 }}>Map each stream to a receiver</h3>
                <table className="od-table">
                  <thead><tr><th>Stream</th><th>Receiver</th></tr></thead>
                  <tbody>
                    {chosen.map((g) => {
                      const options = choices(g);
                      return (
                        <tr key={streamKey(g.stream)}>
                          <td>
                            <div className="mono">{streamLabel(g.stream)}</div>
                            <div className="od-subtle" style={{ fontSize: 12 }}>{g.sequences.length} record{g.sequences.length === 1 ? '' : 's'}</div>
                          </td>
                          <td>
                            <select
                              className="od-select"
                              aria-label={`Receiver for ${streamLabel(g.stream)}`}
                              value={g.receiver}
                              disabled={busy || !options.length}
                              onChange={(e) => setReceivers((r) => ({ ...r, [streamKey(g.stream)]: e.target.value }))}
                            >
                              <option value="">{options.length ? 'Choose receiver' : `No receiver accepts ${g.stream.signal}`}</option>
                              {options.map((id) => <option key={id} value={id}>{id}</option>)}
                            </select>
                            {!options.length && <div role="alert" className="od-field__error">Add a {g.stream.signal} pipeline with a receiver in the Debugger, or send these records to another configuration.</div>}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </section>
            )}
          </>
        )}

        {dest === 'other' && (
          <div className="od-callout od-callout--info" role="status">
            <Icon name="info" size={16} />
            <div className="od-callout__main">
              <div className="od-callout__title">Choose the configuration in the Debugger</div>
              <div className="od-callout__body">Only the capture, its streams and the record numbers are kept for the Debugger, for one hour in this browser session. The records themselves stay in this capture.</div>
            </div>
          </div>
        )}

        <div className="od-callout od-callout--warn" role="note">
          <Icon name="triangle-alert" size={16} />
          <div className="od-callout__main">
            <div className="od-callout__title">Captured data may contain personal data</div>
            <div className="od-callout__body">
              Review the records before exporting a scenario that includes them.
              {raw && ' These records include raw payloads, so the open scenario stays in this browser session only.'}
            </div>
          </div>
        </div>

        {replace.confirm}
        {add.error && <div role="alert" className="od-field__error">Could not add the records: {add.error.message} Your scenario has not been changed.</div>}
        {handoffError && <div role="alert" className="od-field__error">{handoffError}</div>}
        {save.error && <div role="alert" className="od-field__error">{save.error.message}</div>}
        {notice && <div role="status" className="od-field__help">{notice}</div>}

        {dest !== 'other' && ready && groups.length === 1 && (
          <details>
            <summary style={{ cursor: 'pointer', fontSize: 12.5 }}>Save as a fixture instead</summary>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 8, paddingTop: 8 }}>
              <div className="od-field">
                <label htmlFor="fixture-label">Fixture label (optional)</label>
                <input id="fixture-label" className="od-input" maxLength={128} value={label} onChange={(e) => setLabel(e.target.value)} />
              </div>
              {confirmSave ? (
                <InlineConfirm busy={save.isPending} confirmLabel="Save fixture" cancelLabel="Do not save" onConfirm={() => save.mutate()} onCancel={() => setConfirmSave(false)}>
                  {COPY_WARNING}
                </InlineConfirm>
              ) : (
                <div><button type="button" className="od-btn od-btn--sm" disabled={busy} onClick={() => setConfirmSave(true)}><Icon name="save" size={14} /><span>Save as fixture…</span></button></div>
              )}
            </div>
          </details>
        )}
      </div>
      <div className="od-drawer__foot">
        {dest !== 'other' ? (
          <button type="button" className="od-btn od-btn--primary" disabled={!ready || busy || replace.asking} onClick={() => add.mutate()}>
            {add.isPending ? progress || 'Adding…' : dest === 'new' ? 'Create the scenario and open it' : 'Add as inputs and open the scenario'}
          </button>
        ) : (
          <button type="button" className="od-btn od-btn--primary" disabled={busy} onClick={handOff}>Choose a configuration in the Debugger</button>
        )}
        <button type="button" className="od-btn" onClick={onClose}>Cancel</button>
      </div>
    </aside>
  );
}
