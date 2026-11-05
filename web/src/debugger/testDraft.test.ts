import { describe, expect, it } from 'vitest';
import type { Graph, Report, RunInputs } from '../api/types';
import type { WorkspaceState } from '../workspace/store';
import { baseYaml, destinationText, existingRows, newSuggestions, suggestionEvidence, suggestionText, testBase, testDraftPatch, withConfirmed } from './testDraft';

const topology = {
  pipelines: [{ id: 'logs', signal: 'logs', receivers: ['receiver/otlp'], processors: ['processor/filter/noise'], exporters: ['exporter/otlp/backend'] }],
  components: [
    { ref: 'receiver/otlp', id: 'otlp', kind: 'receiver', type: 'otlp', mode: 'real', pipelines: ['logs'] },
    { ref: 'processor/filter/noise', id: 'filter/noise', kind: 'processor', type: 'filter', mode: 'real', pipelines: ['logs'] },
    { ref: 'exporter/otlp/backend', id: 'otlp/backend', kind: 'exporter', type: 'otlp', mode: 'captured', pipelines: ['logs'] },
  ],
  edges: [],
} as unknown as Graph;

const report = {
  result: {
    mode: 'scenario',
    status: 'completed',
    outputs: [{ exporter: 'otlp/backend', signal: 'logs', batches: 1, counts: { items: 3, resources: 1, scopes: 1 } }],
    events: [
      { kind: 'stage.input', component: 'filter/noise', pipeline: 'logs', signal: 'logs', counts: { items: 5 } },
      { kind: 'stage.output', component: 'filter/noise', pipeline: 'logs', signal: 'logs', counts: { items: 3 } },
    ],
  },
} as unknown as Report;

const withCount = `version: 1
inputs:
  - {receiver: otlp, file: a.json}
assertions:
  - target: {exporter: otlp/backend, signal: logs}
    count: {equals: 3}
`;

describe('add assertions', () => {
  it('adds to the run scenario, or to a new one for a run without one', () => {
    const inputs = { run_id: 'r', mode: 'scenario', config: { text: 'c', sha256: 'x' }, scenario: { text: withCount, sha256: 'y', label: 'smoke.yaml' } } as RunInputs;
    expect(testBase(inputs)).toBe('update');
    expect(destinationText('update', inputs)).toContain('“smoke.yaml”');
    expect(testBase({ ...inputs, mode: 'quick', scenario: undefined })).toBe('new');
    expect(testBase(undefined)).toBe('new');
    expect(destinationText('new', undefined)).toContain('a new scenario');
  });

  it('starts from the run scenario, or from a derived one', () => {
    const inputs = { run_id: 'r', mode: 'scenario', config: { text: 'c', sha256: 'x' }, scenario: { text: withCount, sha256: 'y' } } as RunInputs;
    expect(baseYaml('update', inputs, undefined)).toBe(withCount);
    expect(baseYaml('update', { ...inputs, mode: 'quick' }, undefined)).toBeUndefined();
    expect(baseYaml('new', inputs, { run_id: 'r', yaml: 'derived' })).toBe('derived');
    expect(baseYaml('new', inputs, undefined)).toBeUndefined();
  });

  it('lists what the scenario asserts and only suggests what it does not', () => {
    expect(existingRows(withCount)).toHaveLength(1);
    expect(existingRows('not: [valid')).toEqual([]);
    const s = newSuggestions(report, topology, withCount);
    expect(s.map((x) => x.key)).toEqual(['dropped|p|filter/noise|logs|logs']);
    expect(suggestionText(s[0])).toContain('filter/noise');
    expect(suggestionEvidence(s[0])).toBe('Observed 2 dropped in this run');
    expect(newSuggestions(report, topology, 'version: 1\n').map((x) => x.key)).toEqual(['count|e|otlp/backend|logs', 'dropped|p|filter/noise|logs|logs']);
    expect(suggestionEvidence(newSuggestions(report, topology, 'version: 1\n')[0])).toBe('Observed 3 records in this run');
  });

  it('adds only the confirmed suggestions', () => {
    const s = newSuggestions(report, topology, withCount);
    expect(withConfirmed(withCount, [])).toEqual({ yaml: withCount });
    const out = withConfirmed(withCount, s);
    expect('yaml' in out && existingRows(out.yaml)).toHaveLength(2);
  });

  it('keeps the saved source of the scenario it updates', () => {
    const ws = { configYAML: 'c', configPath: '', scenarioYAML: 'old', scenarioPath: '', compare: false } as WorkspaceState;
    const inputs = {
      run_id: 'r', mode: 'scenario', config: { text: 'c', sha256: 'x' },
      scenario: { text: withCount, sha256: 'y', label: 'debug.yaml', artifact: { artifact_id: 'a1', revision_id: 'v3' } },
    } as RunInputs;
    const updated = testDraftPatch(ws, 'update', withCount + '# more\n', inputs);
    expect(updated.patch.scenarioPath).toBe('debug.yaml');
    expect(updated.patch.saved?.scenario).toEqual({ artifactId: 'a1', revisionId: 'v3' });
    expect(updated.patch.baselines?.scenario).toBe(withCount);
    const fresh = testDraftPatch(ws, 'new', 'derived', inputs, ['TOKEN']);
    expect(fresh.patch.saved?.scenario).toBeUndefined();
    expect(fresh.patch.scenarioPath).toBe('');
    expect(fresh.notes.join(' ')).toContain('TOKEN');
    expect(fresh.patch.configYAML).toBeUndefined();
    const empty = testDraftPatch({ ...ws, configYAML: '' }, 'new', 'derived', inputs);
    expect(empty.patch.configYAML).toBe('c');
    expect(empty.patch.baselines?.config).toBe('c');
  });
});
