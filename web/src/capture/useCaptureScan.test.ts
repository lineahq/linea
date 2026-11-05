import { describe, expect, it } from 'vitest';
import { readWholeCapture } from './useCaptureScan';

describe('readWholeCapture', () => {
  it('reads every page of 100 and reports progress', async () => {
    const stream = { collector_id: 'c', pipeline: 'logs', point: 'output', signal: 'logs' };
    const asked: { cursor: string; limit: number }[] = [];
    const progress: number[] = [];
    const read = async (_id: string, cursor = '', _stream?: unknown, limit = 25) => {
      asked.push({ cursor, limit });
      const from = Number(cursor || 0);
      const items = Array.from({ length: Math.min(limit, 230 - from) }, (_, i) => ({ sequence: from + i + 1, stream, payload: {}, received_at: '', sensitivity: 'redacted' }));
      return { session_id: 'x', watermark: 230, items, ...(from + limit < 230 ? { next_cursor: String(from + limit) } : {}) };
    };
    const scan = await readWholeCapture('x', read as never, undefined, (n) => progress.push(n));
    expect(asked).toEqual([{ cursor: '', limit: 100 }, { cursor: '100', limit: 100 }, { cursor: '200', limit: 100 }]);
    expect(progress).toEqual([100, 200, 230]);
    expect(scan.records).toHaveLength(230);
    expect(scan.items.get(230)?.sequence).toBe(230);
    expect(scan.watermark).toBe(230);
  });
});
