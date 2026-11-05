import { describe, expect, it } from 'vitest';
import type { Suite, SuiteMemberStatus, SuiteStatus, TestReport } from '../api/types';
import { assertionLine, assertionTotals, chipClass, firstFailing, junitHref, memberRows, outcomeBadge, rerunBlocker, runLink, scenarioFor, stateBadge, suiteFacets, suiteResultBadge } from './suiteView';

const member = (label: string, run: string | undefined, state: string | undefined, outcome: string | undefined, message?: string): SuiteMemberStatus => ({
  label, run_id: run, state, outcome, message,
});

const status = (done: boolean, outcome: string | undefined, members: SuiteMemberStatus[]): SuiteStatus => ({
  id: 'suite-1', created: '2026-10-23T10:00:00Z', config: 'checkout.yaml', done, outcome, members,
});

const suiteOf = (n: number): Suite => ({ id: 'suite-1', project_id: 'default', created: '2026-10-23T10:00:00Z', config: 'checkout.yaml', members: Array.from({ length: n }, (_, i) => ({ label: `s${i}` })) });

const REPORT: TestReport = {
  config: 'checkout.yaml', outcome: 'fail', test_schema: '1',
  scenarios: [
    { path: 'debug-error-logs', outcome: 'pass', duration_seconds: 1, run_id: 'run-a', assertions: { outcome: 'pass', run_status: 'done', results: [
      { name: 'a', target: 'exporter otlp/pager', outcome: 'pass' }, { name: 'b', target: 'exporter otlp/archive', outcome: 'pass' }] } },
    { path: 'k8s-enrichment', outcome: 'fail', duration_seconds: 1, run_id: 'run-b', assertions: { outcome: 'fail', run_status: 'done', results: [
      { name: 'ns set', target: 'exporter otlphttp/backend', outcome: 'fail', checks: [
        { check: 'attribute', key: 'k8s.namespace.name', outcome: 'fail', expected: 'set on all', observed: '0 of 6' }] },
      { name: 'count', target: 'exporter otlphttp/backend', outcome: 'inconclusive', checks: [
        { check: 'count', outcome: 'inconclusive', expected: '6', message: 'Evidence partial' }] },
      { name: 'ok', target: 'exporter x', outcome: 'pass' }] } },
    { path: 'tail-sampling', outcome: 'error', duration_seconds: 1, run_id: 'run-c', error: 'timed out' },
  ],
};

const DONE = status(true, 'fail', [
  member('debug-error-logs', 'run-a', 'done', 'pass'),
  member('k8s-enrichment', 'run-b', 'done', 'fail'),
  member('tail-sampling', 'run-c', 'error', 'error', 'run timed out'),
]);

describe('badges', () => {
  it.each([
    ['pass', 'pass', 'Passed'],
    ['fail', 'fail', 'Failed'],
    ['inconclusive', 'warn', 'Inconclusive'],
    ['error', 'fail', 'Error'],
    [undefined, 'neutral', 'Not finished'],
  ])('outcome %s', (o, tone, label) => expect(outcomeBadge(o)).toMatchObject({ tone, label }));

  it.each([
    ['done', 'run-1', 'Completed'],
    ['error', 'run-1', 'Run failed'],
    ['cancelled', 'run-1', 'Cancelled'],
    ['running', 'run-1', 'Running'],
    ['queued', 'run-1', 'Queued'],
    [undefined, undefined, 'Not started'],
  ])('state %s of %s', (state, run, label) => expect(stateBadge(state, run).label).toBe(label));

  it('maps tones to design-system chips', () => {
    expect(chipClass('pass')).toBe('od-chip od-chip--pass');
    expect(chipClass('neutral')).toBe('od-chip');
  });
});

describe('suiteResultBadge', () => {
  it('waits for the status', () => expect(suiteResultBadge(suiteOf(3)).label).toBe('Loading…'));
  it('counts finished members while running', () => {
    const s = status(false, undefined, [member('a', 'r1', 'done', 'pass'), member('b', 'r2', 'running', undefined), member('c', 'r3', 'queued', undefined)]);
    expect(suiteResultBadge(suiteOf(3), s)).toMatchObject({ tone: 'running', label: 'Running · 1 of 3 finished' });
  });
  it('says how many members have the suite outcome', () => {
    expect(suiteResultBadge(suiteOf(3), DONE)).toMatchObject({ tone: 'fail', label: 'Failed · 1 of 3' });
    const pass = status(true, 'pass', [member('a', 'r1', 'done', 'pass'), member('b', 'r2', 'done', 'pass'), member('c', 'r3', 'done', 'pass')]);
    expect(suiteResultBadge(suiteOf(3), pass)).toMatchObject({ tone: 'pass', label: 'Passed · 3 of 3' });
  });
});

describe('assertionTotals', () => {
  it('counts every assertion', () => {
    expect(assertionTotals(REPORT)).toEqual({ total: 5, pass: 3, fail: 1, inconclusive: 1, error: 0 });
    expect(assertionTotals(undefined).total).toBe(0);
  });
});

describe('suiteFacets', () => {
  it('summarizes a finished suite with its report', () => {
    const f = suiteFacets(DONE, REPORT);
    expect(f.map((x) => x.name)).toEqual(['Suite', 'Execution', 'Assertions']);
    expect(f[0]).toMatchObject({ tone: 'fail', label: 'Failed', detail: '1 failed · 1 with errors' });
    expect(f[1]).toMatchObject({ tone: 'warn', label: '2 of 3 completed', detail: '1 failed to run' });
    expect(f[2]).toMatchObject({ tone: 'fail', label: '3 of 5 passed', detail: '1 failed · 1 inconclusive' });
  });
  it('has no assertions facet before the report', () => {
    expect(suiteFacets(DONE).map((x) => x.name)).toEqual(['Suite', 'Execution']);
  });
  it('reports a running suite', () => {
    const s = status(false, undefined, [member('a', 'r1', 'done', 'pass'), member('b', 'r2', 'running', undefined)]);
    const f = suiteFacets(s);
    expect(f[0]).toMatchObject({ tone: 'running', label: 'Running', detail: '1 of 2 members finished' });
    expect(f[1]).toMatchObject({ tone: 'running', label: '1 of 2 completed', detail: '1 still running' });
  });
  it('reports a passing suite', () => {
    const s = status(true, 'pass', [member('a', 'r1', 'done', 'pass')]);
    expect(suiteFacets(s)[0]).toMatchObject({ tone: 'pass', label: 'Passed', detail: 'All 1 member passed' });
    expect(suiteFacets(s)[1]).toMatchObject({ tone: 'pass', detail: 'Every member ran' });
  });
  it('counts members that were never admitted', () => {
    const s = status(true, 'error', [member('a', undefined, undefined, 'error', 'cannot read')]);
    expect(suiteFacets(s)[1].detail).toBe('1 not started');
  });
});

describe('assertionLine', () => {
  it('words the evidence of non-passing checks', () => {
    expect(assertionLine(REPORT.scenarios![1].assertions!.results![0])).toEqual({
      tone: 'fail',
      name: 'ns set',
      target: 'exporter otlphttp/backend',
      evidence: 'attribute k8s.namespace.name: expected set on all, observed 0 of 6',
    });
    expect(assertionLine(REPORT.scenarios![1].assertions!.results![1])).toMatchObject({ tone: 'warn', evidence: 'Evidence partial' });
  });
  it('shortens a long list of checks', () => {
    const checks = Array.from({ length: 5 }, (_, i) => ({ check: 'count', outcome: 'fail', expected: String(i), message: `m${i}` }));
    expect(assertionLine({ name: 'n', target: 't', outcome: 'fail', checks }).evidence).toBe('m0 · m1 · m2 · and 2 more');
  });
});

describe('memberRows', () => {
  const rows = memberRows(DONE, REPORT);
  it('has a row per member with its assertions', () => {
    expect(rows.map((r) => r.assertions)).toEqual(['2 of 2 passed', '1 of 3 failed · 1 inconclusive', '']);
    expect(rows[0]).toMatchObject({ label: 'debug-error-logs', runId: 'run-a', state: { label: 'Completed' }, outcome: { label: 'Passed' } });
    expect(rows[1].attention.map((a) => a.name)).toEqual(['ns set', 'count']);
  });
  it('says 2 of 2 passed for a passing member', () => {
    const passing = memberRows(status(true, 'pass', [member('debug-error-logs', 'run-a', 'done', 'pass')]), REPORT);
    expect(passing[0].assertions).toBe('2 of 2 passed');
  });
  it('explains an error without assertion results', () => {
    expect(rows[2].message).toBe('run timed out');
  });
  it('shows members before the report is read', () => {
    expect(memberRows(DONE).map((r) => r.attention.length)).toEqual([0, 0, 0]);
  });
  it('finds the first member to investigate', () => {
    expect(firstFailing(rows)?.runId).toBe('run-b');
    expect(firstFailing(memberRows(status(true, 'pass', [member('a', 'r', 'done', 'pass')])))).toBeUndefined();
  });
});

describe('scenarioFor', () => {
  const m = member('x', 'run-b', 'done', 'fail');
  it('matches by run, else by position', () => {
    expect(scenarioFor(REPORT, m, 0, 3)?.path).toBe('k8s-enrichment');
    expect(scenarioFor(REPORT, member('x', undefined, undefined, 'error'), 2, 3)?.path).toBe('tail-sampling');
    expect(scenarioFor(REPORT, member('x', undefined, undefined, 'error'), 0, 2)).toBeUndefined();
    expect(scenarioFor(undefined, m, 0, 3)).toBeUndefined();
  });
});

describe('runLink', () => {
  it('opens the run in Investigate', () => expect(runLink('run-b')).toEqual({ to: '/', search: { run: 'run-b' } }));
});

describe('rerunBlocker', () => {
  it('allows a finished suite whose members were all admitted', () => {
    expect(rerunBlocker(status(true, 'pass', [member('a.yaml', 'run-a', 'done', 'pass')]))).toBeUndefined();
  });
  it('waits for a running suite', () => {
    expect(rerunBlocker(status(false, undefined, [member('a.yaml', 'run-a', 'running', undefined)]))).toMatch(/still running/);
  });
  it('names a member that was never admitted', () => {
    expect(rerunBlocker(status(true, 'error', [member('a.yaml', 'run-a', 'done', 'pass'), member('b.yaml', undefined, undefined, 'error', 'missing')])))
      .toBe('b.yaml was never admitted, so it recorded no inputs to rerun.');
  });
});

describe('junitHref', () => {
  it('escapes the suite id', () => {
    expect(junitHref('suite/1')).toBe('/api/v1/suites/suite%2F1/junit');
  });
});
