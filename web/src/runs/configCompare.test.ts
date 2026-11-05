import { describe, expect, it } from 'vitest';
import type { ArtifactMetadata, Run, RunInputs, ScenarioFromRun } from '../api/types';
import { configComparisonRequest } from './configCompare';

const inputs = { run_id: 'run-0123456789', mode: 'quick', config: { text: 'receivers: {}\n', label: 'otel.yaml', sha256: 'a' } } as RunInputs;
const derived = { run_id: 'run-0123456789', yaml: 'version: 1\ninputs: []\n' } as ScenarioFromRun;
const run = (runtime?: Run['report']) => ({ id: 'run-0123456789', created: '', state: 'done', report: runtime }) as Run;
const saved = { id: 'c1', head_revision_id: 'c1-r3', label: 'next.yaml' } as ArtifactMetadata;

describe('compare a run with another configuration', () => {
  it('runs the recorded configuration and a saved one on the derived scenario', () => {
    expect(configComparisonRequest(run(), inputs, derived, { kind: 'saved', artifact: saved })).toEqual({
      before: { config_yaml: 'receivers: {}\n', config_label: 'otel.yaml' },
      after: { config_ref: { artifact_id: 'c1', revision_id: 'c1-r3' }, config_label: 'next.yaml' },
      scenario_yaml: 'version: 1\ninputs: []\n',
      scenario_label: 'run 01234567',
    });
  });

  it('runs the open configuration and keeps the run’s scenario label', () => {
    const req = configComparisonRequest(run(), { ...inputs, mode: 'scenario', scenario: { text: 'x', label: 'tests/a.yaml', sha256: 'b' } }, derived, { kind: 'draft', yaml: 'exporters: {}\n' });
    expect(req.after).toEqual({ config_yaml: 'exporters: {}\n' });
    expect(req.scenario_label).toBe('tests/a.yaml');
  });

  it('uses the run’s runtime unless it was the built-in one', () => {
    const builtIn = run({ runtime: { kind: 'built_in', name: 'ignored', collector_version: '0.162.0' } } as Run['report']);
    expect(configComparisonRequest(builtIn, inputs, derived, { kind: 'draft', yaml: 'y' }).runtime).toBeUndefined();
    const built = run({ runtime: { kind: 'container', name: 'acme', collector_version: '0.161.0' } } as Run['report']);
    expect(configComparisonRequest(built, inputs, derived, { kind: 'draft', yaml: 'y' }).runtime).toBe('acme');
  });
});
