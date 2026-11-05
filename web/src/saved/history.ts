import type { ArtifactRevision, ArtifactRevisionData, ArtifactRevisionPage } from '../api/types';

/** Revisions read per page of an artifact's history. */
export const HISTORY_STEP = 20;

export interface History {
  /** Newest first, as the server lists them. */
  revisions: ArtifactRevision[];
  /** The server has older revisions than those read. */
  more: boolean;
}

/**
 * historyOf joins the revision pages read so far (GET
 * /artifacts/{id}/revisions), newest first. Pages stay stable while the
 * artifact gains revisions, so a revision read twice is listed once.
 */
export function historyOf(pages: ArtifactRevisionPage[]): History {
  const seen = new Set<string>();
  const revisions: ArtifactRevision[] = [];
  for (const page of pages) {
    for (const r of page.items ?? []) {
      if (seen.has(r.ref.revision_id)) continue;
      seen.add(r.ref.revision_id);
      revisions.push(r);
    }
  }
  return { revisions, more: Boolean(pages[pages.length - 1]?.next_cursor) };
}

/** revisionNumber is the "rev N" of a revision. */
export function revisionNumber(data: ArtifactRevisionData | ArtifactRevision): number {
  return 'revision' in data ? data.revision.generation : data.generation;
}

/** clipText shortens long content for display, saying what was left out. */
export function clipText(text: string, max = 100_000): { text: string; clipped: boolean } {
  return text.length <= max ? { text, clipped: false } : { text: text.slice(0, max), clipped: true };
}
