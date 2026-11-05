import { describe, expect, it } from 'vitest';
import { parsePreference, resolveTheme } from './theme';

describe('theme preference', () => {
  it.each([
    ['light', 'light'],
    ['dark', 'dark'],
    ['system', 'system'],
    [null, 'system'],
    ['purple', 'system'],
  ] as const)('parses %s as %s', (stored, want) => {
    expect(parsePreference(stored)).toBe(want);
  });

  it.each([
    ['system', true, 'dark'],
    ['system', false, 'light'],
    ['light', true, 'light'],
    ['dark', false, 'dark'],
  ] as const)('resolves %s (system dark %s) to %s', (pref, dark, want) => {
    expect(resolveTheme(pref, dark)).toBe(want);
  });
});
