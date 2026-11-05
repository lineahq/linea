import { describe, expect, it } from 'vitest';
import { handoffRecordCount } from './handoffView';

describe('handoffRecordCount', () => {
  it('adds the records of every stream', () => {
    expect(handoffRecordCount({ streams: [] })).toBe(0);
    const stream = (n: number) => ({ stream: { signal: 'logs' } as never, sequences: Array.from({ length: n }, (_, i) => i), watermark: n });
    expect(handoffRecordCount({ streams: [stream(2), stream(3)] })).toBe(5);
  });
});
