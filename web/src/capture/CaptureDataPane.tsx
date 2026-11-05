import { Link } from '@tanstack/react-router';
import { CaptureDialog } from './CaptureDialog';
import { useCallback, useEffect, useRef, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';
import type { CaptureSession, CaptureStream, CaptureFixture } from '../api/types';
import { downloadText } from '../files';
import { getWorkspace, updateWorkspace, updateWorkspaceSessionOnly, useWorkspace } from '../workspace/store';
import { appendCaptureInputs } from './inputs';
import { storageNote } from './detailView';
import { scenarioSetup } from '../input/quickInput';
import { readScenario, receiverSignals } from '../scenario/scenarioDoc';
import { useDebounced } from '../useDebounced';
import { CaptureRecords } from './pages/CaptureRecords';

const sourceKey = (stream: CaptureStream) => JSON.stringify([stream.collector_id, stream.pipeline, stream.point, stream.signal]);
export const CAPTURE_COPY_WARNING = 'Captured telemetry, mocks and assertions may contain sensitive data. Saving or downloading retains a separate copy. Review it before sharing; deleting the capture will not remove that copy.';

export function CaptureDataPane({ session, onInputsAdded, inputOnly = false }: { session: CaptureSession; onInputsAdded?: (count: number) => void; inputOnly?: boolean }) {
  const client = useQueryClient(); const ws = useWorkspace();
  const collectorLabel = (id?: string) => {
    const fallback = id || 'Unknown collector';
    const source = session.targets?.find(target => target.collector_id === id)?.source;
    return source ? [source.cluster, source.namespace, source.pod].filter(Boolean).join(' / ') || fallback : fallback;
  };
  const sourceLabel = (stream: CaptureStream) => `${collectorLabel(stream.collector_id)} / ${stream.pipeline} / ${stream.point} (${stream.signal})`;
  const [watermark, setWatermark] = useState(0); const onWatermark = useCallback((w: number) => setWatermark(w), []); const [selected, setSelected] = useState<Record<number, CaptureStream>>({});
  const [receivers, setReceivers] = useState<Record<string, string>>({});
  const [error, setError] = useState(''); const [notice, setNotice] = useState(''); const [busy, setBusy] = useState(false);
  const [preparing, setPreparing] = useState('');
  const pending = useRef<AbortController>(undefined);
  useEffect(() => () => pending.current?.abort(), []);
  const [copyAction, setCopyAction] = useState<() => Promise<void>>(); const [deleting, setDeleting] = useState(false);
  const selectionCount = Object.keys(selected).length;
  const selectedStreams = [...new Map(Object.values(selected).map(stream => [sourceKey(stream), stream])).values()];
  const destinationSetup = scenarioSetup(ws.scenarioYAML);
  const destinationConfig = useDebounced(ws.configYAML, 250);
  const debouncedSetup = useDebounced(destinationSetup, 250);
  const destination = useQuery({
    queryKey: ['capture-destinations', destinationConfig, debouncedSetup],
    queryFn: ({ signal }) => api.graph({ config_yaml: destinationConfig, setup_yaml: debouncedSetup }, signal),
    enabled: session.data.enabled && selectionCount > 0 && !!destinationConfig.trim(),
    retry: false,
  });
  const destinationCurrent = destinationConfig === ws.configYAML && debouncedSetup === destinationSetup;
  const destinationValid = destinationCurrent && !destination.isError && destination.data?.valid === true;
  const signals = receiverSignals(destinationValid ? destination.data?.topology : undefined);
  const choices = (stream: CaptureStream) => [...signals].filter(([, accepted]) => accepted.includes(stream.signal ?? '')).map(([id]) => id).sort();
  const destinationReceiver = (stream: CaptureStream) => receivers[sourceKey(stream)] ?? (choices(stream).length === 1 ? choices(stream)[0] : '');
  const scenarioDraft = readScenario(ws.scenarioYAML.trim() ? ws.scenarioYAML : 'version: 1\ninputs: []\n');
  const draftProblem = 'error' in scenarioDraft ? scenarioDraft.error : '';
  const destinationsReady = !draftProblem && destinationValid && selectedStreams.every(stream => choices(stream).includes(destinationReceiver(stream)));
  async function action(fn: () => Promise<void>) { setBusy(true); setError(''); try { await fn(); await client.invalidateQueries({ queryKey: ['capture-session'] }); await client.invalidateQueries({ queryKey: ['captures'] }); } catch (e) { setError(e instanceof Error ? e.message : String(e)); } finally { setBusy(false); } }
  function selections() {
    if (!Object.keys(selected).length || !watermark || !ws.configYAML.trim()) throw new Error('Select received records and a current debugger configuration.');
    if (!destinationsReady) throw new Error('Choose a compatible destination receiver for every selected stream.');
    const groups = new Map<string, { stream: CaptureStream; sequences: number[]; watermark: number; receiver: string }>();
    for (const [sequence, stream] of Object.entries(selected)) { const key = sourceKey(stream); const destination = destinationReceiver(stream); const group = groups.get(key) ?? { stream, sequences: [], watermark, receiver: destination }; group.sequences.push(Number(sequence)); groups.set(key, group); }
    if (groups.size > 16) throw new Error('Select records from at most 16 source streams at once.');
    if ([...groups.values()].some(group => group.sequences.length > 1000)) throw new Error('Select at most 1,000 records from each source stream at once.');
    return [...groups.values()];
  }
  async function fixture(save = false): Promise<CaptureFixture> { const chosen = selections(); if (chosen.length !== 1) throw new Error('A saved fixture uses one source stream. Use Add selected to scenario to combine streams.'); return api.captureFixture(session.id, { selection: chosen[0], config_yaml: ws.configYAML, setup_yaml: destinationSetup, save }); }
  async function addToScenario() {
    const chosen = selections();
    const config = ws.configYAML; const setup = destinationSetup;
    const controller = new AbortController(); pending.current = controller;
    try {
      const fixtures: CaptureFixture[] = [];
      let bytes = 0;
      for (const [index, selection] of chosen.entries()) {
        setPreparing(`Preparing input ${index + 1} of ${chosen.length}…`);
        const result = await api.captureFixture(session.id, { selection, config_yaml: config, setup_yaml: setup }, controller.signal);
        bytes += new TextEncoder().encode(result.fixture_json).length;
        if (bytes > 4 * 1024 * 1024) throw new Error('The selected inputs exceed 4 MiB. Select fewer records and try again.');
        fixtures.push(result);
      }
      controller.signal.throwIfAborted();
      const latest = getWorkspace();
      if (latest.configYAML !== config || scenarioSetup(latest.scenarioYAML) !== setup) throw new Error('The debugger configuration or mocks changed while preparing inputs. Review the destination receivers and try again.');
      const patch = { scenarioYAML: appendCaptureInputs(latest.scenarioYAML, fixtures) };
      if (session.data.contains_raw) updateWorkspaceSessionOnly(patch); else updateWorkspace(patch);
      setNotice(`Added ${fixtures.length} input${fixtures.length === 1 ? '' : 's'} to your scenario. Existing inputs, mocks and assertions were preserved.`);
      onInputsAdded?.(fixtures.length);
    } catch (e) {
      if (!controller.signal.aborted) throw new Error(`Could not add captured records: ${e instanceof Error ? e.message : String(e)} Your scenario has not been changed.`, { cause: e });
    } finally { setPreparing(''); }
  }
  function sensitiveCopy(fn: () => Promise<void>) { setCopyAction(() => fn); }
  if (!session.data.enabled) return <p>Capture data ingress was not enabled for this session.</p>;
  return <section className="capture-data" aria-label="Received capture data">
    <div className="capture-metrics"><div><span>Records received</span><strong>{session.data.received_records.toLocaleString()}</strong></div><div><span>Data received</span><strong>{(session.data.received_bytes / 1024).toFixed(1)} <small>KiB</small></strong></div><div><span>Records rejected</span><strong>{session.data.rejected_records.toLocaleString()}</strong></div><div><span>Data status</span><strong className="capture-data-state">{session.data.state === 'open' ? 'Receiving' : session.data.state === 'frozen' ? 'Complete' : session.data.state.replaceAll('_', ' ')}</strong></div></div>
    <details className="capture-disclosure"><summary>Storage & capture limitations</summary><p>{storageNote(session.data)}</p></details>
    {session.data.contains_raw && <p role="status">Adding raw records as inputs keeps the open configuration and scenario in this browser session only. Saving, exporting or submitting a run retains a separate copy.</p>}
    {session.data.error && <p role="alert">Capture storage failed: {session.data.error}. Inspect receipt counts; enqueue counts do not prove receipt.</p>}
    <CaptureRecords session={session} selected={selected} onSelected={setSelected} onWatermark={onWatermark} guide={false} />
    {selectionCount > 0 && <div className="capture-use-selection"><div className="capture-selection-bar"><div><strong>{selectionCount} record{selectionCount === 1 ? '' : 's'} selected</strong><small>{selectedStreams.length} source stream{selectedStreams.length === 1 ? '' : 's'}</small></div>
      <div className="capture-actions"><button className="primary" disabled={busy || !destinationsReady} onClick={() => void action(addToScenario)}>{busy && preparing ? preparing : 'Add selected to scenario'}</button></div>
    </div>
    {draftProblem ? <p role="alert">Your scenario YAML cannot be read: {draftProblem}. <Link to="/" search={{ editor: 'yaml' }}>Fix the scenario YAML</Link> before adding captured inputs. Your existing draft has been kept.</p> : !ws.configYAML.trim() ? <p className="capture-notice"><strong>No debugger configuration is loaded in this browser.</strong> <Link to="/" search={{ editor: 'config' }}>Choose a configuration in the debugger</Link>, then return to these records. Adding inputs needs a receiver in that configuration.</p>
      : !destinationCurrent || destination.isPending ? <p role="status">Checking destination receivers…</p>
      : destination.error ? <p role="alert">Could not check destination receivers: {destination.error.message} <button onClick={() => void destination.refetch()}>Retry receiver check</button></p>
      : !destinationValid ? <div className="capture-notice is-error" role="alert"><strong>The debugger configuration or mocks cannot be used yet.</strong><ul>{(destination.data?.diagnostics ?? []).filter(item => item.severity === 'error').slice(0, 8).map((item, index) => <li key={index}>{item.message}{item.hint && ` ${item.hint}`}</li>)}</ul><Link to="/" search={{ editor: 'config' }}>Fix the configuration in the debugger</Link>. If it uses environment values or files, supply them on the debugger’s Mocks tab.</div>
      : <div className="capture-destinations" aria-label="Destination receivers">
        <p className="capture-help">Inputs append to your current scenario. Choose their destination receiver from the debugger configuration.</p>{!destinationsReady && <p className="capture-help">Available signals: {[...new Set([...signals.values()].flat())].join(', ') || 'none'}.</p>}
        {selectedStreams.map(stream => {
          const options = choices(stream); const chosen = destinationReceiver(stream); const invalid = !!chosen && !options.includes(chosen);
          return <div key={sourceKey(stream)}>
            <label>{stream.signal} input · {selectedStreams.length > 1 ? sourceLabel(stream) : stream.point}<select aria-label={`Receiver for ${sourceLabel(stream)}`} value={chosen} disabled={busy || !options.length} onChange={e => setReceivers(current => ({ ...current, [sourceKey(stream)]: e.target.value }))}>
              <option value="">{options.length ? 'Choose receiver' : `No receiver for ${stream.signal}`}</option>
              {invalid && <option value={chosen} disabled>{chosen} (incompatible)</option>}
              {options.map(id => <option key={id} value={id}>{id}</option>)}
            </select></label>
            {!options.length && <p role="alert">No receiver accepts {stream.signal} in the current debugger configuration. <Link to="/" search={{ editor: 'config' }}>Add a {stream.signal} pipeline in the debugger</Link>, or <button className="text-link" disabled={busy} onClick={() => setSelected(current => Object.fromEntries(Object.entries(current).filter(([, item]) => item.signal !== stream.signal)))}>Deselect {stream.signal} records</button>.</p>}
            {invalid && options.length > 0 && <p role="alert">Receiver {chosen} no longer accepts {stream.signal}. Choose a compatible receiver.</p>}
          </div>;
        })}
      </div>}
    {error && <p role="alert">{error}</p>}
    {notice && <p role="status">{notice} <Link to="/" search={{ editor: 'inputs' }}>Open scenario in debugger</Link></p>}
    {!inputOnly && <details className="capture-disclosure capture-selection-options"><summary>Save or download selected data</summary>
      <p className="capture-help">A fixture uses one source stream. These actions retain a separate copy; scenario editing happens in the debugger.</p>
      <div className="capture-actions"><button disabled={busy || selectedStreams.length !== 1 || !destinationsReady} onClick={() => sensitiveCopy(async () => { await fixture(true); setNotice('Fixture saved as a separate artifact on the server.'); })}>Save selected fixture…</button><button disabled={busy || selectedStreams.length !== 1 || !destinationsReady} onClick={() => sensitiveCopy(async () => { const result = await fixture(); downloadText(result.fixture_json, 'captured-input.json', 'application/json'); })}>Download selected fixture…</button><button disabled={busy || selectedStreams.length !== 1 || !destinationsReady} onClick={() => sensitiveCopy(async () => { const result = await fixture(); downloadText(result.setup_yaml, 'captured-input-setup.yaml', 'text/yaml'); })}>Download input setup…</button></div>
    </details>}</div>}
    {copyAction && <CaptureDialog title="Retain captured data" busy={busy} onCancel={() => setCopyAction(undefined)} onConfirm={() => void action(async () => { await copyAction(); setCopyAction(undefined); })} confirmLabel="Retain selected copy" cancelLabel="Cancel retained copy"><p>{CAPTURE_COPY_WARNING}</p></CaptureDialog>}
    {!inputOnly && <details className="capture-disclosure capture-delete-action"><summary>Manage retained data</summary><p className="capture-help">Delete the capture once it is stopped and delivery has finished. Saved sources and runs retain their own copies.</p><button disabled={busy || !session.terminal || session.data.state === 'draining' || session.data.state === 'open'} onClick={() => setDeleting(true)}>Delete capture…</button></details>}
    {deleting && <CaptureDialog title="Delete capture" danger busy={busy} onCancel={() => setDeleting(false)} onConfirm={() => void action(async () => { await api.deleteCapture(session.id); setDeleting(false); setNotice('Capture deleted.'); })} confirmLabel="Delete capture and payloads" cancelLabel="Keep capture"><p>Delete this capture and its received payloads? Explicitly saved sources and admitted runs retain their own copies.</p></CaptureDialog>}
    {!selectionCount && error && <p role="alert">{error}</p>}{!selectionCount && notice && <p role="status">{notice}</p>}
  </section>;
}

