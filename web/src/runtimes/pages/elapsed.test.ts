import { describe, expect, it } from 'vitest';
import { formatElapsed } from './elapsed';

describe('formatElapsed', () => {
  it.each([
    [0, '0:00'],
    [999, '0:00'],
    [76_000, '1:16'],
    [3_599_000, '59:59'],
    [3_723_000, '1:02:03'],
    [-5000, '0:00'],
  ])('prints %i ms as %s', (ms, want) => {
    expect(formatElapsed(ms)).toBe(want);
  });
});
