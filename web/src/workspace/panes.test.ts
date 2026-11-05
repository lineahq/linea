import { describe, expect, it } from 'vitest';
import { clampSplit, DEFAULT_PANES, readPanes, splitBounds } from './panes';

describe('pane preferences', () => {
  it('loads only valid ratios and survives unavailable or corrupt storage', () => {
    expect(readPanes('{"left":0.45,"upper":0.3}')).toEqual({ left: 0.45, upper: 0.3 });
    for (const raw of [null, '{', 'null', '[]', '{"left":-1,"upper":0.4}', '{"left":0.4,"upper":"0.3"}', '{"left":1,"upper":0.5}']) {
      expect(readPanes(raw)).toEqual(DEFAULT_PANES);
    }
    expect(readPanes(null, { left: 0.3, upper: 0.25 })).toEqual({ left: 0.3, upper: 0.25 });
  });
  it('reserves both minimum sizes and clamps extreme drags', () => {
    const bounds = splitBounds(1008, 260, 420);
    expect(bounds.min).toBeCloseTo(0.26);
    expect(bounds.max).toBeCloseTo(0.58);
    expect(clampSplit(-1, 1008, 260, 420)).toBe(bounds.min);
    expect(clampSplit(2, 1008, 260, 420)).toBe(bounds.max);
    expect(clampSplit(0.4, 1008, 260, 420)).toBe(0.4);
  });
  it('scales minimums on a small screen without negative tracks', () => {
    const bounds = splitBounds(408, 260, 420);
    expect(bounds.min).toBeCloseTo(260 / 680);
    expect(bounds.max).toBeCloseTo(bounds.min);
    expect(clampSplit(0.9, 0, 260, 420)).toBe(0.5);
  });
});
