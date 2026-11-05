import { describe, expect, it } from 'vitest';
import type { ArtifactRevision, ArtifactRevisionPage } from '../api/types';
import { clipText, historyOf, HISTORY_STEP, revisionNumber } from './history';

const rev = (id: string, generation: number): ArtifactRevision => ({
  ref: { artifact_id: 'a', revision_id: id, workspace_id: 'w' },
  generation,
  actor: 'local',
  content_bytes: 1,
  content_digest: 'd',
  created: '2026-10-23T10:00:00Z',
});

const page = (items: ArtifactRevision[], next?: string): ArtifactRevisionPage => ({
  artifact: { workspace_id: 'w', id: 'a', project_id: 'default', kind: 'scenario', label: 'a.yaml', generation: 3, head_revision_id: 'r3', created: '', updated: '' },
  items,
  next_cursor: next,
});

describe('historyOf', () => {
  it('joins pages newest first and says when older revisions remain', () => {
    const h = historyOf([page([rev('r3', 3), rev('r2', 2)], 'c')]);
    expect(h.revisions.map(revisionNumber)).toEqual([3, 2]);
    expect(h.more).toBe(true);
  });
  it('ends with the last page', () => {
    const h = historyOf([page([rev('r3', 3), rev('r2', 2)], 'c'), page([rev('r1', 1)])]);
    expect(h.revisions.map(revisionNumber)).toEqual([3, 2, 1]);
    expect(h.more).toBe(false);
  });
  it('lists a revision read twice once', () => {
    expect(historyOf([page([rev('r2', 2)], 'c'), page([rev('r2', 2), rev('r1', 1)])]).revisions).toHaveLength(2);
  });
  it('is empty before the first page', () => expect(historyOf([])).toEqual({ revisions: [], more: false }));
  it('reads a bounded step by default', () => expect(HISTORY_STEP).toBe(20));
});

describe('revisionNumber', () => {
  it('reads metadata and revision data alike', () => {
    expect(revisionNumber(rev('r', 4))).toBe(4);
    expect(revisionNumber({ revision: rev('r', 5), content_base64: '' })).toBe(5);
  });
});

describe('clipText', () => {
  it('keeps short text', () => expect(clipText('abc', 10)).toEqual({ text: 'abc', clipped: false }));
  it('clips long text', () => expect(clipText('abcdef', 3)).toEqual({ text: 'abc', clipped: true }));
});
