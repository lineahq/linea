import { describe, expect, it } from 'vitest';
import type { Runtime, UpgradeCheck } from '../api/types';
import {
  compareVersions, diagnosticRows, gateState, retargetNotes, scenarioRows, upgradeProblem, upgradeRequest, upgradeSides, verdictText,
} from './upgrade';

const runtime = (name: string, version: string, verified: boolean): Runtime => ({
  name, collector_version: version, verified, go_image: 'golang', image_id: `sha256:${name}`, manifest: '',
  module_set: '', product_version: 'dev', protocol: 1, size: 70_000_000,
});

const check = (patch: Partial<UpgradeCheck>): UpgradeCheck => ({
  upgrade_check_schema: '0.1',
  verdict: 'compatible',
  from: { name: 'built-in', collector_version: '0.162.0', verified: true },
  to: { name: 'c163', collector_version: '0.163.0', verified: false },
  configuration: { from_valid: true, to_valid: true, added: [], removed: [] },
  feature_gates: {},
  scenarios: [],
  ...patch,
});

const ws = { configYAML: 'receivers: {}', configPath: '', scenarioYAML: '', scenarioPath: '' };

describe('upgradeSides', () => {
  it('lists the built-in runtime, then built ones by release, numerically', () => {
    const sides = upgradeSides([runtime('new', '0.163.0', false), runtime('old', '0.99.0', true), runtime('mid', '0.160.0', true)], '0.162.0');
    expect(sides.map((s) => s.label)).toEqual([
      'Built-in · Collector 0.162.0', 'old · Collector 0.99.0', 'mid · Collector 0.160.0', 'new · Collector 0.163.0 · unverified',
    ]);
    expect(sides.at(-1)?.unverified).toBe(true);
    expect(compareVersions('0.160.0', '0.160.0')).toBe(0);
  });
});

describe('upgradeRequest', () => {
  it('names the built-in target and leaves out an empty from', () => {
    expect(upgradeRequest(ws, 'c160', '')).toEqual({ to: 'built-in', from: 'c160', config_yaml: 'receivers: {}' });
    expect(upgradeRequest(ws, '', 'c163')).toEqual({ to: 'c163', config_yaml: 'receivers: {}' });
  });

  it('sends the scenario draft with its label and directory', () => {
    const req = upgradeRequest({ ...ws, configPath: 'c.yaml', scenarioYAML: 'inputs: []', scenarioPath: 's.yaml' }, '', 'c163');
    expect(req.config_label).toBe('c.yaml');
    expect(req.scenarios).toEqual([{ scenario_yaml: 'inputs: []', scenario_label: 's.yaml' }]);
  });

  it('refuses an empty configuration and the same runtime twice', () => {
    expect(upgradeProblem(' ', '', 'c163')).toMatch(/configuration/);
    expect(upgradeProblem('x', 'c163', 'c163')).toMatch(/two different/);
    expect(upgradeProblem('x', '', 'c163')).toBeUndefined();
  });
});

describe('verdictText', () => {
  it('never words inconclusive or a check without scenarios as a full pass', () => {
    expect(verdictText(check({ verdict: 'inconclusive' }))).toEqual({ text: expect.stringContaining('not a pass'), cls: 'warn' });
    expect(verdictText(check({})).text).toContain('no scenario compared behavior');
    expect(verdictText(check({ verdict: 'configuration_breaks' })).cls).toBe('bad');
  });
});

describe('diagnosticRows and gateState', () => {
  it('lists new diagnostics before gone ones', () => {
    const c = check({ configuration: { from_valid: true, to_valid: false,
      added: [{ severity: 'error', code: 'invalid_config', component: 'processors::transform', message: 'unknown key' }],
      removed: [{ severity: 'warning', code: 'deprecated', message: 'old' }] } });
    expect(diagnosticRows(c)).toEqual([
      { change: 'new', severity: 'error', code: 'invalid_config', component: 'processors::transform', message: 'unknown key' },
      { change: 'gone', severity: 'warning', code: 'deprecated', component: '', message: 'old' },
    ]);
    expect(gateState(undefined)).toBe('absent');
    expect(gateState({ stage: 'beta', enabled: true })).toBe('beta · on');
  });
});

describe('scenarioRows', () => {
  it('states same, changed assertions and scenarios not compared', () => {
    const c = check({ scenarios: [
      { name: 'a', outcome: 'same', inputs_identical: true, before: 'r1', after: 'r2', before_assertions: 'pass', after_assertions: 'pass' },
      { name: 'b', outcome: 'same', inputs_identical: true, before_assertions: 'pass', after_assertions: 'fail' },
      { name: 'c', inputs_identical: false, error: 'the queue is full' },
    ] });
    const rows = scenarioRows(c);
    expect(rows[0]).toEqual({ name: 'a', result: 'Same', cls: 'good', assertions: 'pass → pass', inputs: 'identical', runs: ['r1', 'r2'] });
    expect(rows[1].cls).toBe('bad');
    expect(rows[2]).toMatchObject({ result: 'Not compared: the queue is full', cls: 'warn' });
  });
});

describe('retargetNotes', () => {
  it('warns about missing and custom modules', () => {
    const notes = retargetNotes({ manifest: '', from: '0.162.0', to: '0.160.0', moved: ['a', 'b'], missing: ['m'], custom: ['x'], notes: [] });
    expect(notes.map((n) => n.warn)).toEqual([false, true, true]);
    expect(notes[0].text).toBe('Moved from 0.162.0 to 0.160.0: 2 module versions changed.');
  });
});
