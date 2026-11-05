import { describe, expect, it } from 'vitest';
import type { Bisection } from '../api/types';
import { bisectProblem, bisectRequest, bisectSummary, diffLines, parseBisectSearch, versionRows } from './bisect';

const form = { agent: ' agent-1 ', config: '', good: '', bad: '' };
const ws = { scenarioYAML: 'version: 1\n', scenarioPath: 'scenario.yaml', runtime: '' };

const found: Bisection = {
  bisect_schema: '0.1', verdict: 'found', good: 1, bad: 4, first_bad: 3, last_good: 2, probes: 3,
  target: { agent_id: 'agent-1', config_id: 'pipeline', entry: '10-pipeline', local_files: 0 },
  versions: [
    { version: 1, status: 'good', created_at: '2026-10-01T00:00:00Z' },
    { version: 2, status: 'good', created_at: '2026-10-01T01:00:00Z', created_by: 'ops', rollouts: [{ id: 'r', state: 'completed', created_at: '2026-10-02T00:00:00Z', updated_at: '2026-10-02T00:00:00Z' }] },
    { version: 3, status: 'bad', created_at: '2026-10-01T02:00:00Z', scenarios: [{ name: 'logs', outcome: 'fail', run: 'run-3' }] },
    { version: 4, status: 'untested', created_at: '2026-10-01T03:00:00Z' },
  ],
  culprit: { version: 3, last_good: 2, diff: '--- v2\n+++ v3\n@@ -1,2 +1,2 @@\n a\n-b\n+c\n', scenarios: [] },
};

describe('parseBisectSearch', () => {
  it('keeps the agent and config only', () => {
    expect(parseBisectSearch({ agent: ' a ', config: '', other: 'x' })).toEqual({ agent: 'a' });
  });
});

describe('bisectProblem', () => {
  it('needs an agent, whole versions in order and a scenario', () => {
    expect(bisectProblem({ ...form, agent: '' }, 'x')).toMatch(/agent/);
    expect(bisectProblem({ ...form, good: '1.5' }, 'x')).toMatch(/whole numbers/);
    expect(bisectProblem({ ...form, good: '4', bad: '3' }, 'x')).toMatch(/older/);
    expect(bisectProblem(form, ' ')).toMatch(/scenario/);
    expect(bisectProblem({ ...form, good: '2' }, 'x')).toBeUndefined();
  });
});

describe('bisectRequest', () => {
  it('sends the scenario draft and leaves empty versions to the server', () => {
    expect(bisectRequest(form, ws)).toEqual({ agent: 'agent-1', scenarios: [{ scenario_yaml: 'version: 1\n', scenario_label: 'scenario.yaml' }] });
    expect(bisectRequest({ ...form, config: 'c', good: '2', bad: '9' }, { ...ws, runtime: 'next' })).toMatchObject({ config: 'c', good: 2, bad: 9, runtime: 'next' });
  });
});

describe('bisectSummary', () => {
  it('names the culprit, and never words inconclusive as an answer', () => {
    expect(bisectSummary(found).title).toBe('Version 3 broke the scenario');
    expect(bisectSummary({ ...found, verdict: 'inconclusive', culprit: undefined }).title).toBe('Broken between versions 3 and 3');
    expect(bisectSummary({ ...found, verdict: 'inconclusive', first_bad: undefined, last_good: undefined }).tone).toBe('warn');
    expect(bisectSummary({ ...found, verdict: 'no_regression' }).tone).toBe('pass');
  });
});

describe('versionRows', () => {
  it('lists newest first and marks the culprit and the last good version', () => {
    const rows = versionRows(found);
    expect(rows.map((r) => r.version)).toEqual([4, 3, 2, 1]);
    expect(rows[1]).toMatchObject({ status: 'Fails', tone: 'fail', culprit: true, runs: [{ name: 'logs', run: 'run-3', outcome: 'fail' }] });
    expect(rows[2]).toMatchObject({ lastGood: true, created: '2026-10-01 01:00 · ops', rollouts: 'completed 2026-10-02' });
    expect(rows[0].status).toBe('Not tested');
  });
});

describe('diffLines', () => {
  it('drops the file headers and classifies lines', () => {
    expect(diffLines(found.culprit!.diff)).toEqual([
      { kind: 'hunk', text: '@@ -1,2 +1,2 @@' },
      { kind: 'ctx', text: 'a' },
      { kind: 'del', text: 'b' },
      { kind: 'add', text: 'c' },
    ]);
  });
});
