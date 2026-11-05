import { describe, expect, it } from 'vitest';
import { capOptions, pageOf } from './pages';

const list = Array.from({ length: 60 }, (_, i) => i);

describe('pages', () => {
  it('cuts a list into pages of 25', () => {
    expect(pageOf(list, 0)).toMatchObject({ page: 0, pages: 3, start: 0 });
    expect(pageOf(list, 0).shown).toHaveLength(25);
    expect(pageOf(list, 2).shown).toEqual([50, 51, 52, 53, 54, 55, 56, 57, 58, 59]);
  });
  it('clamps a page past the end to the last one', () => {
    expect(pageOf(list, 9)).toMatchObject({ page: 2, start: 50 });
    expect(pageOf(list, -1).page).toBe(0);
  });
  it('has one empty page for an empty list', () => {
    expect(pageOf([], 3)).toEqual({ shown: [], page: 0, pages: 1, start: 0 });
  });
  it('caps picker options and counts the rest', () => {
    expect(capOptions(list, 50)).toEqual({ shown: list.slice(0, 50), hidden: 10 });
    expect(capOptions(list).hidden).toBe(0);
  });
});
