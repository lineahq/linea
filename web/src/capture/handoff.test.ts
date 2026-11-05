import { describe, expect, it } from 'vitest';
import { CAPTURE_HANDOFF_KEY, CAPTURE_HANDOFF_MAX_AGE_MS, peekCaptureHandoff, stashCaptureHandoff, takeCaptureHandoff, type CaptureHandoffInput } from './handoff';

function memory(initial: Record<string, string> = {}) {
  const data = new Map(Object.entries(initial));
  return {
    data,
    getItem: (k: string) => data.get(k) ?? null,
    setItem: (k: string, v: string) => void data.set(k, v),
    removeItem: (k: string) => void data.delete(k),
  };
}

const now = new Date('2026-10-23T12:00:00Z');
const input: CaptureHandoffInput = {
  captureId: 'cap-1',
  containsRaw: false,
  streams: [{ stream: { collector_id: 'c1', pipeline: 'logs', point: 'egress', signal: 'logs' }, sequences: [3, 1, 2, 2], watermark: 9, receiver: 'otlp' }],
};

describe('capture handoff', () => {
  it('stores identifiers under the versioned key and takes them once', () => {
    const store = memory();
    expect(stashCaptureHandoff(input, store, now)).toBe(true);
    expect(CAPTURE_HANDOFF_KEY).toBe('linea.capture-handoff.v1');
    expect(store.data.has(CAPTURE_HANDOFF_KEY)).toBe(true);
    const taken = takeCaptureHandoff(store, now.getTime());
    expect(taken).toEqual({ version: 1, captureId: 'cap-1', containsRaw: false, stashedAt: now.toISOString(), streams: [{ ...input.streams[0], sequences: [1, 2, 3] }] });
    expect(store.data.size).toBe(0);
    expect(takeCaptureHandoff(store, now.getTime())).toBeUndefined();
  });

  it('peek leaves the entry in place', () => {
    const store = memory();
    stashCaptureHandoff(input, store, now);
    expect(peekCaptureHandoff(store, now.getTime())?.captureId).toBe('cap-1');
    expect(store.data.size).toBe(1);
  });

  it('never stores payloads even if a caller passes extra fields', () => {
    const store = memory();
    const risky = { ...input, streams: [{ ...input.streams[0], payload: { secret: 'x' } }] } as unknown as CaptureHandoffInput;
    stashCaptureHandoff(risky, store, now);
    expect([...store.data.values()][0]).not.toContain('secret');
  });

  it.each([
    ['no streams', { ...input, streams: [] }],
    ['empty capture id', { ...input, captureId: '' }],
    ['no sequences', { ...input, streams: [{ ...input.streams[0], sequences: [] }] }],
    ['too many sequences', { ...input, streams: [{ ...input.streams[0], sequences: Array.from({ length: 1001 }, (_, i) => i) }] }],
    ['negative sequence', { ...input, streams: [{ ...input.streams[0], sequences: [-1] }] }],
    ['fractional sequence', { ...input, streams: [{ ...input.streams[0], sequences: [1.5] }] }],
    ['incomplete stream', { ...input, streams: [{ ...input.streams[0], stream: { collector_id: 'c1' } }] }],
    ['too many streams', { ...input, streams: Array.from({ length: 17 }, () => input.streams[0]) }],
  ])('refuses to stash %s', (_name, bad) => {
    const store = memory();
    expect(stashCaptureHandoff(bad as CaptureHandoffInput, store, now)).toBe(false);
    expect(store.data.size).toBe(0);
  });

  it('clears and ignores damaged or foreign entries', () => {
    for (const raw of ['not json', '{"version":2}', '[]', 'null', JSON.stringify({ version: 1, captureId: 'x' })]) {
      const store = memory({ [CAPTURE_HANDOFF_KEY]: raw });
      expect(takeCaptureHandoff(store, now.getTime())).toBeUndefined();
      expect(store.data.size).toBe(0);
    }
  });

  it('discards an abandoned handoff', () => {
    const store = memory();
    stashCaptureHandoff(input, store, now);
    expect(takeCaptureHandoff(store, now.getTime() + CAPTURE_HANDOFF_MAX_AGE_MS + 1)).toBeUndefined();
    expect(store.data.size).toBe(0);
  });

  it('survives storage that throws on every access', () => {
    const broken = {
      getItem: () => {
        throw new Error('blocked');
      },
      setItem: () => {
        throw new Error('full');
      },
      removeItem: () => {
        throw new Error('blocked');
      },
    };
    expect(stashCaptureHandoff(input, broken, now)).toBe(false);
    expect(peekCaptureHandoff(broken, now.getTime())).toBeUndefined();
    expect(takeCaptureHandoff(broken, now.getTime())).toBeUndefined();
  });

  it('reports failure without any storage', () => {
    expect(stashCaptureHandoff(input, undefined, now)).toBe(false);
    expect(takeCaptureHandoff(undefined, now.getTime())).toBeUndefined();
  });
});
