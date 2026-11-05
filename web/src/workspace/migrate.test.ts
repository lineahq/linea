import { describe, expect, it } from 'vitest';
import { parse } from 'yaml';
import { migrate } from './migrate';
import type { WorkspaceState } from './store';

const base: WorkspaceState = { configYAML: 'c', configPath: '', scenarioYAML: '', scenarioPath: '', compare: false };
const legacy = (patch: Record<string, unknown>) => migrate({ ...base, ...patch } as WorkspaceState);
const payload = '{"resourceLogs":[]}';

describe('migrate', () => {
  it('leaves a current workspace alone', () => {
    const ws = { ...base, scenarioYAML: 'version: 1\n' };
    expect(migrate(ws)).toBe(ws);
  });

  it('drops a stored scenario directory, which the server never reads', () => {
    const ws = migrate({ ...base, scenarioYAML: 'version: 1\n', scenarioDir: '/work' } as WorkspaceState);
    expect(ws).toEqual({ ...base, scenarioYAML: 'version: 1\n' });
  });

  it('turns a quick draft into a one-input document with its mocks', () => {
    const ws = legacy({
      inputJSON: payload,
      inputReceiver: 'otlp',
      inputClientAddress: '10.0.0.1',
      quickSetupYAML: 'environment: {A: b}\n',
      runMode: 'quick',
      inputName: 'in.json',
      baselines: { config: 'c', quick: 'x' },
      saved: { fixture: { artifactId: 'f', revisionId: 'r' } },
    });
    expect(parse(ws.scenarioYAML)).toEqual({
      version: 1,
      environment: { A: 'b' },
      inputs: [{ receiver: 'otlp', client_address: '10.0.0.1', data: payload }],
    });
    expect(Object.keys(ws).filter((k) => k.startsWith('input') || k === 'runMode' || k === 'quickSetupYAML')).toEqual([]);
    expect(ws.baselines).toEqual({ config: 'c', scenario: undefined });
    expect(ws.saved).toEqual({});
  });

  it('keeps the scenario when the user was in scenario mode', () => {
    const ws = legacy({ scenarioYAML: 'version: 1\nname: mine\n', inputJSON: payload, runMode: 'scenario' });
    expect(ws.scenarioYAML).toBe('version: 1\nname: mine\n');
    expect('inputJSON' in ws).toBe(false);
  });

  it('adds a quick input to an existing scenario draft without losing either', () => {
    const ws = legacy({
      scenarioYAML: 'version: 1\n# mine\nenvironment: {A: mine}\ninputs:\n  - file: a.json\n',
      inputJSON: payload,
      inputReceiver: 'otlp',
      quickSetupYAML: 'environment: {A: quick}\nmocks: {files: [{path: /x, content: y}]}\n',
      runMode: 'quick',
    });
    expect(ws.scenarioYAML).toContain('# mine');
    expect(parse(ws.scenarioYAML)).toEqual({
      version: 1,
      environment: { A: 'mine' },
      inputs: [{ receiver: 'otlp', data: payload }, { file: 'a.json' }],
      mocks: { files: [{ path: '/x', content: 'y' }] },
    });
  });

  it('drops an empty quick draft', () => {
    const ws = legacy({ inputJSON: '  ', runMode: 'quick' });
    expect(ws.scenarioYAML).toBe('');
  });
});
