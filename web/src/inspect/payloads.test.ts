import { describe, expect, it } from 'vitest';
import type { Event } from '../api/types';
import { payloadGroups, payloadOption, payloadSummary, totalText } from './payloads';

const exported = (seq: number, items: number, signal = 'logs'): Event =>
  ({ seq, kind: 'exporter.captured', component: 'otlp/backend', signal, counts: { items, resources: 1, scopes: 1 } }) as Event;

describe('payloads', () => {
  const batches = [exported(12, 4), exported(14, 2), exported(16, 1)];
  it('names each payload as a request by position', () => {
    expect(batches.map((_, i) => payloadOption(batches, i))).toEqual(['Request 1 · 4 log records', 'Request 2 · 2 log records', 'Request 3 · 1 log record']);
  });
  it('says which request is shown and what all of them carried', () => {
    expect(payloadSummary(batches, batches[0])).toBe('request 1 of 3 (4 log records); all 3 requests: 7 log records');
    expect(payloadSummary([batches[1]], batches[1])).toBe('2 log records');
  });
  it('sums each signal apart', () => {
    expect(totalText([exported(1, 4), exported(2, 3, 'traces')])).toBe('4 log records, 3 spans');
  });
  it('splits payloads by signal, keeping their order', () => {
    expect(payloadGroups([exported(1, 4), exported(2, 3, 'traces'), exported(3, 1)])).toEqual([{ signal: 'logs', indexes: [0, 2] }, { signal: 'traces', indexes: [1] }]);
  });
});
