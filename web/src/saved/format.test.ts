import { describe, expect, it } from 'vitest';
import { kindLabel, originRun, originText, plural, savedSection, shortId, whenText } from './format';

describe('shortId', () => {
  it.each([
    ['run-0123456789abcdef', 'run-01234567'],
    ['suite-0123456789abcdef', 'suite-01234567'],
    ['run-abc', 'run-abc'],
    ['custom', 'custom'],
  ])('%s', (id, want) => expect(shortId(id)).toBe(want));
});

describe('whenText', () => {
  const now = new Date(2026, 9, 23, 16, 0); // Oct 23 2026, local time
  it.each([
    [new Date(2026, 9, 23, 14, 5), 'Today 14:05'],
    [new Date(2026, 9, 22, 9, 30), 'Yesterday 09:30'],
    [new Date(2026, 9, 14, 8, 0), 'Oct 14'],
    [new Date(2025, 11, 1, 8, 0), 'Dec 1 2025'],
  ])('%s', (d, want) => expect(whenText(d.toISOString(), now)).toBe(want));
  it('returns text it cannot read', () => expect(whenText('soon', now)).toBe('soon'));
});

describe('labels', () => {
  it('names kinds', () => {
    expect(kindLabel('scenario')).toBe('Scenario');
    expect(kindLabel('other')).toBe('other');
  });
  it('pluralizes', () => {
    expect(plural(1, 'scenario')).toBe('1 scenario');
    expect(plural(3, 'scenario')).toBe('3 scenarios');
    expect(plural(0, 'scenario')).toBe('0 scenarios');
  });
});

describe('originText', () => {
  it.each([
    [undefined, 'written'],
    [{ kind: 'run', run_id: 'run-0123456789abcdef', base_digest: 'x' }, 'from run run-01234567'],
    [{ kind: 'run', base_digest: 'x' }, 'from a run'],
    [{ kind: 'candidate', base_digest: 'x' }, 'from a statement edit'],
    [{ kind: 'capture', base_digest: 'x' }, 'from a capture'],
    [{ kind: 'import', base_digest: 'x' }, 'from import'],
  ])('%j', (origin, want) => expect(originText(origin)).toBe(want));
  it('names the run only for run origins', () => {
    expect(originRun({ kind: 'run', run_id: 'run-1', base_digest: '' })).toBe('run-1');
    expect(originRun({ kind: 'capture', base_digest: '' })).toBeUndefined();
    expect(originRun(undefined)).toBeUndefined();
  });
});

describe('savedSection', () => {
  it.each([
    ['/saved', 'sources'],
    ['/saved/abc123', 'sources'],
    ['/saved/suites', 'suites'],
    ['/saved/suites/suite-1', 'suites'],
    ['/saved/suitesx', 'sources'],
    ['/saved/library', 'library'],
  ])('%s', (path, want) => expect(savedSection(path)).toBe(want));
});
