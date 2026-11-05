import { describe, expect, it } from 'vitest';
import type { UpgradeCheck } from '../../api/types';
import { checkedLine, compareSearch, diffRows, firstDifference, sideLine, summaryOf, upgradeFacets } from './upgradeView';

const check = (patch: Partial<UpgradeCheck>): UpgradeCheck => ({
  upgrade_check_schema: '0.1',
  verdict: 'compatible',
  from: { name: 'built-in', collector_version: '0.136.0', verified: true },
  to: { name: 'acme', collector_version: '0.137.0', verified: true },
  configuration: { from_valid: true, to_valid: true, added: [], removed: [] },
  feature_gates: {},
  scenarios: [],
  ...patch,
});

const same = { name: 'same', outcome: 'same', inputs_identical: true, before: 'r1', after: 'r2' };
const different = { name: 'k8s-enrichment', outcome: 'different', inputs_identical: true, before: 'r3', after: 'r4' };
const unsure = { name: 'tail-sampling', outcome: 'inconclusive', inputs_identical: true, before: 'r5', after: 'r6' };

const byName = (c: UpgradeCheck) => Object.fromEntries(upgradeFacets(c).map((f) => [f.name, f]));

describe('upgradeFacets', () => {
  it('is all clear for a compatible check that compared every scenario', () => {
    const f = byName(check({ scenarios: [same] }));
    expect(f.Configuration).toMatchObject({ tone: 'pass', value: 'Valid on both', detail: 'same diagnostics on both' });
    expect(f.Behavior).toMatchObject({ tone: 'pass', value: 'Same behavior', detail: '1 scenario' });
    expect(f['Feature gates']).toMatchObject({ tone: 'pass', value: 'No change' });
    expect(f.Certainty).toMatchObject({ tone: 'pass', value: 'Complete' });
  });

  it('counts new diagnostics, differing scenarios and changed gates', () => {
    const f = byName(check({
      verdict: 'behavior_changes',
      configuration: { from_valid: true, to_valid: true, added: [{ severity: 'warning', code: 'deprecated', message: 'old key' }], removed: [] },
      scenarios: [same, different, different],
      feature_gates: { changes: [
        { id: 'pkg.translator.semconv.useNewNames', from: { stage: 'beta', enabled: false }, to: { stage: 'beta', enabled: true } },
        { id: 'other', from: undefined, to: { stage: 'alpha', enabled: false } },
      ] },
    }));
    expect(f.Configuration.detail).toBe('1 new diagnostic on the target');
    expect(f.Behavior).toMatchObject({ tone: 'fail', value: '2 different', detail: 'of 3 scenarios' });
    expect(f['Feature gates']).toMatchObject({ tone: 'warn', value: '2 changed', detail: 'pkg.translator.semconv.useNewNames and 1 more' });
  });

  it.each([
    [{ from_valid: true, to_valid: false }, 'fail', 'Invalid on the target'],
    [{ from_valid: false, to_valid: true }, 'warn', 'Invalid on the baseline'],
    [{ from_valid: false, to_valid: false }, 'fail', 'Invalid on both'],
  ])('words an invalid configuration %j', (cfg, tone, value) => {
    expect(byName(check({ configuration: cfg })).Configuration).toMatchObject({ tone, value });
  });

  it('never calls an inconclusive check complete', () => {
    const f = byName(check({ verdict: 'inconclusive', scenarios: [unsure], reasons: [{ code: 'timeout', message: 'decision_wait reached run limit' }] }));
    expect(f.Behavior).toMatchObject({ tone: 'warn', value: 'Inconclusive' });
    expect(f.Certainty).toMatchObject({ tone: 'warn', value: 'Partial', detail: 'decision_wait reached run limit' });
    expect(byName(check({ verdict: 'inconclusive' })).Certainty.detail).toBe('the check could not decide');
  });

  it('says what was not compared', () => {
    const f = byName(check({ feature_gates: { unknown: ['baseline'] } }));
    expect(f.Behavior).toMatchObject({ tone: 'neutral', value: 'Not compared' });
    expect(f['Feature gates']).toMatchObject({ tone: 'neutral', value: 'Not compared', detail: 'the baseline runtime lists no gates' });
  });
});

describe('summaryOf', () => {
  it.each([
    ['compatible', 'pass', 'Compatible'],
    ['configuration_breaks', 'fail', 'Configuration breaks'],
    ['behavior_changes', 'fail', 'Behavior changes'],
    ['inconclusive', 'warn', 'Inconclusive'],
  ])('states the %s verdict', (verdict, tone, title) => {
    const s = summaryOf(check({ verdict }));
    expect(s).toMatchObject({ tone, title });
    expect(s.body.endsWith('.')).toBe(true);
  });

  it('keeps an unknown verdict as a warning', () => {
    expect(summaryOf(check({ verdict: 'surprising' }))).toEqual({ tone: 'warn', title: 'surprising', body: '' });
  });
});

describe('diffRows', () => {
  it('lists gates, diagnostics both ways and the reasons for uncertainty', () => {
    const rows = diffRows(check({
      configuration: {
        from_valid: true, to_valid: true,
        added: [{ severity: 'warning', code: 'deprecated', component: 'processor/batch', message: 'send_batch_max_size will be required' }],
        removed: [{ severity: 'warning', code: 'old', message: 'gone' }],
      },
      feature_gates: { changes: [{ id: 'g', component: 'processors/transform', from: { stage: 'beta', enabled: false }, to: { stage: 'beta', enabled: true } }] },
      reasons: [{ code: 'timeout', message: 'decision_wait reached run limit' }],
    }));
    expect(rows).toEqual([
      { kind: 'changed', path: 'g', before: 'beta · off', after: 'beta · on', by: 'feature gate · processors/transform' },
      { kind: 'added', path: 'deprecated · processor/batch', before: '', after: 'send_batch_max_size will be required', by: 'warning · new on the target' },
      { kind: 'removed', path: 'old', before: 'gone', after: '', by: 'warning · no longer reported' },
      { kind: 'changed', path: 'timeout', before: '', after: 'decision_wait reached run limit', by: 'uncertainty' },
    ]);
  });
});

describe('compare links', () => {
  it('opens a scenario\'s two runs in Compare runs', () => {
    expect(compareSearch(['a', 'b'])).toEqual({ run: 'a', compare: 'b', tab: 'compare' });
    expect(compareSearch(['a'])).toBeUndefined();
    expect(compareSearch([])).toBeUndefined();
  });

  it('prefers a different scenario over an inconclusive one, and has none when all are the same', () => {
    expect(firstDifference(check({ scenarios: [unsure, same, different] }))).toEqual({
      name: 'k8s-enrichment', search: { run: 'r3', compare: 'r4', tab: 'compare' },
    });
    expect(firstDifference(check({ scenarios: [same, unsure] }))?.name).toBe('tail-sampling');
    expect(firstDifference(check({ scenarios: [same] }))).toBeUndefined();
    expect(firstDifference(check({ scenarios: [{ ...different, before: undefined, after: undefined }] }))).toBeUndefined();
  });
});

describe('checkedLine', () => {
  const subject = { configLabel: 'checkout-logs.yaml', scenarios: 3, from: '', to: 'x', startedAt: 0 };
  it('names the draft and its scenarios', () => {
    expect(checkedLine(subject)).toEqual({ config: 'checkout-logs.yaml', scenarios: 'with 3 scenarios' });
    expect(checkedLine({ ...subject, configLabel: '', scenarios: 1 })).toEqual({ config: 'the open configuration', scenarios: 'with 1 scenario' });
    expect(checkedLine({ ...subject, scenarios: 0 }).scenarios).toBe('without a scenario');
  });
  it('words a side', () => {
    expect(sideLine({ verified: false })).toBe('unverified release');
  });
});
