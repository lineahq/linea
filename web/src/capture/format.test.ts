import { describe, expect, it } from 'vitest';
import { clockTime, formatBytes, formatCount, relativeTime, shortId } from './format';

describe('formatCount', () => {
  it.each([[0, '0'], [999, '999'], [1204, '1,204'], [1234567, '1,234,567'], [4.6, '5']])('%s', (n, text) => expect(formatCount(n)).toBe(text));
});

describe('formatBytes', () => {
  it.each([
    [0, '0 B'],
    [512, '512 B'],
    [1536, '1.5 KiB'],
    [3_250_585, '3.1 MiB'],
    [200 * 1024 * 1024, '200 MiB'],
    [-1, '0 B'],
    [Number.NaN, '0 B'],
  ])('%s -> %s', (n, text) => expect(formatBytes(n)).toBe(text));
});

describe('relativeTime', () => {
  const now = Date.parse('2026-10-23T12:00:00Z');
  it.each([
    ['2026-10-23T12:00:00Z', 'now'],
    ['2026-10-23T11:59:56Z', '4 s ago'],
    ['2026-10-23T11:57:00Z', '3 min ago'],
    ['2026-10-23T10:00:00Z', '2 h ago'],
    ['2026-10-20T12:00:00Z', '3 d ago'],
    ['2026-10-23T12:00:30Z', 'now'],
    ['nonsense', '—'],
    [undefined, '—'],
  ])('%s -> %s', (iso, text) => expect(relativeTime(iso, now)).toBe(text));
});

describe('clockTime', () => {
  it('writes local time with padding and tolerates garbage', () => {
    expect(clockTime('2026-10-23T14:30:02')).toBe('14:30:02');
    expect(clockTime('nope')).toBe('');
    expect(clockTime(undefined)).toBe('');
  });
});

describe('shortId', () => {
  it('keeps the tail', () => {
    expect(shortId('capture-0123456789abcdef')).toBe('89abcdef');
    expect(shortId('short')).toBe('short');
  });
});

