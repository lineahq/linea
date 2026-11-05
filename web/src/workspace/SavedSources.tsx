import { useState } from 'react';
import { api } from '../api/client';
import type { ArtifactMetadata } from '../api/types';
import {
  decodeText,
  kindDraft,
  kindText,
  openedPatch,
  type DraftKind,
  type SavedSource,
} from './drafts';
import { getWorkspace, updateWorkspace, type WorkspaceState } from './store';
import { useReplaceDrafts } from './useReplaceDrafts';
import { useSavedDraft } from './useSavedDraft';

interface Opened {
  text: string;
  label: string;
  source: SavedSource;
}

async function readRevision(meta: ArtifactMetadata, revision = meta.head_revision_id): Promise<Opened> {
  const data = await api.artifactRevision(meta.id, revision);
  return { text: decodeText(data.content_base64 ?? ''), label: meta.label, source: { artifactId: meta.id, revisionId: revision } };
}

interface Props {
  ws: WorkspaceState;
  kind: DraftKind;
  /** Saving is offered elsewhere (the workbench header's Save menu). */
  hideSave?: boolean;
}

/**
 * SavedSources opens a draft from the server's saved work and saves it there
 * as a new revision. It shows nothing when the server cannot list saved work. Saving an edit
 * names the revision it was opened from; when the artifact has moved on, the
 * current head is shown with the choice to load it or save as a new artifact.
 */
export function SavedSources({ ws, kind, hideSave }: Props) {
  const { list, busy, message, conflict, setConflict, run, save } = useSavedDraft(kind);
  const [open, setOpen] = useState(false);
  const replacer = useReplaceDrafts({
    compact: true,
    after: () => {
      setConflict(undefined);
      setOpen(false);
    },
  });
  if (!list.data) return null; // Saved work unavailable (503), or not loaded yet.
  const draft = kindDraft(kind);
  const source = ws.saved?.[kind];

  // Opening a revision replaces the draft; `skipAsk` is for a revision the user already chose to load.
  const commit = (o: Opened, skipAsk = false) =>
    replacer.request([draft], `“${o.label}”`, () => updateWorkspace(openedPatch(getWorkspace(), kind, o.text, o.label, o.source)), { skipAsk });
  const pick = (meta: ArtifactMetadata) => run(async () => commit(await readRevision(meta)));

  const items = list.data.items ?? [];
  return (
    <span className="saved-sources">
      <button type="button" disabled={busy} aria-expanded={open} onClick={() => setOpen(!open)} title="Open a revision saved on this server">
        Saved…
      </button>
      {!hideSave && (
        <button type="button" disabled={busy || !kindText(ws, kind).trim()} onClick={() => void save(false)}
          title={source ? 'Save as a new revision of the saved item it was opened from' : 'Save in this server’s saved work'}>
          Save
        </button>
      )}
      {open && (
        <ul className="saved-list" aria-label="Saved artifacts">
          {items.length === 0 && <li className="muted">Nothing saved yet.</li>}
          {items.map((m) => (
            <li key={m.id}>
              <button type="button" className="link" disabled={busy} onClick={() => void pick(m)}>
                {m.label || m.id}
              </button>{' '}
              <span className="muted small">
                revision {m.generation} · {new Date(m.updated).toLocaleString()}
              </span>
            </li>
          ))}
        </ul>
      )}
      {replacer.confirm}
      {conflict && (
        <span className="confirm-inline" role="alertdialog" aria-label="Saved artifact changed">
          <span>
            “{conflict.label}” changed since you opened it: its head is revision {conflict.generation}, saved {new Date(conflict.updated).toLocaleString()}.
          </span>
          <button type="button" disabled={busy} onClick={() => void run(async () => commit(await readRevision(conflict), true))}>Load it</button>
          <button type="button" disabled={busy} onClick={() => void save(true)}>Save as new</button>
        </span>
      )}
      {message && <span className="muted small" role="status">{message}</span>}
    </span>
  );
}
