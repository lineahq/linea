import { describe, expect, it } from 'vitest';
import { followRows, stepText } from './followBoth';

describe('followRows', () => {
  it('lines up both paths and marks where they diverge', () => {
    const rows = followRows({
      diverges_at: 'filter/health (logs)',
      before: { lineage_id: 'a', path: [{ component: 'otlp' }, { component: 'filter/health', pipeline: 'logs' }, { component: 'otlp/backend', end: 'delivered' }] },
      after: { lineage_id: 'b', path: [{ component: 'otlp' }, { component: 'filter/health', pipeline: 'logs', end: 'dropped' }] },
    });
    expect(rows.map((r) => [r.label, r.before, r.after, r.diverges, r.differs])).toEqual([
      ['otlp', 'passed', 'passed', false, false],
      ['filter/health (logs)', 'passed', 'dropped', true, true],
      ['otlp/backend', 'delivered', undefined, false, true],
    ]);
  });

  it('places a step only the after run took after the step it follows', () => {
    const rows = followRows({
      before: { lineage_id: 'a', path: [{ component: 'otlp' }, { component: 'debug', end: 'delivered' }] },
      after: { lineage_id: 'b', path: [{ component: 'batch', pipeline: 'logs' }, { component: 'otlp' }, { component: 'transform', pipeline: 'logs', changes: 2 }, { component: 'debug', end: 'delivered' }] },
    });
    expect(rows.map((r) => [r.label, r.before, r.after])).toEqual([
      ['batch (logs)', undefined, 'passed'],
      ['otlp', 'passed', 'passed'],
      ['transform (logs)', undefined, 'changed 2 fields'],
      ['debug', 'delivered', 'delivered'],
    ]);
  });

  it('is empty when neither run recorded a path', () => {
    expect(followRows({ before: { lineage_id: '' }, after: { lineage_id: '' } })).toEqual([]);
  });
});

describe('stepText', () => {
  it('prefers the end, then the changes', () => {
    expect(stepText({ component: 'x', end: 'not_routed', changes: 1 })).toBe('not routed');
    expect(stepText({ component: 'x', changes: 1 })).toBe('changed 1 field');
    expect(stepText({ component: 'x' })).toBe('passed');
  });
});
