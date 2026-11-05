import { describe, expect, it } from 'vitest';
import type { RunInputs } from '../api/types';
import { confirmText, decodeText, dirtyDrafts, encodeText, isDirty, loaded, newLabel, newPatch, saveLabel, yamlName, openedPatch, runDraft, runExportDraft, runExportNeedsScenario } from './drafts';
import type { WorkspaceState } from './store';
import { parse } from 'yaml';

const base: WorkspaceState = {
  configYAML: '',
  configPath: '',
  scenarioYAML: '',
  scenarioPath: '',
  compare: false,
};

const quickRun: RunInputs = {
  run_id: 'r1',
  mode: 'quick',
  config: { text: 'receivers: {}\n', label: 'saved.yaml', sha256: 'a', artifact: { artifact_id: 'c1', revision_id: 'rev1' } },
  fixture: { text: '{"resourceLogs":[]}', label: 'input.json', sha256: 'b' },
  setup: { text: 'environment: {A: b}\n', sha256: 'c' },
  assertions: { text: 'assertions: []\n', sha256: 'd' },
  receiver: 'otlp',
  client_address: '10.0.0.1',
  files: [],
  required_environment: ['TOKEN'],
};

const scenarioRun: RunInputs = {
  run_id: 'r2',
  mode: 'scenario',
  config: { text: 'exporters: {}\n', sha256: 'e' },
  scenario: { text: 'version: 1\n', label: 'tests/a.yaml', sha256: 'f', artifact: { artifact_id: 's1', revision_id: 'rev2' } },
  files: ['data/input.json'],
  required_environment: [],
};

describe('dirty drafts', () => {
  it('treats content without a baseline as dirty and empty drafts as clean', () => {
    expect(isDirty(base, 'config')).toBe(false);
    expect(isDirty(base, 'scenario')).toBe(false);
    expect(isDirty({ ...base, configYAML: 'x' }, 'config')).toBe(true);
    expect(isDirty({ ...base, scenarioYAML: 'version: 1\n' }, 'scenario')).toBe(true);
    expect(isDirty({ ...base, scenarioYAML: '  \n' }, 'scenario')).toBe(false);
  });
  it('compares with the last loaded or saved baseline', () => {
    const ws = { ...base, ...loaded(base, { configYAML: 'a', scenarioYAML: 'version: 1\n' }, ['config', 'scenario']) };
    expect(dirtyDrafts(ws, ['config', 'scenario'])).toEqual([]);
    expect(isDirty({ ...ws, configYAML: 'b' }, 'config')).toBe(true);
    expect(isDirty({ ...ws, scenarioYAML: 'version: 1\nmocks: {}\n' }, 'scenario')).toBe(true);
    // Emptying a loaded draft is an edit too.
    expect(isDirty({ ...ws, configYAML: '' }, 'config')).toBe(true);
  });
  it('names the drafts a replacement would lose', () => {
    expect(confirmText(['config'])).toBe('Replace your unsaved configuration?');
    expect(confirmText(['config', 'scenario'])).toBe('Replace your unsaved configuration and scenario?');
  });
});

describe('using a run as draft', () => {
  const ws: WorkspaceState = { ...base, scenarioYAML: 'version: 1\nname: mine\n' };
  it('opens a quick run as the configuration and a one-input document', () => {
    const d = runDraft(ws, quickRun);
    expect(d.drafts).toEqual(['config', 'scenario']);
    const next = { ...ws, ...d.patch };
    expect(next).toMatchObject({ configYAML: 'receivers: {}\n', configPath: 'saved.yaml', scenarioPath: '' });
    expect(parse(next.scenarioYAML)).toEqual({
      version: 1,
      environment: { A: 'b' },
      inputs: [{ receiver: 'otlp', client_address: '10.0.0.1', data: '{"resourceLogs":[]}' }],
      assertions: [],
    });
    expect(next.saved).toEqual({ configuration: { artifactId: 'c1', revisionId: 'rev1' }, scenario: undefined });
    expect(dirtyDrafts(next, ['config', 'scenario'])).toEqual([]);
    expect(d.notes.join(' ')).toContain('TOKEN');
  });
  it('replaces the configuration and scenario of a scenario run', () => {
    const d = runDraft(ws, scenarioRun);
    expect(d.drafts).toEqual(['config', 'scenario']);
    const next = { ...ws, ...d.patch };
    expect(next).toMatchObject({ configYAML: 'exporters: {}\n', configPath: '', scenarioYAML: 'version: 1\n', scenarioPath: 'tests/a.yaml' });
    expect(next.saved?.scenario).toEqual({ artifactId: 's1', revisionId: 'rev2' });
    expect(next.saved?.configuration).toBeUndefined();
    expect(d.notes).toEqual([expect.stringContaining('data/input.json')]);
  });
});

describe('new work', () => {
  it('starts from the template with an empty scenario, clean and saved nowhere', () => {
    const ws: WorkspaceState = {
      ...base,
      configYAML: 'old', configPath: 'a.yaml', scenarioYAML: 'version: 1\n', scenarioPath: 's.yaml',
      saved: { configuration: { artifactId: 'a', revisionId: 'r' } }, lastRunId: 'run-1',
    };
    const next = { ...ws, ...newPatch(ws, 'service: {}\n') };
    expect(next).toMatchObject({ configYAML: 'service: {}\n', configPath: '', scenarioYAML: '', scenarioPath: '', saved: {}, lastRunId: undefined });
    expect(isDirty(next, 'config')).toBe(false);
    expect(isDirty(next, 'scenario')).toBe(false);
  });
});

describe('saved sources', () => {
  it('opens text as the draft baseline and remembers its revision', () => {
    const next = { ...base, ...openedPatch(base, 'scenario', 'version: 1\n', 'a.yaml', { artifactId: 's', revisionId: 'r' }) };
    expect(next.scenarioYAML).toBe('version: 1\n');
    expect(next.saved?.scenario).toEqual({ artifactId: 's', revisionId: 'r' });
    expect(isDirty(next, 'scenario')).toBe(false);
    const file = { ...next, ...openedPatch(next, 'scenario', 'version: 1\n', 'local.yaml') };
    expect(file.saved?.scenario).toBeUndefined();
  });
  it('labels new artifacts after the draft file', () => {
    expect(newLabel({ ...base, configPath: 'C:\\work\\otel.yaml' }, 'configuration')).toBe('otel.yaml');
    expect(newLabel(base, 'scenario')).toBe('scenario.yaml');
  });
  it('labels a new scenario by its name before its file', () => {
    const named = { ...base, scenarioPath: '/work/scenario.yaml', scenarioYAML: 'name: drop debug logs\ninput: {}\n' };
    expect(newLabel(named, 'scenario')).toBe('drop debug logs.yaml');
    expect(newLabel({ ...named, scenarioYAML: 'input: {}\n' }, 'scenario')).toBe('scenario.yaml');
    expect(newLabel({ ...named, scenarioYAML: 'name: [unclosed' }, 'scenario')).toBe('scenario.yaml');
  });
  it('saves under the typed name, or the fallback when it is blank', () => {
    expect(saveLabel('  checkout pipeline ', 'config.yaml')).toBe('checkout pipeline.yaml');
    expect(saveLabel('   ', 'config.yaml')).toBe('config.yaml');
    expect(saveLabel(undefined, 'scenario.yaml')).toBe('scenario.yaml');
    expect(saveLabel(undefined, 'old name')).toBe('old name.yaml');
    const long = saveLabel('x'.repeat(2000), 'f');
    expect(long).toHaveLength(1024);
    expect(long.endsWith('.yaml')).toBe(true);
  });
  it('always names saved work as a YAML file', () => {
    expect(yamlName('config.yaml')).toBe('config.yaml');
    expect(yamlName('otel.yml')).toBe('otel.yml');
    expect(yamlName('Otel.YAML')).toBe('Otel.YAML');
    expect(yamlName('config.bin')).toBe('config.bin.yaml');
    expect(yamlName('config.yaml.exe')).toBe('config.yaml.exe.yaml');
    expect(yamlName('config')).toBe('config.yaml');
  });
  it('round-trips exact UTF-8 text through base64', () => {
    const text = 'name: café ✓\n';
    expect(decodeText(encodeText(text))).toBe(text);
  });
});

describe('export a run for CI', () => {
  it('derives a scenario only for a run that read files beside its scenario', () => {
    expect(runExportNeedsScenario(quickRun)).toBe(false);
    expect(runExportNeedsScenario(scenarioRun)).toBe(true);
  });

  it('keeps saved sources linked when the run needs no derived scenario', () => {
    const d = runExportDraft(base, quickRun);
    expect(d.patch.saved?.configuration).toEqual({ artifactId: 'c1', revisionId: 'rev1' });
    expect(d.drafts).toEqual(['config', 'scenario']);
  });

  it('opens the derived scenario unsaved in place of one that read files', () => {
    const yaml = 'version: 1\ninputs: []\n';
    const d = runExportDraft(base, scenarioRun, { run_id: 'r2', yaml, required_environment: [] } as never);
    const ws = { ...base, ...d.patch };
    expect(ws.scenarioYAML).toBe(yaml);
    expect(ws.saved?.scenario).toBeUndefined();
    expect(isDirty(ws, 'scenario')).toBe(true);
    expect(isDirty(ws, 'config')).toBe(false);
    expect(d.notes.some((n) => n.startsWith('The run read'))).toBe(false);
  });
});
