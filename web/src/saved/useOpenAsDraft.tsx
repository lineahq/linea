import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';
import type { ArtifactMetadata } from '../api/types';
import { decodeText, kindDraft, openedPatch } from '../workspace/drafts';
import { getWorkspace, updateWorkspace } from '../workspace/store';
import { useReplaceDrafts } from '../workspace/useReplaceDrafts';
import { canOpenAsDraft } from './openDraft';
import { revisionQuery } from './useSavedArtifacts';

/**
 * useOpenAsDraft opens a saved revision as the configuration or scenario
 * draft and goes to the debugger. When that draft holds unsaved edits, it
 * first renders an inline confirmation (`confirm`, placed by the caller);
 * nothing is replaced until the user chooses to. The draft remembers the
 * revision it came from, so saving in the debugger makes a new revision of
 * the same artifact.
 */
export function useOpenAsDraft() {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const replacer = useReplaceDrafts({ after: () => void navigate({ to: '/' }) });
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  /** open reads the revision (the head by default) and opens it, or asks first. */
  const open = async (meta: ArtifactMetadata, revisionId = meta.head_revision_id) => {
    if (!canOpenAsDraft(meta.kind)) return;
    setBusy(true);
    setError(undefined);
    try {
      const data = await qc.fetchQuery(revisionQuery(meta.id, revisionId));
      const kind = meta.kind;
      const text = decodeText(data.content_base64 ?? '');
      replacer.request([kindDraft(kind)], `“${meta.label}” (rev ${data.revision.generation})`, () =>
        updateWorkspace(openedPatch(getWorkspace(), kind, text, meta.label, { artifactId: meta.id, revisionId })),
      );
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return { open, confirm: replacer.confirm, error, busy };
}
