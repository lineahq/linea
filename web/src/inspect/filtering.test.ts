import { describe, expect, it } from 'vitest';
import type { Filtering } from '../api/types';
import { dropsBy, filterSummary, filteringFor, refLabel, runTotals, totalsSummary, unused } from './filtering';

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

  it('groups drops by condition and finds conditions that removed nothing', () => {
    const by = dropsBy(verified);
    expect(by.get(0)?.map((d) => d.ref)).toEqual(['resource[1]']);
    expect(by.get(2)).toBeUndefined();
    expect(unused(verified).map((c) => c.index)).toEqual([2]);
    expect(unused({ ...verified, verification: 'replay_differs' })).toEqual([]);
  });

  it('summarizes each verification', () => {
    expect(filterSummary(verified)).toBe('4 log records in, 3 dropped, 1 passed on. Evaluating the conditions on the recorded input reproduces the output, so each drop is named with its condition.');
    expect(filterSummary({ ...verified, verification: 'replay_differs', drops: undefined })).toBe('4 log records in, 3 dropped, 1 passed on. Evaluating the conditions did not reproduce what the processor emitted, so no drop is attributed to a condition.');
    expect(filterSummary({ ...verified, verification: 'unsupported', output: undefined, dropped: 4, error: 'boom' })).toBe('4 log records in, 4 dropped, 0 passed on. A condition failed under error_mode propagate, so the processor returned an error and the whole request was lost. This processor filters with settings the debugger does not evaluate, so no drop is attributed to a condition.');
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
