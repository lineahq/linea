import { describe, expect, it } from 'vitest';
import type { Filtering } from '../api/types';
import { filterSummary, filteringFor, refLabel, removedBy, removedByText, runTotals, totalsSummary, unused } from './filtering';

const verified: Filtering = {
  processor: 'filter/health', pipeline: 'logs', signal: 'logs', input: 7, output: 8, verification: 'replay_verified', records: 4, dropped: 3,
  conditions: [
    { index: 0, source: 'logs.resource[0]', context: 'resource', condition: 'attributes["env"] == "dev"', error_mode: 'ignore', evaluated: 2, matched: 1, records: 2, errors: 0 },
    { index: 1, source: 'logs.log_record[0]', context: 'log', condition: 'IsMatch(body, "^health")', error_mode: 'ignore', evaluated: 2, matched: 1, records: 1, errors: 0 },
    { index: 2, source: 'logs.log_record[1]', context: 'log', condition: 'severity_number < 5', error_mode: 'ignore', evaluated: 1, matched: 0, records: 0, errors: 0 },
  ],
  drops: [{ ref: 'resource[1]', condition: 0, records: 2 }, { ref: 'resource[0].scope[0].log_record[1]', condition: 1, records: 1 }],
};

describe('filtering', () => {
  it('lists one processor’s inputs in order', () => {
    const later = { ...verified, input: 9 };
    expect(filteringFor([later, verified, { ...verified, pipeline: 'traces' }], 'filter/health', 'logs')).toEqual([verified, later]);
    expect(filteringFor(undefined, 'filter/health', 'logs')).toEqual([]);
  });

  it('names input positions 1-based', () => {
    expect(refLabel('resource[0].scope[0].log_record[2]')).toBe('resource 1 › scope 1 › log record 3');
    expect(refLabel('resource[1].scope[0].span[0].event[3]')).toBe('resource 2 › scope 1 › span 1 › event 4');
    expect(refLabel('resource[0].scope[0].metric[1].data_point[0]')).toBe('resource 1 › scope 1 › metric 2 › data point 1');
  });

  it('finds conditions that removed nothing, only when the replay is verified', () => {
    expect(unused(verified).map((c) => c.index)).toEqual([2]);
    expect(unused({ ...verified, verification: 'replay_differs' })).toEqual([]);
  });

  it('names the conditions that removed a record', () => {
    const at = (entity: string, f: Filtering = verified) => removedBy(f, entity).map((c) => c.index);
    expect(at('resource[0].scope[0].log_record[1]')).toEqual([1]);
    expect(at('resource[0].scope[0].log_record[0]')).toEqual([]);
    expect(at('resource[1].scope[0].log_record[0]')).toEqual([0]);
    expect(at('resource[1]')).toEqual([0]);
    expect(removedBy(undefined, 'resource[1]')).toEqual([]);
  });

  it('matches a data point drop to its metric, and refs in removed', () => {
    const metrics: Filtering = { ...verified, signal: 'metrics', drops: [
      { ref: 'resource[0].scope[0].metric[2].data_point[1]', condition: 2, records: 1 },
      { ref: 'resource[0].scope[0].metric[3]', condition: 1, records: 1, removed: [{ ref: 'resource[0].scope[0].metric[4]', name: 'm4', lineage: 'l4' }] },
    ] };
    expect(removedBy(metrics, 'resource[0].scope[0].metric[2]').map((c) => c.index)).toEqual([2]);
    expect(removedBy(metrics, 'resource[0].scope[0].metric[4]').map((c) => c.index)).toEqual([1]);
    expect(removedBy(metrics, 'resource[0].scope[0].metric[5]')).toEqual([]);
  });

  it('names nothing unless the replay reproduces the output', () => {
    for (const verification of ['replay_differs', 'unsupported'] as const) {
      expect(removedBy({ ...verified, verification }, 'resource[1]')).toEqual([]);
    }
  });

  it('words which conditions removed a record', () => {
    const cs = verified.conditions!;
    expect(removedByText([cs[1]])).toBe('Removed by condition 1: IsMatch(body, "^health")');
    expect(removedByText([cs[0], cs[2]])).toBe('Removed by conditions 0 and 2');
    expect(removedByText(cs)).toBe('Removed by conditions 0, 1 and 2');
    expect(removedByText([])).toBeUndefined();
  });

  it('summarizes each verification without repeating the counts', () => {
    expect(filterSummary(verified)).toBe('Evaluating the conditions on the recorded input reproduces the output: the counts below are exact, and Changes names the condition that removed each record.');
    expect(filterSummary({ ...verified, drops: undefined })).toBe('Evaluating the conditions on the recorded input reproduces the output: the counts below are exact.');
    expect(filterSummary({ ...verified, verification: 'replay_differs', drops: undefined })).toBe('Evaluating the conditions did not reproduce what the processor emitted, so no count or drop is attributed to a condition.');
    expect(filterSummary({ ...verified, verification: 'unsupported', output: undefined, dropped: 4, error: 'boom' })).toBe('A condition failed under error_mode propagate, so the processor returned an error and the whole request was lost. This processor filters with settings the debugger does not evaluate, so no drop is attributed to a condition.');
  });

  it('sums the run per condition, counting only verified requests', () => {
    const second: Filtering = { ...verified, input: 9, records: 2, dropped: 1, conditions: verified.conditions!.map((c) => ({ ...c, evaluated: 1, matched: c.index === 1 ? 1 : 0, records: c.index === 1 ? 1 : 0 })) };
    const differs: Filtering = { ...verified, input: 11, verification: 'replay_differs', records: 5, dropped: 0 };
    const t = runTotals([verified, second, differs]);
    expect([t.requests, t.verified, t.records, t.dropped]).toEqual([3, 2, 11, 4]);
    expect(t.conditions.map((c) => [c.index, c.evaluated, c.matched, c.records])).toEqual([[0, 3, 1, 2], [1, 3, 2, 2], [2, 2, 0, 0]]);
    expect(totalsSummary(t, 'logs')).toBe('3 requests, 11 log records in, 4 dropped. Condition counts cover the 2 requests whose evaluation reproduces the output.');
    expect(totalsSummary(runTotals([verified, second]), 'logs')).toBe('2 requests, 6 log records in, 4 dropped.');
  });
});
