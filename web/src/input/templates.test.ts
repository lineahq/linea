import { describe, expect, it } from 'vitest';
import { summarizePayload } from './quickInput';
import { attrs, PAYLOAD_TEMPLATES, templatesFor } from './templates';

describe('payload templates', () => {
  it('are valid OTLP JSON of their signal', () => {
    for (const t of PAYLOAD_TEMPLATES) {
      const s = summarizePayload(t.json());
      expect(s, t.id).toMatchObject({ signal: t.signal });
      expect((s as { items: number }).items, t.id).toBeGreaterThan(0);
    }
  });

  it('place timestamps relative to now', () => {
    const now = Date.UTC(2026, 0, 1);
    for (const t of PAYLOAD_TEMPLATES) {
      const stamps = [...t.json(now).matchAll(/"(?:timeUnixNano|startTimeUnixNano|endTimeUnixNano)": "(\d+)"/g)].map((m) => BigInt(m[1]));
      expect(stamps.length, t.id).toBeGreaterThan(0);
      for (const n of stamps) {
        const ms = Number(n / 1_000_000n);
        expect(Math.abs(ms - now), t.id).toBeLessThan(10 * 60_000);
      }
    }
  });

  it('offer the signals a receiver feeds first', () => {
    expect(templatesFor([])).toEqual(PAYLOAD_TEMPLATES);
    const traces = templatesFor(['traces']);
    expect(traces).toHaveLength(PAYLOAD_TEMPLATES.length);
    expect(traces.slice(0, 2).every((t) => t.signal === 'traces')).toBe(true);
  });

  it('encode attribute values by type', () => {
    expect(attrs({ s: 'a', i: 3, d: 1.5, b: true })).toEqual([
      { key: 's', value: { stringValue: 'a' } },
      { key: 'i', value: { intValue: '3' } },
      { key: 'd', value: { doubleValue: 1.5 } },
      { key: 'b', value: { boolValue: true } },
    ]);
  });
});
