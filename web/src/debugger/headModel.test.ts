import { describe, expect, it } from 'vitest';
import type { ArtifactMetadata, Run } from '../api/types';
import type { WorkspaceState } from '../workspace/store';
import { elapsedText, progressFacts, runConfigName, runDuration, runMeta, saveState, type SaveState } from './headModel';

const run = (over: Partial<Run>): Run => ({ id: 'run-0123456789abcdef', project_id: 'default', created: '2026-10-20T14:02:00', state: 'done', ...over });

describe('elapsedText', () => {
  it.each([[1100, '1.1 s'], [0, '0.0 s'], [-5, '0.0 s'], [59_940, '59.9 s'], [123_400, '2 min 3 s']])('%d ms', (ms, want) => {
    expect(elapsedText(ms)).toBe(want);
  });
});

describe('runDuration and runMeta', () => {
  it('needs both ends', () => {
    expect(runDuration({ started: '2026-10-20T14:02:00Z' })).toBeUndefined();
    expect(runDuration({ started: '2026-10-20T14:02:00Z', finished: '2026-10-20T14:02:01.400Z' })).toBe('1.4 s');
  });
  it('words a finished run without a state, and a failed one with it', () => {
    const times = { started: '2026-10-20T14:02:00Z', finished: '2026-10-20T14:02:30Z' };
    const now = new Date('2026-10-20T18:00:00');
    expect(runMeta(run({ ...times }), now)).toBe('Today 14:02 · 30.0 s');
    expect(runMeta(run({ ...times, state: 'error' }), now)).toBe('Today 14:02 · failed after 30.0 s');
    expect(runMeta(run({ state: 'cancelled' }), now)).toBe('Today 14:02 · cancelled');
  });
});

describe('saveState', () => {
  const items = [{ id: 'a', label: 'checkout.yaml', head_revision_id: 'v3', generation: 3 }] as ArtifactMetadata[];
  const ws = (over: Partial<WorkspaceState>): WorkspaceState => ({
    configYAML: 'service: {}\n', configPath: '', scenarioYAML: '', scenarioPath: '', compare: false, ...over,
  });
  const clean = { baselines: { config: 'service: {}\n' } };
  it.each<[string, Partial<WorkspaceState>, ArtifactMetadata[] | undefined, SaveState]>([
    ['new, never saved', {}, items, { name: 'Untitled configuration', tone: 'unsaved', text: 'Not saved' }],
    ['opened from a file', { ...clean, configPath: 'C:/x/logs.yaml' }, items, { name: 'logs.yaml', tone: 'unsaved', text: 'Not saved' }],
    ['saved, at the head', { ...clean, saved: { configuration: { artifactId: 'a', revisionId: 'v3' } } }, items, { name: 'checkout.yaml', tone: 'saved', text: 'Saved · revision 3' }],
    ['saved, an earlier revision', { ...clean, saved: { configuration: { artifactId: 'a', revisionId: 'v2' } } }, items, { name: 'checkout.yaml', tone: 'saved', text: 'Saved · earlier revision' }],
    ['saved, list not loaded', { ...clean, configPath: 'logs.yaml', saved: { configuration: { artifactId: 'a', revisionId: 'v3' } } }, undefined, { name: 'logs.yaml', tone: 'saved', text: 'Saved' }],
    ['edited after saving', { baselines: { config: 'old' }, saved: { configuration: { artifactId: 'a', revisionId: 'v3' } } }, items, { name: 'checkout.yaml', tone: 'changed', text: 'Unsaved changes' }],
  ])('configuration: %s', (_, over, list, want) => expect(saveState(ws(over), 'configuration', list)).toEqual(want));

  it('names a scenario by its name, then its file', () => {
    expect(saveState(ws({ scenarioYAML: 'version: 1\nname: smoke\n' }), 'scenario', []).name).toBe('smoke');
    expect(saveState(ws({ scenarioYAML: 'version: 1\n', scenarioPath: 'a/b.yaml' }), 'scenario', []).name).toBe('b.yaml');
    expect(saveState(ws({ scenarioYAML: 'version: 1\n' }), 'scenario', []).name).toBe('Untitled scenario');
    expect(runConfigName(undefined)).toBe('configuration');
  });
});

describe('progressFacts', () => {
  it('counts events and the distinct components that reported', () => {
    expect(progressFacts([])).toEqual({ events: 0, components: 0 });
    const e = (component: string | undefined, pipeline?: string) => ({ kind: 'stage.input', component, pipeline });
    expect(progressFacts([e('a', 'logs'), e('a', 'logs'), e('a', 'traces'), e(undefined)])).toEqual({ events: 4, components: 2 });
  });
});
