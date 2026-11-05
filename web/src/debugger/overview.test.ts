import { describe, expect, it } from 'vitest';
import type { Event, Report, Run } from '../api/types';
import { assertionRows, coverage, coverageText, durationText, environmentRows, facets, problems } from './overview';

const counts = (items: number) => ({ items, resources: 1, scopes: 1 });
const comp = (kind: string, id: string, mode = 'real', extra = {}) => ({ kind, id, type: id, ref: `${kind}/${id}`, mode, ...extra });
const ev = (seq: number, kind: string, component: string, items: number, source = 'collector') =>
  ({ seq, kind, component, source, counts: counts(items), id: String(seq), run_id: 'r', time: '' }) as unknown as Event;

const report = (status = 'completed', extra: Partial<Report> = {}): Report => ({
  report_schema: '0.3',
  topology: { components: [comp('receiver', 'otlp'), comp('processor', 'batch'), comp('exporter', 'archive', 'captured'), comp('extension', 'health_check', 'simulated')], pipelines: [] },
  result: { mode: 'scenario', status, inputs: [{ receiver: 'otlp', signal: 'logs', injected: true, counts: counts(5) }] },
  ...extra,
}) as unknown as Report;
const run = (r: Report, over: Partial<Run> = {}): Run => ({ id: 'run-0123456789abcdef', project_id: 'default', created: '2026-10-23T14:02:00Z', state: 'done', started: '2026-10-23T14:02:00Z', finished: '2026-10-23T14:02:01.400Z', report: r, ...over });

describe('overview facts', () => {
  it('keeps execution, evidence and assertions apart', () => {
    const f = facets(run(report('completed', { assertions: { outcome: 'fail', run_status: 'completed', results: [{ name: 'a', outcome: 'fail', target: 'archive' }, { name: 'b', outcome: 'pass', target: 'x' }] } })), []);
    expect(f.map((x) => [x.name, x.fact.text, x.fact.tone])).toEqual([
      ['Execution', 'Completed', 'pass'],
      ['Evidence', 'Complete', 'pass'],
      ['Assertions', '1 of 2 failed', 'fail'],
    ]);
    expect(f[0].detail).toBe('1.4 s');
    expect(f[1].detail).toBe('3 of 3 components recorded');
  });
  it('says the run did not finish instead of evaluating assertions', () => {
    const f = facets(run(report('partial'), { finished: '2026-10-23T14:02:30Z' }), []);
    expect(f.map((x) => x.fact.tone)).toEqual(['warn', 'warn', 'neutral']);
    expect(f[0].detail).toBe('after 30.0 s');
    expect(f[2].fact.text).toBe('Not evaluated');
    expect(f[2].detail).toBe('run did not finish');
  });
  it('formats durations', () => {
    expect(durationText({ started: '2026-10-23T14:00:00Z', finished: '2026-10-23T14:00:00.250Z' })).toBe('250 ms');
    expect(durationText({})).toBeUndefined();
  });
});

describe('evidence coverage', () => {
  it('counts every component of a completed run, even an exporter that captured nothing', () => {
    const rows = coverage(report(), []);
    expect(rows.map((r) => r.state)).toEqual(['recorded', 'recorded', 'recorded']);
    expect(rows[2].detail).toBe('received 0');
  });
  it('separates recorded, partial and missing after an early end', () => {
    const events = [ev(1, 'input.injected', 'otlp', 5), ev(2, 'stage.input', 'batch', 5)];
    const rows = coverage(report('partial'), events);
    expect(rows.map((r) => [r.id, r.state, r.detail])).toEqual([
      ['otlp', 'recorded', '5 injected'],
      ['batch', 'partial', 'input recorded, no output'],
      ['archive', 'missing', 'not recorded'],
    ]);
    expect(coverageText(rows)).toBe('1 of 3 components recorded');
  });
  it('is the same for a receiver and an exporter that share an id', () => {
    const r = report('partial');
    r.topology!.components!.push(comp('exporter', 'otlp', 'captured') as never);
    const rows = coverage(r, [ev(1, 'input.injected', 'otlp', 5)]);
    expect(rows.filter((x) => x.id === 'otlp').map((x) => [x.kind, x.state])).toEqual([['receiver', 'recorded'], ['exporter', 'missing']]);
  });
});

describe('assertions and environment', () => {
  it('lists failing checks with what was expected and observed', () => {
    const rows = assertionRows({
      outcome: 'fail', run_status: 'completed',
      results: [{ name: 'pager gets 2', outcome: 'fail', target: 'otlp/pager', checks: [{ check: 'count', expected: '2', observed: '0', outcome: 'fail' }, { check: 'x', expected: '1', outcome: 'pass' }] }],
    });
    expect(rows).toEqual([{ outcome: 'fail', icon: 'circle-x', name: 'pager gets 2', target: 'otlp/pager', evidence: ['count: expected 2, observed 0'] }]);
    expect(assertionRows(undefined)).toEqual([]);
  });
  it('lists only what did not run for real, with stand-ins and unavailable references', () => {
    const r = report('completed', {
      topology: { components: [comp('exporter', 'a', 'captured'), comp('exporter', 'b', 'captured'), comp('processor', 'k8sattributes', 'dependency_mocked'), comp('processor', 'acme', 'stand_in', { note: 'not compiled in' }), comp('receiver', 'otlp')], pipelines: [] },
      result: { mode: 'scenario', status: 'completed', mocks: [{ component: 'processor/k8sattributes', mode: 'dependency_mocked', provider: 'kubernetes', supplied: ['pod checkout'] }] },
      references: [{ scheme: 'env', target: 'PAGER', source: 'stand_in', reason: 'unbound', stand_in: 'x' }],
    } as unknown as Partial<Report>);
    const rows = environmentRows(r);
    expect(rows.map((x) => [x.mode, x.subject])).toEqual([
      ['mocked', 'processor/k8sattributes'], ['captured', 'a, b'], ['standin', 'processor/acme'], ['standin', '${env:PAGER}'],
    ]);
    expect(rows[0].detail).toBe('kubernetes mock supplied pod checkout');
    expect(environmentRows(report()).map((x) => x.mode)).toEqual(['captured', 'simulated']);
  });
  it('splits errors, warnings and notes so the first two stay in the open', () => {
    const p = problems(run(report('partial')));
    expect(p.warnings[0].text).toContain('hit its deadline');
    expect(p.errors).toEqual([]);
    expect(problems({ id: 'x', project_id: 'default', created: '', state: 'error', error: 'boom' }).errors[0].text).toContain('boom');
  });
});
