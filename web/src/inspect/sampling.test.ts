import { describe, expect, it } from 'vitest';
import type { RecordLineage, Sampling } from '../api/types';
import { decisionRows, decisionSummary, recordRef, samplingFor } from './sampling';

const sampling: Sampling = {
  processor: 'tail_sampling', pipeline: 'traces', decision_wait: '2s',
  policies: [
    { name: 'errors', type: 'status_code', rule: 'samples traces with a span whose status is ERROR' },
    { name: 'slow', type: 'latency', rule: 'samples traces lasting more than 500ms' },
  ],
  traces: [
    { trace_id: 'a', name: 'GET /search', decision: 'sampled', policy: 'slow', spans: 1, duration: '850ms', verification: 'replay_verified',
      records: ['4:resource[0].scope[0].span[1]'],
      policies: [{ policy: 'errors', result: 'not_sampled', evidence: 'span statuses: UNSET' }, { policy: 'slow', result: 'sampled', evidence: 'lasted 850ms' }] },
    { trace_id: 'b', name: 'GET /orders/{id}', decision: 'not_sampled', spans: 1, duration: '40ms', late_spans: 1, late: 'dropped', verification: 'replay_differs',
      policies: [{ policy: 'errors', result: 'not_sampled', evidence: 'span statuses: UNSET' }, { policy: 'slow', result: 'error', evidence: 'boom' }] },
    { trace_id: 'c', name: 'probe', decision: 'dropped', policy: 'errors', spans: 1, duration: '1ms', verification: 'replay_verified',
      policies: [{ policy: 'errors', result: 'dropped', evidence: 'tenant: probe' }] },
    { trace_id: 'd', name: 'unfinished', decision: 'pending_dropped', spans: 2, duration: '3ms', verification: '' },
  ],
};

describe('sampling decisions', () => {
  it('finds the explanation of one processor instance', () => {
    expect(samplingFor([sampling], 'tail_sampling', 'traces')).toBe(sampling);
    expect(samplingFor([sampling], 'tail_sampling', 'traces/other')).toBeUndefined();
    expect(samplingFor(undefined, 'tail_sampling', 'traces')).toBeUndefined();
  });
  it('explains each decision by the policy that made it and what every policy found', () => {
    const lineage: RecordLineage[] = [{ id: 'j1', name: 'GET /search', signal: 'traces', hops: [{ seq: 4, kind: 'stage.input', parent: -1, ref: 'resource[0].scope[0].span[1]' }] }];
    const [search, orders, probe, unfinished] = decisionRows(sampling, lineage);
    expect(search).toMatchObject({ tone: 'kept', label: 'Kept', reason: 'policy slow sampled it', lineage: 'j1', warning: undefined });
    expect(search.cells).toEqual([
      { policy: 'errors', outcome: 'no-match', label: 'no match', evidence: 'span statuses: UNSET', decisive: false },
      { policy: 'slow', outcome: 'match', label: 'samples', evidence: 'lasted 850ms', decisive: true },
    ]);
    expect(orders).toMatchObject({ tone: 'dropped', reason: 'no policy sampled it', late: '1 span(s) arrived after the decision and were dropped with it' });
    expect(orders.warning).toContain('decided differently');
    expect(orders.cells[1]).toMatchObject({ outcome: 'error', label: 'error', evidence: 'boom' });
    expect(probe).toMatchObject({ label: 'Dropped by policy', reason: 'drop policy errors matched it' });
    expect(probe.cells[0]).toMatchObject({ outcome: 'drop', label: 'drops', decisive: true });
    expect(unfinished).toMatchObject({ label: 'Dropped at shutdown', cells: [] });
    expect(unfinished.reason).toContain('decision_wait (2s)');
    expect(decisionSummary([search, orders, probe, unfinished])).toBe('1 of 4 traces kept');
  });
  it('splits record refs into their event and entity', () => {
    expect(recordRef('14:resource[0].scope[0].span[1]')).toEqual({ seq: 14, entity: 'resource[0].scope[0].span[1]' });
    expect(recordRef('resource[0]')).toBeUndefined();
  });
});
