import { describe, expect, it } from 'vitest';
import type { RunPage } from '../api/client';
import type { Run } from '../api/types';
import { dedupeRuns, firstPageRuns, flattenRunPages, hasMoreRuns, runCountText, runsCountText, withRuns } from './runPages';

const run = (id: string, state = 'done') => ({ id, state, created: '2026-10-30T00:00:00Z' }) as unknown as Run;
const page = (ids: string[], next?: string): RunPage => ({ runs: ids.map((id) => run(id)), next_cursor: next });

describe('flattenRunPages', () => {
  it('joins pages in order', () => {
    expect(flattenRunPages([page(['c', 'b'], 'x'), page(['a'])]).map((r) => r.id)).toEqual(['c', 'b', 'a']);
  });

  it('keeps the first occurrence of a run that shifted onto the next page', () => {
    const first = { runs: [run('d', 'running'), run('c')], next_cursor: 'x' };
    const second = { runs: [run('c', 'stale'), run('b')] };
    const out = flattenRunPages([first, second]);
    expect(out.map((r) => r.id)).toEqual(['d', 'c', 'b']);
    expect(out.find((r) => r.id === 'c')?.state).toBe('done');
  });

  it('copes with no pages and with a page without runs', () => {
    expect(flattenRunPages(undefined)).toEqual([]);
    expect(flattenRunPages([{}, page(['a'])]).map((r) => r.id)).toEqual(['a']);
  });
});

describe('dedupeRuns', () => {
  it('prefers the earlier list', () => {
    expect(dedupeRuns([run('a', 'one'), run('a', 'two')]).map((r) => r.state)).toEqual(['one']);
  });
});

describe('first page helpers', () => {
  it('reads the first page', () => {
    expect(firstPageRuns(undefined)).toEqual([]);
    expect(firstPageRuns(page(['a', 'b'])).length).toBe(2);
    expect(hasMoreRuns(page(['a'], 'c'))).toBe(true);
    expect(hasMoreRuns(page(['a']))).toBe(false);
    expect(hasMoreRuns({ runs: [], next_cursor: '' })).toBe(false);
  });

  it('marks a count as a lower bound while older runs follow', () => {
    expect(runCountText(undefined)).toBe('0');
    expect(runCountText(page(['a', 'b']))).toBe('2');
    expect(runCountText(page(['a', 'b'], 'c'))).toBe('2+');
  });
});

describe('runsCountText', () => {
  it('words the count of the list', () => {
    expect(runsCountText(1, false, false)).toBe('1 run');
    expect(runsCountText(3, false, false)).toBe('3 runs');
    expect(runsCountText(50, true, false)).toBe('50+ runs');
  });

  it('words the count of a filtered list', () => {
    expect(runsCountText(1, false, true)).toBe('1 run matches');
    expect(runsCountText(0, false, true)).toBe('0 runs match');
    expect(runsCountText(2, true, true)).toBe('2+ runs match');
  });
});

describe('withRuns', () => {
  const at = (id: string, created: string) => ({ id, state: 'done', created }) as unknown as Run;
  const list = [at('c', '2026-10-30T03:00:00Z'), at('b2', '2026-10-30T02:00:00Z'), at('b1', '2026-10-30T02:00:00Z')];

  it('returns the list itself when nothing is added', () => {
    expect(withRuns(list, [undefined, list[0]])).toBe(list);
  });

  it('places older runs newest first and keeps same-instant runs in list order', () => {
    const out = withRuns(list, [at('a', '2026-10-29T00:00:00Z'), undefined]);
    expect(out.map((r) => r.id)).toEqual(['c', 'b2', 'b1', 'a']);
  });
});
