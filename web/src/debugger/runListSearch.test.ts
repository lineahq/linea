import { describe, expect, it } from 'vitest';
import { NO_FILTER } from './runFacts';
import { filterFromSearch, parseRunListSearch, searchFromFilter } from './runListSearch';

describe('run list search', () => {
  it('reads each filter from the URL', () => {
    const f = filterFromSearch({ scenario: 'checkout', runtime: 'built-in', result: 'partial', mode: 'quick', q: 'error logs' });
    expect(f).toEqual({ scenario: 'checkout', runtime: 'built-in', result: 'partial', mode: 'quick', query: 'error logs' });
  });

  it('is the empty filter without a search', () => {
    expect(filterFromSearch({})).toEqual(NO_FILTER);
  });

  it('round-trips a filter and leaves no key for unset values', () => {
    const f = { ...NO_FILTER, scenario: 'a', result: 'failed' };
    const search = searchFromFilter(f);
    expect(search).toEqual({ scenario: 'a', runtime: undefined, result: 'failed', mode: undefined, q: undefined });
    expect(filterFromSearch(search)).toEqual(f);
    expect(Object.values(searchFromFilter(NO_FILTER)).every((v) => v === undefined)).toBe(true);
  });

  it('keeps a numeric-looking name as text', () => {
    // The router parses ?scenario=2026 into a number.
    expect(parseRunListSearch({ scenario: 2026 }).scenario).toBe('2026');
  });

  it('drops an unknown result and values of other types', () => {
    expect(parseRunListSearch({ result: 'exploded', scenario: { a: 1 }, runtime: '', q: true })).toEqual({
      scenario: undefined, runtime: undefined, result: undefined, mode: undefined, q: undefined,
    });
  });
});
