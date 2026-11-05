import { describe, expect, it } from 'vitest';
import { initialTargetMode, parseNewCaptureSearch } from './newCaptureSearch';

describe('parseNewCaptureSearch', () => {
  it.each([
    ['nothing', {}, {}],
    ['an array of collectors', { collectors: ['a', 'b'] }, { collectors: ['a', 'b'] }],
    ['a comma list', { collectors: 'a, b,,a' }, { collectors: ['a', 'b'] }],
    ['numeric IDs from the router parser', { collectors: [1, 2] }, { collectors: ['1', '2'] }],
    ['blank values', { collectors: ['', '  '], group: ' ', from: '' }, {}],
    ['a group', { group: 'g1' }, { group: 'g1' }],
    ['a capture to copy', { from: 'cap-1' }, { from: 'cap-1' }],
    ['junk types', { collectors: { a: 1 }, group: ['x'], from: true }, {}],
  ])('%s', (_name, raw, expected) => {
    expect(parseNewCaptureSearch(raw)).toEqual(expected);
  });

  it('keeps at most 16 collectors', () => {
    const many = Array.from({ length: 30 }, (_, i) => `c${i}`);
    expect(parseNewCaptureSearch({ collectors: many }).collectors).toHaveLength(16);
  });
});

describe('initialTargetMode', () => {
  it('prefers copying a capture, then a group, then picking', () => {
    expect(initialTargetMode({ from: 'x', group: 'g', collectors: ['a'] })).toBe('from');
    expect(initialTargetMode({ group: 'g', collectors: ['a'] })).toBe('group');
    expect(initialTargetMode({ collectors: ['a'] })).toBe('pick');
    expect(initialTargetMode({})).toBe('pick');
  });
});
