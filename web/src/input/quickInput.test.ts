import { describe, expect, it } from 'vitest';
import { parse } from 'yaml';
import { jsonProblem, quickDoc, runBlocker, runFields, runPlan, scenarioSetup, setupInUse, summarizePayload } from './quickInput';

const logs = JSON.stringify({
  resourceLogs: [{ scopeLogs: [{ logRecords: [{}, {}] }, { logRecords: [{}] }] }, { scopeLogs: [] }],
});

const doc = (...lines: string[]) => ['version: 1', ...lines, ''].join('\n');
const one = (data = logs, ...extra: string[]) =>
  doc('inputs:', '  - receiver: otlp', '    client_address: " 10.0.0.1 "', `    data: '${data}'`, ...extra);

describe('summarizePayload', () => {
  it('names the signal and counts items', () => {
    expect(summarizePayload(logs)).toEqual({ signal: 'logs', resources: 2, items: 3 });
    expect(summarizePayload('{"resource_spans":[{"scope_spans":[{"spans":[{}]}]}]}')).toEqual({
      signal: 'traces',
      resources: 1,
      items: 1,
    });
    const metrics = {
      resourceMetrics: [
        { scopeMetrics: [{ metrics: [{ gauge: { dataPoints: [{}, {}] } }, { sum: { dataPoints: [{}] } }, { histogram: {} }] }] },
      ],
    };
    expect(summarizePayload(JSON.stringify(metrics))).toEqual({ signal: 'metrics', resources: 1, items: 3 });
  });

  it('explains what is wrong', () => {
    expect(summarizePayload('')).toHaveProperty('error');
    expect(summarizePayload('[]')).toEqual({ error: 'An OTLP JSON payload is an object.' });
    expect(summarizePayload('{"spans":[]}')).toHaveProperty('error');
    expect((summarizePayload('{\n  "a": }') as { error: string }).error).toMatch(/line 2/);
  });
});

describe('jsonProblem', () => {
  it('locates the error', () => {
    expect(jsonProblem('{"a": 1}')).toBeUndefined();
    const p = jsonProblem('{\n  "a": 1,\n  oops\n}');
    expect(p?.line).toBe(3);
    expect(p?.column).toBeGreaterThan(0);
  });
});

describe('runPlan and runBlocker', () => {
  it('runs one inline input without assertions as a quick run', () => {
    expect(runPlan(one())).toEqual({ mode: 'quick', input: 0 });
    expect(runPlan(one(logs, 'assertions:', '  - count: {exporter: debug, equals: 1}'))).toEqual({ mode: 'scenario' });
    expect(runPlan(doc('inputs:', '  - {receiver: otlp, file: a.json}'))).toEqual({ mode: 'scenario' });
    expect(runPlan(doc('inputs:', `  - {data: '${logs}'}`, `  - {data: '${logs}'}`))).toEqual({ mode: 'scenario' });
    expect(runPlan('[')).toEqual({ mode: 'scenario' });
  });

  it('blocks a run until its input is usable', () => {
    expect(runBlocker({ scenarioYAML: one() }, false)).toBe('The configuration is not valid');
    expect(runBlocker({ scenarioYAML: one() }, true)).toBeUndefined();
    expect(runBlocker({ scenarioYAML: one('{') }, true)).toMatch(/Not valid JSON/);
    expect(runBlocker({ scenarioYAML: '' }, true)).toBe('Add an input to run');
    expect(runBlocker({ scenarioYAML: doc('environment: {A: b}') }, true)).toBeUndefined();
    expect(runBlocker({ scenarioYAML: one() }, true, 1)).toBe('There is no such input');
    expect(runBlocker({ scenarioYAML: doc('inputs:', '  - {receiver: otlp}') }, true, 0)).toMatch(/file/);
  });
});

describe('runFields', () => {
  it('sends an inline input as a quick run with the document as setup', () => {
    expect(runFields({ scenarioYAML: one() })).toEqual({
      fixture_json: logs,
      receiver: 'otlp',
      client_address: '10.0.0.1',
      setup_yaml: undefined,
    });
    const f = runFields({ scenarioYAML: one(logs, 'environment: {A: b}') }) as { setup_yaml: string };
    expect(parse(f.setup_yaml)).toEqual({ version: 1, environment: { A: 'b' } });
  });

  it('sends the document as a scenario, or one file input alone', () => {
    const y = one(logs, 'assertions:', '  - count: {exporter: x, equals: 1}');
    expect(runFields({ scenarioYAML: y })).toEqual({ scenario_yaml: y });
    const files = doc('inputs:', '  - {file: a.json}', '  - {file: b.json, after: 2s}', 'assertions:', '  - count: {exporter: x, equals: 1}');
    const f = runFields({ scenarioYAML: files }, 1) as { scenario_yaml: string };
    expect(parse(f.scenario_yaml)).toEqual({ version: 1, inputs: [{ file: 'b.json' }] });
  });
});

describe('quickDoc', () => {
  it('builds a document from a quick run', () => {
    const setup = '# the cluster\nmocks:\n  kubernetes:\n    objects: [{apiVersion: v1, kind: Pod, metadata: {name: a}}]\n';
    const r = quickDoc({ fixture: logs, receiver: 'otlp', clientAddress: '10.0.0.1', setup, name: 'from quick run' });
    if ('error' in r) throw new Error(r.error);
    expect(r.yaml).toContain('# the cluster');
    expect(r.yaml.indexOf('version')).toBeLessThan(r.yaml.indexOf('mocks'));
    const v = parse(r.yaml);
    expect(v).toMatchObject({ version: 1, name: 'from quick run', mocks: { kubernetes: { objects: [{ kind: 'Pod' }] } } });
    expect(v.inputs).toHaveLength(1);
    expect(v.inputs[0]).toMatchObject({ receiver: 'otlp', client_address: '10.0.0.1' });
    expect(v.inputs[0].data).toBe(logs);
    expect(runPlan(r.yaml)).toEqual({ mode: 'quick', input: 0 });
  });
});

describe('setupInUse', () => {
  it('is true only when the setup declares something', () => {
    expect(setupInUse('')).toBe(false);
    expect(setupInUse('version: 1\n')).toBe(false);
    expect(setupInUse('environment: {A: b}\n')).toBe(true);
    expect(setupInUse('mocks: [')).toBe(true);
  });
});

describe('scenarioSetup', () => {
  it('keeps mocks and environment, not inputs or assertions', () => {
    const y = scenarioSetup(`version: 1
name: pods
# the cluster
mocks:
  kubernetes:
    objects: []
environment:
  REGION: eu
inputs:
  - receiver: otlp
assertions: []
`);
    expect(y).toContain('# the cluster');
    expect(y).toContain('REGION: eu');
    expect(y).not.toMatch(/inputs|assertions|name:/);
  });

  it('yields nothing without setup or for invalid text', () => {
    expect(scenarioSetup('version: 1\ninputs: []\n')).toBe('');
    expect(scenarioSetup('[')).toBe('');
    expect(scenarioSetup('')).toBe('');
  });
});
