import { useMemo } from 'react';
import { useQueries, useQuery } from '@tanstack/react-query';
import { api, ApiError } from '../api/client';
import type { ArtifactMetadata, ArtifactPage, ArtifactRevisionData } from '../api/types';
import { decodeText } from '../workspace/drafts';
import type { Profile, Profiles } from './collection';
import { originText } from './format';
import { assertionCount } from './scenarioView';

/** Artifacts are read 100 at a time (the API's largest page) up to this many. */
const PAGE = 100;
const MAX_PAGES = 20;

export interface SavedArtifacts {
  workspace: ArtifactPage['workspace'];
  items: ArtifactMetadata[];
  /** More artifacts exist than were read. */
  truncated: boolean;
}

async function loadAll(): Promise<SavedArtifacts> {
  const items: ArtifactMetadata[] = [];
  let workspace: ArtifactPage['workspace'] | undefined;
  let cursor = '';
  for (let i = 0; i < MAX_PAGES; i++) {
    const page = await api.artifacts('', cursor, PAGE);
    workspace ??= page.workspace;
    items.push(...(page.items ?? []));
    if (!page.next_cursor) return { workspace, items, truncated: false };
    cursor = page.next_cursor;
  }
  return { workspace: workspace!, items, truncated: true };
}

/** savedArtifactsKey is the query key of the whole collection; invalidate it after a save. */
export const savedArtifactsKey = ['saved', 'artifacts'] as const;

/** useSavedArtifacts reads every artifact the server has saved. */
export function useSavedArtifacts(enabled = true) {
  return useQuery({ queryKey: savedArtifactsKey, queryFn: loadAll, retry: false, enabled });
}

/**
 * noWorkspace: the server cannot keep or list saved work, so nothing can be
 * saved or listed (the artifact endpoints answer 503, "unavailable" or
 * "workspace_required").
 */
export function noWorkspace(error: unknown): boolean {
  return error instanceof ApiError && error.status === 503;
}

/** revisionQuery reads one revision. Revisions are immutable, so it never goes stale. */
export const revisionQuery = (artifactId: string, revisionId: string) => ({
  queryKey: ['saved', 'revision', artifactId, revisionId] as const,
  queryFn: () => api.artifactRevision(artifactId, revisionId),
  staleTime: Infinity,
  retry: false,
});

/**
 * useProfiles reads the head revision of each target (bounded by the caller,
 * see collection.profileTargets) and derives its origin and, for scenarios,
 * its assertion count. A revision that has not loaded has no profile yet.
 */
export function useProfiles(targets: ArtifactMetadata[]): Profiles {
  const results = useQueries({
    queries: targets.map((m) => revisionQuery(m.id, m.head_revision_id)),
    combine: (rs) => rs.map((r) => r.data),
  });
  return useMemo(() => {
    const out = new Map<string, Profile>();
    targets.forEach((m, i) => {
      const data = results[i];
      if (data) out.set(m.id, profileOf(m.kind, data));
    });
    return out;
  }, [targets, results]);
}

// A revision never changes, so its profile is derived once per loaded revision.
const derived = new WeakMap<object, Profile>();

function profileOf(kind: string, data: ArtifactRevisionData): Profile {
  const known = derived.get(data);
  if (known) return known;
  const profile: Profile = { origin: originText(data.revision.origin) };
  if (kind === 'scenario') {
    try {
      const n = assertionCount(decodeText(data.content_base64 ?? ''));
      if (n === undefined) profile.unreadable = true;
      else profile.assertions = n;
    } catch {
      profile.unreadable = true;
    }
  }
  derived.set(data, profile);
  return profile;
}
