import { describe, expect, it } from 'vitest';
import type { Report, Run } from '../api/types';
import {
  elapsedText,
  assertionFact, assertionOutcomeFact, comparedAssertions, comparedEvidence, comparedExecution, evidenceFact, executionFact, filterRuns, inputCount,
  modesOf, NO_FILTER, runFacts, runLabel, runtimesOf, runtimeText, scenariosOf, serverFilter, sourceText, toggleSelection, whenText,
} from './runFacts';

const run = (over: Partial<Run> & { report?: unknown } = {}) => ({ id: 'run-0123456789abcdef', created: '2026-10-23T14:09:00', state: 'done', ...over }) as Run;
const report = (r: unknown) => r as Report;

describe('labelling runs', () => {
  it('shortens the identifier, since runs carry no sequence number', () => {
    expect(runLabel('run-0123456789abcdef')).toBe('#01234567');
    expect(runLabel('other')).toBe('#other');
  });
  it('words the creation time relative to today', () => {
    const now = new Date(2026, 9, 23, 15, 0);
    expect(whenText(new Date(2026, 9, 23, 14, 9).toISOString(), now)).toBe('Today 14:09');
    expect(whenText(new Date(2026, 9, 22, 17, 5).toISOString(), now)).toBe('Yesterday 17:05');
    expect(whenText(new Date(2026, 9, 16, 9, 0).toISOString(), now)).toBe('Oct 16 09:00');
    expect(whenText(new Date(2025, 11, 31, 23, 59).toISOString(), now)).toBe('Dec 31 2025 23:59');
    expect(whenText('not a time')).toBe('not a time');
  });
});

describe('execution, evidence and assertions are separate facts', () => {
  const completed = { result: { status: 'completed', inputs: [{ injected: true }, { injected: true }] } };
  it('words how a run ended without reading its assertions', () => {
    expect(executionFact(run({ state: 'queued' })).text).toBe('Queued');
    expect(executionFact(run({ state: 'running' })).tone).toBe('running');
    expect(executionFact(run({ state: 'cancelled' })).text).toBe('Cancelled');
    expect(executionFact(run({ state: 'error', error: 'boom' }))).toMatchObject({ text: 'Error', tone: 'fail', title: 'boom' });
    expect(executionFact(run({ state: 'error', interrupted: true })).text).toBe('Interrupted');
    expect(executionFact(run({ report: report(completed) })).text).toBe('Completed');
    expect(executionFact(run({ report: report({ result: { status: 'partial', status_reason: 'deadline' } }) }))).toMatchObject({ text: 'Ended early', title: 'deadline' });
    expect(executionFact(run({ report: report({ result: { status: 'failed' } }) })).tone).toBe('fail');
    // The list carries no report: only the state is known.
    expect(executionFact(run()).text).toBe('Finished');
  });
  it('states completeness on its own, and nothing until a finished run is read', () => {
    expect(evidenceFact(run())).toBeUndefined();
    expect(evidenceFact(run({ state: 'running' }))?.text).toBe('Recording');
    expect(evidenceFact(run({ state: 'cancelled' }))?.text).toBe('None');
    expect(evidenceFact(run({ report: report(completed) }))).toMatchObject({ text: 'Complete', tone: 'pass' });
    const partial = { result: { status: 'partial', inputs: [{ injected: true }, { injected: false }, { injected: true }] } };
    expect(evidenceFact(run({ report: report(partial) }))).toMatchObject({ text: 'Partial 2 of 3', tone: 'warn' });
    expect(evidenceFact(run({ report: report({ result: { status: 'completed', truncation: { reason: 'budget' } } }) }))).toMatchObject({ text: 'Truncated', title: 'budget' });
  });
  it('separates no assertions from not evaluated, and passed from inconclusive', () => {
    expect(assertionFact(run())).toBeUndefined();
    expect(assertionFact(run({ state: 'cancelled' }))?.text).toBe('Not evaluated');
    expect(assertionFact(run({ report: report(completed) }))?.text).toBe('No assertions');
    expect(assertionFact(run({ report: report({ result: { status: 'partial' } }) }))?.text).toBe('Not evaluated');
    const results = (outcomes: string[], outcome: string) => run({ report: report({ ...completed, assertions: { outcome, results: outcomes.map((o) => ({ outcome: o })) } }) });
    expect(assertionFact(results(['pass', 'pass'], 'pass'))).toMatchObject({ text: '2 of 2 passed', tone: 'pass' });
    expect(assertionFact(results(['pass', 'fail'], 'fail'))).toMatchObject({ text: '1 of 2 failed', tone: 'fail' });
    expect(assertionFact(results(['pass', 'inconclusive'], 'inconclusive'))).toMatchObject({ text: '1 of 2 inconclusive', tone: 'warn' });
  });
  it('collects a row, pending until the report is read', () => {
    const now = new Date(2026, 9, 23, 15, 0);
    expect(runFacts(run({ created: new Date(2026, 9, 23, 14, 9).toISOString() }), now)).toMatchObject({ label: '#01234567', when: 'Today 14:09', pending: true });
    const read = runFacts(run({ report: report({ ...completed, scenario: 'debug-error-logs', runtime: { kind: 'built_in', collector_version: '0.162.0', verified: true }, provenance: { config: { sha256: 'sha256:c41e9a1234' } } }) }), now);
    expect(read).toMatchObject({ pending: false, scenario: 'debug-error-logs', runtime: 'built-in 0.162.0', source: 'untitled c41e9a' });
  });
  it('reads a listed run from the platform summary, without its report', () => {
    const listed = (summary: unknown) => run({ summary: summary as Run['summary'] });
    const done = listed({ scenario: 'sampling', status: 'completed', inputs: 2, injected: 2, runtime: { kind: 'remote', name: 'staging', collector_version: '0.162.0', verified: false },
      assertions: { outcome: 'fail', pass: 2, fail: 1, inconclusive: 0, error: 0 } });
    expect(runFacts(done)).toMatchObject({ pending: false, scenario: 'sampling', runtime: 'staging 0.162.0 · remote · unverified', source: undefined });
    expect(executionFact(done).text).toBe('Completed');
    expect(evidenceFact(done)?.text).toBe('Complete');
    expect(assertionFact(done)).toMatchObject({ text: '1 of 3 failed', tone: 'fail' });
    const partial = listed({ status: 'partial', inputs: 3, injected: 1, runtime: { kind: 'built_in', name: 'built-in', collector_version: '0.162.0', verified: true } });
    expect(runFacts(partial)).toMatchObject({ runtime: 'built-in 0.162.0', execution: { text: 'Ended early' }, evidence: { text: 'Partial 1 of 3' }, assertions: { text: 'Not evaluated' } });
    expect(evidenceFact(listed({ status: 'completed', inputs: 1, injected: 1, truncated: true }))?.text).toBe('Truncated');
    expect(assertionFact(listed({ status: 'completed', inputs: 1, injected: 1 }))?.text).toBe('No assertions');
  });
});

describe('what a run was made from', () => {
  it('names the runtime, marking remote and unverified ones', () => {
    expect(runtimeText(undefined)).toBeUndefined();
    expect(runtimeText(report({ runtime: { kind: 'remote', name: 'staging-worker', collector_version: '0.162.0', verified: false } }))).toBe('staging-worker 0.162.0 · remote · unverified');
  });
  it('names the configuration by file, else by digest', () => {
    expect(sourceText(report({ provenance: { config: { path: 'C:\\x\\checkout-logs.yaml', sha256: 'abc' } } }))).toBe('checkout-logs.yaml');
    expect(sourceText(undefined)).toBeUndefined();
    expect(inputCount(report({ result: { inputs: [{ injected: true }, { injected: false }] } }))).toBe(1);
    expect(inputCount(report({}))).toBeUndefined();
  });
});

describe('filtering and selecting runs', () => {
  const summary = { scenario: 'sampling', status: 'completed', inputs: 1, injected: 1, runtime: { kind: 'container', name: 'acme', collector_version: '0.162.0', verified: true } } as Run['summary'];
  const runs = [run({ id: 'run-aaaa000000000001', state: 'done', bundle: { mode: 'scenario' } as Run['bundle'], summary }), run({ id: 'run-bbbb000000000002', state: 'error', bundle: { mode: 'quick' } as Run['bundle'] })];
  it('searches every word a row shows and narrows by mode in the browser', () => {
    expect(filterRuns(runs, NO_FILTER)).toHaveLength(2);
    expect(filterRuns(runs, { ...NO_FILTER, mode: 'scenario' }).map((r) => r.id)).toEqual(['run-aaaa000000000001']);
    expect(filterRuns(runs, { ...NO_FILTER, query: 'BBBB quick' })).toHaveLength(1);
    expect(filterRuns(runs, { ...NO_FILTER, query: 'sampling acme' }).map((r) => r.id)).toEqual(['run-aaaa000000000001']);
    expect(filterRuns(runs, { ...NO_FILTER, query: 'nothing' })).toHaveLength(0);
    expect(modesOf(runs)).toEqual(['quick', 'scenario']);
    expect(scenariosOf(runs)).toEqual(['sampling']);
    expect(runtimesOf(runs)).toEqual(['acme']);
  });
  it('sends only the set scenario, runtime and result to the server', () => {
    expect(serverFilter(NO_FILTER)).toEqual({});
    expect(serverFilter({ ...NO_FILTER, query: 'x', mode: 'quick', runtime: 'built-in', result: 'partial' })).toEqual({ runtime: 'built-in', result: 'partial' });
  });
  it('keeps at most two selected, replacing the earliest', () => {
    expect(toggleSelection([], 'a')).toEqual(['a']);
    expect(toggleSelection(['a'], 'b')).toEqual(['a', 'b']);
    expect(toggleSelection(['a', 'b'], 'c')).toEqual(['b', 'c']);
    expect(toggleSelection(['a', 'b'], 'a')).toEqual(['b']);
  });
});

describe('the sides of a comparison', () => {
  const info = { id: 'run-a', end: 'done', complete: true, execution_status: 'completed', assertions: 'fail' };
  it('words each side from the platform summary', () => {
    expect(comparedExecution(info).text).toBe('Completed');
    expect(comparedExecution({ ...info, execution_status: undefined, end: 'cancelled' })).toMatchObject({ text: 'Cancelled', tone: 'warn' });
    expect(comparedEvidence(info).text).toBe('Evidence complete');
    expect(comparedEvidence({ ...info, complete: false, incomplete: 'deadline' })).toMatchObject({ text: 'Evidence incomplete', title: 'deadline' });
    expect(comparedEvidence({ ...info, truncated_reason: 'budget' }).text).toBe('Evidence truncated');
    expect(comparedAssertions(info, ['pass', 'fail']).text).toBe('1 of 2 failed');
    expect(comparedAssertions({ ...info, assertions: undefined }, []).text).toBe('No assertions');
  });
  it('words one assertion in each run, absent when the run lacks it', () => {
    expect(['pass', 'fail', 'inconclusive', undefined].map((o) => assertionOutcomeFact(o).text)).toEqual(['Passed', 'Failed', 'Inconclusive', 'Absent']);
    expect(assertionOutcomeFact('fail').tone).toBe('fail');
  });
});

describe('elapsedText', () => {
  it('counts seconds, then minutes and seconds', () => {
    expect(elapsedText(0)).toBe('0 s');
    expect(elapsedText(4_400)).toBe('4 s');
    expect(elapsedText(59_400)).toBe('59 s');
    expect(elapsedText(75_000)).toBe('1 min 15 s');
  });
});
