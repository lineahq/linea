import { describe, expect, it } from 'vitest';
import { diffValue, fieldLines, inlineDiff, otlpValue } from './structuredDiff';

describe('structured diff lines', () => {
  it('marks only the changed part of a string while reconstructing both original values', () => {
    const pair = inlineDiff('"https://shop/cart?token=secret"', '"https://shop/cart"');
    expect(pair.before.map((t) => t.text).join('')).toBe('"https://shop/cart?token=secret"');
    expect(pair.after.map((t) => t.text).join('')).toBe('"https://shop/cart"');
    expect(pair.before.filter((t) => t.changed).map((t) => t.text).join('')).toBe('?token=secret');
    expect(pair.after.some((t) => t.changed)).toBe(false);
  });
  it('bounds the work for large or entirely unrelated telemetry strings', () => {
    const a = 'a'.repeat(5000), b = 'b'.repeat(5000);
    expect(inlineDiff(a, b)).toEqual({ before: [{ text: a, changed: true }], after: [{ text: b, changed: true }] });
    expect(inlineDiff('same', 'same').before).toEqual([{ text: 'same', changed: false }]);
    expect(inlineDiff('"jane"', '"81f8f6dde88365f3928796ec7aa53f72820b06db8664f5fe76a7eb13e24546a2"').before).toEqual([{ text: '"jane"', changed: true }]);
  });
  it('distinguishes missing values, empty strings, booleans and lossless integers', () => {
    expect(diffValue(undefined)).toBe('not recorded');
    expect(diffValue('')).toBe('""');
    expect(diffValue(false, 'Bool')).toBe('false');
    expect(diffValue('9223372036854775807', 'Int')).toBe('9223372036854775807');
    expect(diffValue('5', 'Str')).toBe('"5"');
  });
  it('preserves nested attribute paths and hides only excess unchanged context', () => {
    const lines = fieldLines([{ entity: 'resource[0]', path: 'attributes["a.b"].value[0]', op: 'modified', before: 1, after: 2 }],
      Array.from({ length: 10 }, (_, i) => ({ key: String(i), value: 'context' })), 2);
    expect(lines).toHaveLength(6);
    expect(lines[4].before!.key).toBe('… 6 unchanged attributes');
    expect(lines[5].before!.key).toBe('a.b.value[0]');
    expect(lines[5].depth).toBe(2);
  });
  it('preserves native OTLP types in unchanged attribute context', () => {
    expect(otlpValue({ intValue: '9223372036854775807' })).toBe('9223372036854775807');
    expect(otlpValue({ boolValue: false })).toBe('false');
    const lines = fieldLines([], [{ key: 'count', value: '5' }], 0, { attributes: [{ key: 'count', value: { intValue: '5' } }] });
    expect(lines[0].before!.text).toBe('5');
  });
});
