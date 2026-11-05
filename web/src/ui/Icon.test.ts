import { describe, expect, it } from 'vitest';
import { ICON_NAMES, ICONS } from './Icon';

describe('Icon set', () => {
  it('gives every icon name a non-empty body', () => {
    expect(ICON_NAMES.length).toBeGreaterThan(0);
    for (const name of ICON_NAMES) {
      const body = ICONS[name].body;
      expect(body, name).toBeTruthy();
      expect(typeof body, name).toBe('object');
    }
  });

  it('defines the left navigation rail aliases', () => {
    for (const name of [
      'logo',
      'nav-debugger',
      'nav-capture',
      'nav-fleet',
      'nav-runtimes',
      'nav-saved',
      'nav-help',
      'nav-settings',
    ] as const) {
      expect(ICON_NAMES).toContain(name);
    }
  });

  it('keeps the rail logo on its own 16x16 grid', () => {
    expect(ICONS.logo.viewBox).toBe('0 0 16 16');
    expect(ICONS.logo.strokeWidth).toBe(1.5);
  });

  it('uses kebab-case names without duplicates', () => {
    expect(new Set(ICON_NAMES).size).toBe(ICON_NAMES.length);
    for (const name of ICON_NAMES) expect(name).toMatch(/^[a-z0-9]+(-[a-z0-9]+)*$/);
  });
});
