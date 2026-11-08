import { describe, expect, it } from 'vitest';
import {
  batcherLabel, batcherRows, batchingFor, batchRows, durationMs, impactRows, itemNoun, metadataNote, settingsText, summaryText, timerNote,
  verificationText,
} from './batching';
import { metadataBatching as m, normalized, sampleBatching as b } from './batching.fixture';

describe('batching model', () => {
  it('finds the processor instance of a pipeline', () => {
    expect(batchingFor([b], 'batch', 'logs')).toBe(b);
    expect(batchingFor([b], 'batch', 'traces')).toBeUndefined();
    expect(batchingFor(undefined, 'batch', 'logs')).toBeUndefined();
  });
  it('counts what the processor counts for each signal', () => {
    expect(itemNoun('logs', 1)).toBe('1 log record');
    expect(itemNoun('traces', 3)).toBe('3 spans');
    expect(itemNoun('metrics', 2)).toBe('2 data points');
  });
  it('sums up requests in, batches out and why they left', () => {
    expect(summaryText(b)).toBe('5 requests in, 4 batches out (15 log records): 2 size reached, 1 timer, 1 shutdown.');
    expect(settingsText(b)).toBe('send_batch_size 4 · timeout 300ms · send_batch_max_size 6');
    expect(settingsText(m)).toBe('send_batch_size 4 · timeout 300ms · metadata_keys [tenant]');
  });
  it('compares requests, sizes, resource blocks and wait before and after', () => {
    expect(impactRows(b)).toEqual([
      { label: 'Requests', before: '5', after: '4', note: 'fewer, larger requests to the exporters' },
      { label: 'Log records per request', before: '3', after: '3.8' },
      { label: 'Resource blocks', before: '5', after: '6', note: 'not merged: batching appends requests, so equal resources stay separate blocks' },
      { label: 'Added wait', before: '0', after: 'up to 99.8ms' },
    ]);
    expect(impactRows(normalized(b)).map((r) => r.label)).not.toContain('Added wait');
    expect(impactRows(m).at(-1)).toEqual({ label: 'Batchers', before: '1', after: '3', note: 'one per tenant value: a batch never mixes them' });
  });
  it('names batchers by the listed keys only', () => {
    expect([0, 1, 2].map((i) => batcherLabel(m, i))).toEqual(['tenant=acme', 'tenant=globex', 'no tenant']);
    expect(batcherLabel({ ...m, batchers: [{ metadata: {}, unknown: true, arrivals: 1, items: 1, flushes: 0 }] }, 0)).toBe('unknown metadata');
    expect(batcherRows(m).map((r) => [r.label, r.requests, r.items, r.batches, r.reasons])).toEqual([
      ['tenant=acme', 2, 4, 1, [['size', 1]]],
      ['tenant=globex', 1, 2, 1, [['timeout', 1]]],
      ['no tenant', 1, 1, 1, [['timeout', 1]]],
    ]);
    expect(batcherRows(b)).toEqual([]);
  });
  it('explains each batch with its reason, requests and wait', () => {
    const rows = batchRows(b);
    expect(rows.map((r) => r.why)).toEqual([
      '5 log records were waiting, at least send_batch_size 4',
      'The 300ms timer fired with 1 log record waiting, short of 4',
      '9 log records were waiting; send_batch_max_size 6 cut this batch and 3 kept waiting',
      'The Collector shut down with 3 log records waiting; they are sent, not lost',
    ]);
    expect(rows[0].contents).toEqual([{ request: 1, source: 'checkout', items: 2 }, { request: 2, source: 'cart', items: 1 }, { request: 3, source: 'checkout', items: 2 }]);
    expect(rows.map((r) => r.waited)).toEqual(['no wait', 'waited 99.8ms', 'no wait', 'no wait']);
    expect(rows[0].batcher).toBeUndefined();
    expect(batchRows(m).map((r) => r.batcher)).toEqual(['tenant=acme', 'no tenant', 'tenant=globex']);
    expect(timerNote(b, rows[1])).toContain('restart after every batch');
    expect(timerNote(m, batchRows(m)[1])).toBeUndefined();
  });
  it('says what metadata_keys does, set or not', () => {
    expect(metadataNote(b)).toContain('Every request shares one batcher');
    expect(metadataNote(m)).toContain('metadata_keys [tenant]: one batcher per value of this request header');
  });
  it('reads Go durations and states the metrics check', () => {
    expect(durationMs('1m0.5s')).toBe(60_500);
    expect(verificationText(b).tone).toBe('pass');
    expect(verificationText({ ...b, verification: 'unverified' }).tone).toBe('neutral');
  });
});
