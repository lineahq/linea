import { describe, expect, it } from 'vitest';
import type { ArtifactMetadata } from '../api/types';
import type { WorkspaceState } from '../workspace/store';
import { bytesToBase64, ciExportBlocker, exportPlanText, importSummary, MAX_EXPORT_SCENARIOS, openExportPlan, savedConfigId } from './ciArchive';

const scenario = (id: string): ArtifactMetadata => ({ workspace_id: 'w', id, project_id: 'default', kind: 'scenario', label: `${id}.yaml`, generation: 1, head_revision_id: 'r', created: '', updated: '' });

describe('ciExportBlocker', () => {
  it('exports a saved configuration with scenarios', () => {
    expect(ciExportBlocker('saved:c1', [scenario('a')])).toBeUndefined();
    expect(savedConfigId('saved:c1')).toBe('c1');
  });
  it('refuses the unsaved draft', () => {
    expect(ciExportBlocker('draft', [scenario('a')])).toMatch(/save the open configuration first/);
    expect(savedConfigId('draft')).toBeUndefined();
  });
  it('needs a configuration and 1 to 32 scenarios', () => {
    expect(ciExportBlocker('', [scenario('a')])).toMatch(/Pick a saved configuration/);
    expect(ciExportBlocker('saved:c1', [])).toMatch(/at least one/);
    expect(ciExportBlocker('saved:c1', Array.from({ length: MAX_EXPORT_SCENARIOS + 1 }, (_, i) => scenario(`s${i}`)))).toMatch(/at most 32/);
  });
});

describe('importSummary', () => {
  it('counts each outcome it saw', () => {
    const artifact = scenario('a');
    expect(importSummary({ items: [
      { path: 'collector/config.yaml', status: 'unchanged', artifact },
      { path: 'collector/tests/a.yaml', status: 'revised', artifact },
      { path: 'collector/tests/b.yaml', status: 'created', artifact },
      { path: 'collector/tests/c.yaml', status: 'created', artifact },
    ] })).toBe('Imported 4 files: 2 created, 1 revised, 1 unchanged.');
  });
});

describe('bytesToBase64', () => {
  it('encodes bytes beyond one chunk', () => {
    const bytes = new Uint8Array(0x8000 + 3).map((_, i) => i % 256);
    expect(atob(bytesToBase64(bytes)).length).toBe(bytes.length);
    expect(bytesToBase64(new Uint8Array([80, 75, 3, 4]))).toBe('UEsDBA==');
  });
});

describe('openExportPlan', () => {
  const config: ArtifactMetadata = { ...scenario('c'), kind: 'configuration', head_revision_id: 'c2' };
  const lists = { configuration: [config], scenario: [scenario('s')] };
  const ws = (over: Partial<WorkspaceState> = {}): WorkspaceState => ({
    configYAML: 'service: {}\n', configPath: '', scenarioYAML: 'version: 1\n', scenarioPath: '', compare: false,
    baselines: { config: 'service: {}\n', scenario: 'version: 1\n' },
    saved: { configuration: { artifactId: 'c', revisionId: 'c2' }, scenario: { artifactId: 's', revisionId: 'r' } },
    ...over,
  });
  it('downloads straight away when both are saved at their heads', () => {
    expect(openExportPlan(ws(), lists)).toEqual({ saves: [] });
    expect(exportPlanText([])).toMatch(/^Downloads collector-ci.zip/);
  });
  it('saves what was never saved or has changed first', () => {
    expect(openExportPlan(ws({ saved: {} }), lists)).toEqual({ saves: ['configuration', 'scenario'] });
    expect(openExportPlan(ws({ scenarioYAML: 'version: 1\nname: x\n' }), lists)).toEqual({ saves: ['scenario'] });
    expect(exportPlanText(['configuration', 'scenario'])).toMatch(/^Saves the configuration and the scenario, then downloads/);
    expect(exportPlanText(['scenario'])).toMatch(/^Saves the scenario, then downloads/);
  });
  it('says why it cannot export', () => {
    expect(openExportPlan(ws(), { configuration: undefined, scenario: undefined })).toEqual({ blocker: expect.stringMatching(/saved work/) });
    expect(openExportPlan(ws({ scenarioYAML: '' }), lists)).toEqual({ blocker: expect.stringMatching(/Add a scenario first/) });
    expect(openExportPlan(ws({ saved: { configuration: { artifactId: 'c', revisionId: 'c1' } } }), lists)).toEqual({ blocker: expect.stringMatching(/earlier revision/) });
  });
});
