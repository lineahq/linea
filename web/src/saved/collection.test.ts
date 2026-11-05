import { describe, expect, it } from 'vitest';
import type { ArtifactMetadata } from '../api/types';
import {
  filterCounts,
  filterItems,
  isTest,
  paginate,
  profileTargets,
  profilesComplete,
  scenarioScope,
  unscopedScenarios,
  selectedMembers,
  setSelected,
  toggleSelected,
  type Profiles,
} from './collection';

const meta = (id: string, kind: string, label: string, updated: string): ArtifactMetadata => ({
  id, project_id: 'default', kind, label, updated, created: updated, generation: 1, head_revision_id: `${id}-r1`, workspace_id: 'w',
});

const ITEMS = [
  meta('a1', 'configuration', 'checkout.yaml', '2026-10-20T10:00:00Z'),
  meta('a2', 'scenario', 'debug-error-logs', '2026-10-23T10:00:00Z'),
  meta('a3', 'scenario', 'smoke', '2026-10-22T10:00:00Z'),
  meta('a4', 'fixture', 'payload.json', '2026-10-21T10:00:00Z'),
  meta('a5', 'manifest', 'builder.yaml', '2026-10-19T10:00:00Z'),
];

const profiles = (entries: Record<string, number | undefined>): Profiles =>
  new Map(Object.entries(entries).map(([id, assertions]) => [id, { origin: 'written', assertions }]));

describe('tests', () => {
  const p = profiles({ a2: 3, a3: 0 });
  it('are scenarios with assertions', () => {
    expect(ITEMS.filter((m) => isTest(m, p)).map((m) => m.id)).toEqual(['a2']);
  });
  it('count only once every scenario is read', () => {
    expect(profilesComplete(ITEMS, p)).toBe(true);
    expect(filterCounts(ITEMS, p).test).toBe(1);
    expect(filterCounts(ITEMS, profiles({ a2: 3 })).test).toBeUndefined();
    expect(filterCounts(ITEMS, profiles({ a2: 3, a3: undefined })).test).toBeUndefined();
    const unreadable: Profiles = new Map([['a2', { origin: 'written', assertions: 3 }], ['a3', { origin: 'written', unreadable: true }]]);
    expect(filterCounts(ITEMS, unreadable).test).toBe(1);
  });
  it('are a filter of the scenarios', () => {
    expect(filterItems(ITEMS, 'test', '', p).map((m) => m.id)).toEqual(['a2']);
    expect(filterItems(ITEMS, 'scenario', '', p).map((m) => m.id)).toEqual(['a2', 'a3']);
  });
});

describe('filterCounts', () => {
  it('counts each kind', () => {
    const c = filterCounts(ITEMS, profiles({ a2: 1, a3: 0 }));
    expect(c).toMatchObject({ all: 5, configuration: 1, scenario: 2, test: 1, fixture: 1, setup: 0, manifest: 1 });
  });
});

describe('filterItems', () => {
  const p = profiles({ a2: 1, a3: 0 });
  it('orders newest first', () => {
    expect(filterItems(ITEMS, 'all', '', p).map((m) => m.id)).toEqual(['a2', 'a3', 'a4', 'a1', 'a5']);
  });
  it.each([
    ['ERROR', ['a2']],
    ['yaml', ['a1', 'a5']],
    ['a4', ['a4']],
    ['fixture', ['a4']],
    ['  ', ['a2', 'a3', 'a4', 'a1', 'a5']],
    ['nothing', []],
  ])('searches %j', (q, want) => expect(filterItems(ITEMS, 'all', q, p).map((m) => m.id)).toEqual(want));
});

describe('paginate', () => {
  const rows = Array.from({ length: 53 }, (_, i) => i);
  it.each([
    [1, 1, 1, 25],
    [3, 3, 51, 53],
    [9, 3, 51, 53],
    [0, 1, 1, 25],
  ])('page %i', (page, wantPage, from, to) => {
    const v = paginate(rows, page);
    expect(v).toMatchObject({ page: wantPage, pages: 3, from, to, total: 53 });
    expect(v.rows).toHaveLength(to - from + 1);
  });
  it('handles an empty list', () => {
    expect(paginate([], 1)).toEqual({ rows: [], page: 1, pages: 1, from: 0, to: 0, total: 0 });
  });
});

describe('selection', () => {
  it('toggles and sets without mutating', () => {
    const none = new Set<string>();
    const one = toggleSelected(none, 'a2');
    expect([...none]).toEqual([]);
    expect([...one]).toEqual(['a2']);
    expect([...toggleSelected(one, 'a2')]).toEqual([]);
    expect([...setSelected(one, ['a3', 'a4'], true)].sort()).toEqual(['a2', 'a3', 'a4']);
    expect([...setSelected(one, ['a2'], false)]).toEqual([]);
  });
  it('keeps only existing scenarios, in list order', () => {
    const got = selectedMembers(ITEMS, new Set(['a1', 'a3', 'a2', 'gone']));
    expect(got.map((m) => m.id)).toEqual(['a2', 'a3']);
  });
});

describe('profileTargets', () => {
  it('reads every scenario up to the limit plus the page rows', () => {
    expect(profileTargets(ITEMS, [ITEMS[0]]).map((m) => m.id).sort()).toEqual(['a1', 'a2', 'a3']);
  });
  it('bounds the scenarios, newest first', () => {
    expect(profileTargets(ITEMS, [], 1).map((m) => m.id)).toEqual(['a2']);
  });
});

describe('scope', () => {
  it('checks the newest scenarios only', () => {
    expect(scenarioScope(ITEMS, 1).map((m) => m.id)).toEqual(['a2']);
    expect(unscopedScenarios(ITEMS, 1)).toBe(1);
    expect(unscopedScenarios(ITEMS)).toBe(0);
  });
  it('is complete once the scoped scenarios are read', () => {
    expect(profilesComplete(ITEMS, profiles({ a2: 1 }), 1)).toBe(true);
    expect(profilesComplete(ITEMS, profiles({}), 1)).toBe(false);
  });
});
