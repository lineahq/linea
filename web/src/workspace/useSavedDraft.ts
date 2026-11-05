import { useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../api/client';
import type { ArtifactError, ArtifactMetadata, SaveRequest } from '../api/types';
import { encodeText, kindDraft, kindText, loaded, newLabel, saveLabel, type DraftKind } from './drafts';
import { getWorkspace, updateWorkspace } from './store';

/**
 * useSavedDraft is the save side of a draft's saved artifact: the server
 * workspace's artifacts of the kind (none without a workspace), saving the
 * draft as a new revision of the artifact it was opened from, or as a new
 * artifact, and the conflict to resolve when that artifact has moved on.
 */
export function useSavedDraft(kind: DraftKind) {
  const qc = useQueryClient();
  const list = useQuery({ queryKey: ['artifacts', kind], queryFn: () => api.artifacts(kind), retry: false });
  const [conflict, setConflict] = useState<ArtifactMetadata>();
  const [message, setMessage] = useState<string>();
  const [busy, setBusy] = useState(false);
  const draft = kindDraft(kind);

  /** run does the work, showing its failure as the message. */
  const run = async (work: () => Promise<void>) => {
    setBusy(true);
    setMessage(undefined);
    try {
      await work();
    } catch (e) {
      setMessage(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  /**
   * save stores the draft: as a new revision of the artifact it was opened
   * from, unless asNew or it was opened from none. A non-blank label names
   * it; on a revision that renames the saved work. Without one a new
   * artifact is named by newLabel and a revision keeps its name.
   */
  const save = (asNew: boolean, label?: string) =>
    run(async () => {
      if (!list.data) return;
      const cur = getWorkspace();
      const source = cur.saved?.[kind];
      let req: SaveRequest = {
        workspace_id: list.data.workspace.id,
        kind,
        label: saveLabel(label, newLabel(cur, kind)),
        expected_generation: 0,
        content_base64: encodeText(kindText(cur, kind)),
      };
      if (source && !asNew) {
        const meta = await api.artifact(source.artifactId);
        if (meta.head_revision_id !== source.revisionId) {
          setConflict(meta);
          return;
        }
        req = { ...req, artifact_id: meta.id, label: saveLabel(label, meta.label), expected_generation: meta.generation, expected_head_revision_id: meta.head_revision_id };
      }
      try {
        const saved = await api.saveArtifact(req);
        const now = getWorkspace();
        updateWorkspace(loaded(now, { saved: { ...now.saved, [kind]: { artifactId: saved.artifact.id, revisionId: saved.revision.ref.revision_id } } }, [draft]));
        setConflict(undefined);
        setMessage(`Saved as revision ${saved.revision.ref.revision_id.slice(0, 8)} of “${saved.artifact.label}”.`);
        void qc.invalidateQueries({ queryKey: ['artifacts', kind] });
      } catch (e) {
        const current = e instanceof ApiError && e.status === 409 ? (e.body as ArtifactError | undefined)?.current : undefined;
        if (!current) throw e;
        setConflict(current);
      }
    });

  return { list, busy, message, setMessage, conflict, setConflict, run, save };
}
