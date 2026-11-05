import { describe, expect, it } from 'vitest';
import type { ArtifactMetadata } from '../api/types';
import { buildSuiteRequest, configOptions, defaultConfig, DRAFT_CONFIG, MAX_SUITE_MEMBERS, suiteBlocker } from './suiteRequest';

const meta = (id: string, kind: string, label: string, updated = '2026-10-20T10:00:00Z'): ArtifactMetadata => ({
  id, project_id: 'default', kind, label, updated, created: updated, generation: 4, head_revision_id: `${id}-head`, workspace_id: 'w',
});

const CONFIGS = [meta('c1', 'configuration', 'old.yaml', '2026-10-01T00:00:00Z'), meta('c2', 'configuration', 'checkout.yaml')];
const SCENARIOS = [meta('s1', 'scenario', 'debug-error-logs'), meta('s2', 'scenario', 'tail-sampling')];
const DRAFT = { yaml: 'receivers: {}', path: 'C:\\work\\draft.yaml' };

describe('configOptions', () => {
  it('lists the draft first, then saved configurations newest first', () => {
    expect(configOptions(DRAFT, CONFIGS)).toEqual([
      { value: DRAFT_CONFIG, label: 'The open configuration · draft.yaml' },
      { value: 'saved:c2', label: 'checkout.yaml · rev 4' },
      { value: 'saved:c1', label: 'old.yaml · rev 4' },
    ]);
  });
  it('leaves out an empty draft', () => {
    const o = configOptions({ yaml: ' \n', path: '' }, CONFIGS);
    expect(o.map((x) => x.value)).toEqual(['saved:c2', 'saved:c1']);
    expect(defaultConfig(o)).toBe('saved:c2');
    expect(defaultConfig([])).toBe('');
  });
  it('words an unnamed draft', () => {
    expect(configOptions({ yaml: 'a: 1', path: '' }, [])[0].label).toBe('The open configuration');
  });
});

describe('suiteBlocker', () => {
  const options = configOptions(DRAFT, CONFIGS);
  it.each([
    [0, DRAFT_CONFIG, 'Select at least one scenario.'],
    [MAX_SUITE_MEMBERS + 1, DRAFT_CONFIG, 'A suite holds at most 32 scenarios; 33 are selected.'],
    [2, '', 'Choose a configuration: write one in the debugger or save one.'],
    [2, 'saved:gone', 'Choose a configuration: write one in the debugger or save one.'],
    [2, 'saved:c1', undefined],
    [MAX_SUITE_MEMBERS, DRAFT_CONFIG, undefined],
  ])('%i members with %j', (n, config, want) => expect(suiteBlocker(n, config, options)).toBe(want));
});

describe('buildSuiteRequest', () => {
  it('sends the draft as text and scenarios by head revision', () => {
    expect(buildSuiteRequest(DRAFT_CONFIG, DRAFT, CONFIGS, SCENARIOS)).toEqual({
      config_yaml: 'receivers: {}',
      config_label: 'draft.yaml',
      scenarios: [
        { scenario_ref: { artifact_id: 's1', revision_id: 's1-head' }, scenario_label: 'debug-error-logs' },
        { scenario_ref: { artifact_id: 's2', revision_id: 's2-head' }, scenario_label: 'tail-sampling' },
      ],
    });
  });
  it('names a saved configuration by its head revision and carries the runtime', () => {
    const req = buildSuiteRequest('saved:c2', DRAFT, CONFIGS, SCENARIOS.slice(0, 1), 'acme');
    expect(req).toMatchObject({ config_ref: { artifact_id: 'c2', revision_id: 'c2-head' }, config_label: 'checkout.yaml', runtime: 'acme' });
    expect(req?.config_yaml).toBeUndefined();
  });
  it('omits the runtime for the built-in one', () => {
    expect(buildSuiteRequest('saved:c2', DRAFT, CONFIGS, SCENARIOS, '')).not.toHaveProperty('runtime');
  });
  it('is undefined for a configuration that is gone or an empty draft', () => {
    expect(buildSuiteRequest('saved:gone', DRAFT, CONFIGS, SCENARIOS)).toBeUndefined();
    expect(buildSuiteRequest(DRAFT_CONFIG, { yaml: '', path: '' }, CONFIGS, SCENARIOS)).toBeUndefined();
  });
});
