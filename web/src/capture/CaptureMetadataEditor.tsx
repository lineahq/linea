import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';
import type { CaptureSession, CaptureMetadataUpdate } from '../api/types';
import { CaptureDialog } from './CaptureDialog';

export function CaptureMetadataEditor({ session, buttonClassName }: { session: CaptureSession; buttonClassName?: string }) {
  const client = useQueryClient(); const [draft, setDraft] = useState<CaptureMetadataUpdate>();
  const [busy, setBusy] = useState(false); const [error, setError] = useState('');
  async function open() {
    setError(''); setBusy(true);
    try { const latest = await api.capture(session.id); setDraft({ revision: latest.metadata?.revision ?? 1, name: latest.metadata?.name ?? '', description: latest.metadata?.description ?? '' }); }
    catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    finally { setBusy(false); }
  }
  async function save() {
    if (!draft || busy) return; setBusy(true); setError('');
    try { const saved = await api.updateCaptureMetadata(session.id, draft); client.setQueryData(['capture-session', session.id], saved); await client.invalidateQueries({ queryKey: ['captures'] }); setDraft(undefined); }
    catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    finally { setBusy(false); }
  }
  return <><button className={buttonClassName} disabled={busy} onClick={() => void open()}>Edit details</button>{!draft && error && <p role="alert">{error}</p>}{draft && <CaptureDialog title="Edit capture details" busy={busy} confirmLabel="Save details" cancelLabel="Cancel" onConfirm={() => void save()} onCancel={() => { setDraft(undefined); setError(''); }}><form onSubmit={e => { e.preventDefault(); void save(); }}><fieldset disabled={busy} className="capture-editor-fields"><label>Name<input autoFocus maxLength={128} value={draft.name} placeholder="Optional capture name" onChange={e => setDraft({ ...draft, name: e.target.value })} /></label><label>Description<textarea rows={4} maxLength={2048} value={draft.description} onChange={e => setDraft({ ...draft, description: e.target.value })} /></label><p className="capture-help">Edit organizational details. Recorded telemetry, targets, capture settings and storage stay unchanged.</p>{error && <p role="alert">{error} Close and reopen this editor to load the latest details.</p>}</fieldset></form></CaptureDialog>}</>;
}
