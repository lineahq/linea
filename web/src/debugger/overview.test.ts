import { describe, expect, it } from 'vitest';
import type { Event, Report, Run } from '../api/types';
import { assertionRows, assertionsCard, coverage, coverageText, durationText, environmentRows, facets, notRealLine, problems } from './overview';

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

describe('overview cards', () => {
  const row = (outcome: string, name = outcome) => ({ outcome, icon: 'circle-check' as const, name, target: 't', evidence: ['x: expected 1'] });
  it('summarizes the assertions: failures listed without evidence, passes in one line', () => {
    const failed = assertionsCard([row('pass', 'a'), row('fail', 'b')], true)!;
    expect(failed).toMatchObject({ status: '1 of 2 failed', tone: 'fail', total: 2, failed: 1 });
    expect(failed.failing.map((x) => x.name)).toEqual(['b']);
    expect(assertionsCard([row('pass'), row('pass')], true)).toMatchObject({ status: 'Passed', tone: 'pass', line: 'All 2 passed', failing: [] });
  });
  it('lists errors first and never counts an inconclusive check as a pass', () => {
    const c = assertionsCard([row('inconclusive', 'i'), row('error', 'e'), row('pass', 'p')], true)!;
    expect(c.status).toBe('1 of 3 failed, 1 inconclusive');
    expect(c.failing.map((x) => x.name)).toEqual(['e', 'i']);
    const only = assertionsCard([row('inconclusive', 'i'), row('pass')], true)!;
    expect(only).toMatchObject({ status: '1 of 2 inconclusive', tone: 'warn', failed: 0 });
    expect(only.line).toBeUndefined();
  });
  it('says no assertions only for a completed run', () => {
    expect(assertionsCard([], true)).toMatchObject({ status: 'None', line: 'No assertions', unchecked: true, total: 0 });
    expect(assertionsCard([], false)).toBeUndefined();
  });
  it('says what did not run for real or was not recorded, and nothing otherwise', () => {
    expect(notRealLine(undefined, [], false)).toBeUndefined();
    expect(notRealLine(report(), coverage(report(), [ev(1, 'input.injected', 'otlp', 5)]), false)).toBeUndefined();
    const r = report('completed', {
      topology: { components: [comp('processor', 'k8sattributes', 'dependency_mocked'), comp('processor', 'geoip', 'stand_in'), comp('exporter', 'a', 'captured')], pipelines: [] },
    } as unknown as Partial<Report>);
    expect(notRealLine(r, [], false)).toEqual({ text: '1 mocked component, 1 stand-in · checks through stand-ins are inconclusive', tone: 'warn' });
    const early = report('partial');
    const rows = coverage(early, []);
    expect(notRealLine(early, rows, false)!.text).toBe(`${rows.filter((x) => x.state !== 'recorded').length} components not recorded · missing output is not a drop`);
    expect(notRealLine(early, rows, true)).toBeUndefined();
  });
});
