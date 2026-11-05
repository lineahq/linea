import { useEffect, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';
import type { CapturePointPage, CaptureSelection, CaptureSession, CaptureValidation, Collector } from '../api/types';
import { CaptureMetadataEditor } from './CaptureMetadataEditor';
import { CaptureDialog } from './CaptureDialog';
import { CaptureDataPane } from './CaptureDataPane';
import type { GroupCaptureTargets } from './groupCapture';
import { CaptureSelectionForm } from './CaptureSelectionForm';
import { capturePhase, reasonText, sessionCounts, targetChip } from './phase';

export type CaptureView = 'new' | 'history';
export function CaptureControls({ inputOnly = false, onInputsAdded, draft, groupTargets, collector, points = [], view: controlledView, onViewChange, pointPaging, pointsLoading, pointsError, onKeepDraft }: {
  inputOnly?: boolean; onInputsAdded?: (count: number) => void; draft?: CaptureSelection; groupTargets?: GroupCaptureTargets; collector?: Collector; points?: NonNullable<CapturePointPage['items']>; view?: CaptureView; onViewChange?: (view: CaptureView) => void; pointPaging?: React.ReactNode; pointsLoading?: boolean; pointsError?: string; onKeepDraft?: (id?: string, group?: GroupCaptureTargets) => void;
}) {
  const client = useQueryClient();
  const [text, setText] = useState(''); const [baseline, setBaseline] = useState('');
  const [pages, setPages] = useState(['']); const [error, setError] = useState(''); const [busy, setBusy] = useState(false);
  const [validation, setValidation] = useState<CaptureValidation>(); const [selectedID, setSelectedID] = useState<string>();
  const [activeGroup, setActiveGroup] = useState<GroupCaptureTargets>();
  const [pendingGroup, setPendingGroup] = useState<GroupCaptureTargets>();
  const [starting, setStarting] = useState(false);
  const [missingID, setMissingID] = useState<string>();
  const [pendingDraft, setPendingDraft] = useState<CaptureSelection>(); const [localView, setLocalView] = useState<CaptureView>('new');
  const view = controlledView ?? localView;
  const changeView = (next: CaptureView) => { setLocalView(next); onViewChange?.(next); };
  const sessions = useQuery({ queryKey: ['captures', pages.at(-1)], queryFn: () => api.captures(pages.at(-1)), refetchInterval: 2000, refetchIntervalInBackground: false });
  const details = useQuery({ queryKey: ['capture-session', selectedID], queryFn: () => api.capture(selectedID!), enabled: !!selectedID, refetchInterval: 2000, refetchIntervalInBackground: false });
  useEffect(() => {
    if (!draft) return;
    if (text !== baseline) { setPendingDraft(draft); setPendingGroup(groupTargets); return; }
    setActiveGroup(groupTargets);
    const next = JSON.stringify(draft, null, 2); setText(next); setBaseline(next); setValidation(undefined);
    // Only deliberate collector changes propose replacing a draft.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draft]);
  useEffect(() => {
    if (view !== 'history' || selectedID || !sessions.data?.items?.length) return;
    const newest = [...sessions.data.items].filter(item => item.id !== missingID).sort((a, b) => b.created_at.localeCompare(a.created_at))[0];
    if (newest) setSelectedID(newest.id);
  }, [view, selectedID, sessions.data, missingID]);
  useEffect(() => { if (details.error && selectedID && (details.error as { status?: number }).status === 404) { setMissingID(selectedID); setSelectedID(undefined); client.removeQueries({ queryKey: ['capture-session', selectedID] }); } }, [details.error, selectedID, client]);
  async function action(fn: () => Promise<void>) {
    setBusy(true); setError('');
    try { await fn(); await client.invalidateQueries({ queryKey: ['captures'] }); await client.invalidateQueries({ queryKey: ['capture-session'] }); }
    catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    finally { setBusy(false); }
  }
  async function submit(start: boolean) {
    setStarting(start);
    await action(async () => {
      const req = JSON.parse(text) as CaptureSelection;
      if (start) { const session = await api.createCapture(req); client.setQueryData(['capture-session', session.id], session); setSelectedID(session.id); setBaseline(text); setValidation(undefined); changeView('history'); }
      else setValidation(await api.validateCapture(req));
    });
    setStarting(false);
  }
  const selected = details.data;
  const sessionItems = [...(sessions.data?.items ?? [])].sort((a, b) => b.created_at.localeCompare(a.created_at));
  return <div className="capture-workflow">
    <section className="capture-configuration capture-panel" hidden={view !== 'new'} aria-label="Configure capture">
      {pendingDraft && <CaptureDialog title="Replace capture settings?" onConfirm={() => { const next = JSON.stringify(pendingDraft, null, 2); setText(next); setBaseline(next); setActiveGroup(pendingGroup); setPendingGroup(undefined); setPendingDraft(undefined); setValidation(undefined); }} confirmLabel="Replace capture selection" cancelLabel="Keep capture selection" onCancel={() => { setPendingDraft(undefined); try { onKeepDraft?.(JSON.parse(baseline).collector_ids?.[0], activeGroup); } catch { /* retain the existing draft */ } }}><p>Switch capture targets and replace your edited capture settings?</p></CaptureDialog>}

      {!text && <div className="capture-welcome"><span className="capture-welcome-icon">↗</span><h2>Capture real telemetry</h2><p>Select a collector to see where you can collect data.</p><ol><li>Choose a connected collector or Capture group.</li><li>Select capture points and a duration.</li><li>Start the capture, then inspect the received records.</li></ol></div>}
      {!!text && <CaptureSelectionForm text={text} onChange={value => { setText(value); setValidation(undefined); }} groupTargets={activeGroup} collector={activeGroup ? undefined : collector} points={activeGroup?.points ?? points} busy={busy} starting={starting} disabled={!!pendingDraft} validation={validation} onValidate={() => void submit(false)} onStart={() => void submit(true)} pointPaging={pointPaging} pointsLoading={pointsLoading} pointsError={pointsError} />}
      {error && view === 'new' && <p className="capture-notice is-error" role="alert">{error}</p>}
    </section>
    <section className="capture-history-layout" hidden={view !== 'history'} aria-label="Capture history">
      <aside className="capture-panel capture-session-list"><div className="capture-section-title"><div><span className="capture-eyebrow">History</span><h2>Captures</h2></div><span className="capture-badge">{sessionItems.length}</span></div>
        {sessions.isPending && <p role="status">Loading captures…</p>}
        {sessions.error && <div className="capture-notice" role="alert"><p>{sessions.error.message}</p><button onClick={() => { setPages(['']); void sessions.refetch(); }}>Restart capture list</button></div>}
        {sessions.data && !sessionItems.length && <div className="capture-empty"><p>No captures yet.</p><button onClick={() => changeView('new')}>Create your first capture</button></div>}
        {sessionItems.map(session => <button className={`capture-session-row ${selectedID === session.id ? 'is-selected' : ''}`} key={session.id} aria-pressed={selectedID === session.id} onClick={() => setSelectedID(session.id)}><span className="capture-session-row-top"><strong>{session.metadata?.name || new Date(session.created_at).toLocaleTimeString()}</strong><span className={`capture-badge ${session.state === 'active' ? 'is-good' : session.uncertain_count || session.failed_count ? 'is-warning' : ''}`}>{session.state}</span></span><span>{session.target_count} collector{session.target_count === 1 ? '' : 's'} · {session.data?.received_records ?? 0} record{session.data?.received_records === 1 ? '' : 's'}</span><small>{session.id.slice(-8)}</small></button>)}
        {(pages.length > 1 || sessions.data?.next_cursor) && <nav aria-label="Capture session pages"><button disabled={pages.length === 1} onClick={() => setPages(p => p.slice(0, -1))}>Previous captures</button><span>{pages.length}</span><button disabled={!sessions.data?.next_cursor} onClick={() => { const next = sessions.data?.next_cursor; if (next) setPages(p => [...p, next]); }}>Next captures</button></nav>}
      </aside>
      <div className="capture-panel capture-result-panel">
        {!selectedID && <div className="capture-welcome"><h2>Your captured telemetry appears here</h2><p>Choose a capture to inspect its records and use them in the debugger.</p></div>}
        {selectedID && details.isPending && <p role="status">Loading capture details…</p>}
        {details.error && <p className="capture-notice is-error" role="alert">{details.error.message}</p>}
        {selected && <><div className="capture-section-title capture-result-heading"><div><span className="capture-eyebrow">3 · Inspect & use</span><h2>{selected.metadata?.name || `Capture at ${new Date(selected.created_at).toLocaleTimeString()}`}</h2>{selected.metadata?.description && <p className="capture-description">{selected.metadata.description}</p>}<p>{(selected.targets?.length ?? 0) > 1 ? `${(selected.targets?.length ?? 0)} collectors` : (selected.targets ?? []).map(target => target.source?.pod || target.collector_id).join(', ')} · {(selected.selection.signals ?? []).join(', ')}</p></div><div className="capture-actions">{!inputOnly && <CaptureMetadataEditor session={selected} />}<span className={`capture-badge ${selected.state === 'active' ? 'is-good' : selected.state === 'partial' ? 'is-warning' : ''}`}>{selected.state}</span>{!selected.terminal && <button className="capture-danger" disabled={busy || selected.state === 'stopping'} onClick={() => void action(async () => { const stopped = await api.stopCapture(selected.id); client.setQueryData(['capture-session', selected.id], stopped); })}>{selected.state === 'stopping' ? 'Stopping…' : 'Stop capture'}</button>}</div></div>
          {!selected.terminal && <p className="capture-live-note" role="status"><span className="capture-status-dot" />{selected.state === 'pending' ? 'Waiting for the collector to confirm activation…' : selected.state === 'stopping' ? 'Waiting for the collector to confirm its stop…' : 'Capture is running. Status updates automatically.'} Stops by {new Date(selected.expires_at).toLocaleTimeString()}.</p>}
          {selected.terminal && <p className="capture-help">{capturePhase(sessionCounts(selected)).headline ?? reasonText(selected.reason || selected.stop_reason) ?? 'The capture has ended.'}</p>}
          {error && <p className="capture-notice is-error" role="alert">{error}</p>}
          {(selected.targets?.length ?? 0) > 1 && <ul className="capture-target-list" aria-label="Collector capture status">{(selected.targets ?? []).map(target => <li key={target.collector_id}><div><strong>{target.source?.pod || target.collector_id}</strong><small>{[target.source?.cluster, target.source?.namespace].filter(Boolean).join(' / ')}</small></div><span className="capture-badge">{targetChip(target).label}</span></li>)}</ul>}
          {selected.data?.enabled ? <CaptureDataPane key={selected.id} session={selected} inputOnly={inputOnly} onInputsAdded={onInputsAdded} /> : <p className="capture-empty">Live capture data was unavailable for this capture, so no records were stored.</p>}
          {!inputOnly && <CaptureDiagnostics session={selected} />}
        </>}
      </div>
    </section>
  </div>;
}

function CaptureDiagnostics({ session }: { session: CaptureSession }) {
  return <details className="capture-disclosure capture-diagnostics"><summary>Session diagnostics <span>Collector status, provenance & request</span></summary><p className="capture-help">Session ID: <code>{session.id}</code>. Enqueued counters do not prove telemetry was received.{(session.reason || session.stop_reason) && <> Reason: {reasonText(session.reason || session.stop_reason)}</>}</p>
    <div className="collectors-table"><table><thead><tr><th>Collector</th><th>State</th><th>Selected</th><th>Enqueued</th><th>Filter errors</th></tr></thead><tbody>{(session.targets ?? []).map(target => <tr key={target.collector_id}><td>{target.source?.pod || target.collector_id}</td><td>{target.state}</td><td>{target.stats?.selected ?? '—'}</td><td>{target.stats?.enqueued_records ?? '—'}</td><td>{target.stats?.filter_errors ?? '—'}</td></tr>)}</tbody></table></div>
    {(session.targets ?? []).map(target => <div key={target.collector_id}><p>{target.source && <>Admission source: {[target.source.cluster, target.source.namespace, target.source.pod].filter(Boolean).join('/')} · group {target.source.collector_group} · transport {target.source.transport_security || 'unknown'} · authentication {target.source.authentication || 'unknown'} · reported Collector {target.source.collector_version}, distribution {target.source.distribution_version || 'unknown'}.</>}{target.agent_id && <> Agent {target.agent_id}, generation {target.generation}.</>}</p><p>Dropped: {Object.entries(target.stats?.dropped ?? {}).map(([reason, count]) => `${reason}: ${count}`).join(', ') || '0'}.</p></div>)}
    <pre className="capture-json">{JSON.stringify({ selection: session.selection, requested_limits: session.requested_limits }, null, 2)}</pre>
  </details>;
}
