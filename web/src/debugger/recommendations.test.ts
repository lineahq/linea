import { describe, expect, it } from 'vitest';
import type { RecommendationVerification, Report } from '../api/types';
import { benefitChangeText, benefitText, changeText, changeTitle, componentNode, effect, holdsOf, measuredViews, moves, percent, provenance, recommendationGroups, recommendationHint, recommendationKindGroups, recommendationKinds, recommendationViews, relative, ruleTitle, savesText, share, totalText, verifiable, verificationView, verifyProgress } from './recommendations';

const comp = (kind: string, id: string) => ({ kind, id, type: id.split('/')[0], ref: `${kind}/${id}`, mode: 'real' });

/** transform/route is shared by two pipelines; attributes is unused. */
function report(recommendations: Report['recommendations']): Report {
  return {
    report_schema: '0.15',
    topology: {
      components: [comp('receiver', 'otlp'), comp('processor', 'batch'), comp('processor', 'transform/route'), comp('processor', 'attributes'), comp('exporter', 'debug')],
      pipelines: [
        { id: 'logs/archive', signal: 'logs', receivers: ['receiver/otlp'], processors: ['processor/transform/route', 'processor/batch'], exporters: ['exporter/debug'] },
        { id: 'logs/pager', signal: 'logs', receivers: ['receiver/otlp'], processors: ['processor/transform/route'], exporters: ['exporter/debug'] },
      ],
    },
    recommendations,
  } as Report;
}

describe('recommendationViews', () => {
  it('words each recommendation in report order and links the components it names', () => {
    const views = recommendationViews(report([
      { rule: 'config.unused_component', kind: 'redundancy', confidence: 'static', holds: 'any_input', components: ['processor/attributes'], summary: 'attributes is configured but no pipeline uses it.' },
      {
        rule: 'ottl.statement_never_matched', kind: 'redundancy', confidence: 'observed', holds: 'this_input', components: ['processor/transform/route'],
        summary: 'Statement 1 of group 0 of transform/route never matched.',
        change: { op: 'remove_statement', component: 'transform/route', group: 0, index: 0, text: 'set(attributes["route"], "pager") where severity_text == "FATAL"' },
        benefit: { metric: 'evaluations_without_effect', count: 6 },
        coverage: ['Observed on 2 requests; another input may differ.'],
      },
      { rule: 'sampling.late_spans', kind: 'risk', confidence: 'observed', holds: 'this_input', pipeline: 'logs/pager', components: ['processor/transform/route'], summary: 'Late.', change: { op: 'set_option', component: 'tail_sampling', option: 'decision_wait' } },
    ]), 'abc');
    expect(views.map((v) => v.title)).toEqual(['Unused component', 'Statement not tested', 'Late spans skipped evaluation']);
    expect(views[0]).toMatchObject({ provenance: { text: 'From the configuration' }, actions: [], change: undefined, saves: undefined });
    expect(views[1]).toMatchObject({
      kind: { label: 'Redundant', tone: 'plain' },
      provenance: { text: 'Observed in run abc', tone: 'observed' },
      saves: 'Saves 6 statement evaluations',
      change: ['Remove ', { code: 'set(attributes["route"], "pager") where severity_text == "FATAL"' }],
      changeTitle: 'Remove statement 0 of group 0 of transform/route',
      coverage: ['Observed on 2 requests; another input may differ.'],
      actions: [{ label: 'Open transform/route', node: 'logs/archive|processor/transform/route' }],
    });
    expect(views[2].kind).toEqual({ label: 'Risk', tone: 'warn' });
    expect(views[2].actions[0].node).toBe('logs/pager|processor/transform/route');
  });

  it('is empty without recommendations and shows an unknown rule by its ID', () => {
    expect(recommendationViews(undefined, 'abc')).toEqual([]);
    expect(recommendationViews(report(undefined), 'abc')).toEqual([]);
    const [v] = recommendationViews(report([{ rule: 'future.rule', kind: 'novel', confidence: 'observed', holds: 'this_input', components: [], summary: 's' }]), 'abc');
    expect(v).toMatchObject({ title: 'future.rule', kind: { label: 'novel', tone: 'plain' } });
  });
});

describe('changeText', () => {
  const text = (...a: Parameters<typeof changeText>) => changeText(...a).map((s) => (typeof s === 'string' ? s : `\`${s.code}\``)).join('');

  it('words each operation', () => {
    expect(text({ op: 'swap', processors: ['memory_limiter', 'batch'] }, 'logs')).toBe('Processors of `logs`: `[memory_limiter, batch]`');
    expect(text({ op: 'remove_statement', component: 'filter/health', index: 1, text: 'x' })).toBe('Remove `x`');
    expect(changeTitle({ op: 'remove_statement', component: 'filter/health', index: 1, text: 'x' })).toBe('Remove condition 1 of filter/health');
    expect(changeTitle({ op: 'remove_statement', component: 't', group: 0, index: 2, text: 'y' })).toBe('Remove statement 2 of group 0 of t');
    expect(text({ op: 'set_option', component: 'tail_sampling', option: 'decision_wait' })).toBe('Set `decision_wait` of `tail_sampling`; the value is yours to choose');
    expect(text({ op: 'set_option', component: 'batch', option: 'timeout', value: '5s' })).toBe('Set `timeout` of `batch` to `5s`');
  });

  it('words a processor order as the moves from the current one', () => {
    const now = ['enrich', 'batch', 'tenant', 'filter', 'memory_limiter'];
    const swap = (processors: string[]) => ({ op: 'swap' as const, processors });
    expect(text(swap(['memory_limiter', 'enrich', 'batch', 'tenant', 'filter']), 'logs', now)).toBe('Move `memory_limiter` first');
    expect(text(swap(['enrich', 'filter', 'batch', 'tenant', 'memory_limiter']), 'logs', now)).toBe('Move `filter` before `batch`');
    expect(text(swap(['memory_limiter', 'enrich', 'filter', 'batch', 'tenant']), 'logs', now)).toBe('Move `memory_limiter` first; move `filter` before `batch`');
    expect(text(swap(['batch', 'enrich', 'tenant', 'filter', 'memory_limiter']), 'logs', now)).toBe('Move `enrich` before `tenant`');
    expect(text(swap(['enrich', 'batch', 'tenant', 'filter', 'memory_limiter', 'x']), 'logs', now)).toBe('Processors of `logs`: `[enrich, batch, tenant, filter, memory_limiter, x]`');
    expect(text(swap(['memory_limiter', 'filter', 'tenant', 'batch', 'enrich']), 'logs', now)).toMatch(/^Processors of/);
    expect(changeTitle(swap(['a', 'b']))).toBe('New order: a, b');
    expect(moves(['a', 'b'], ['a', 'c'])).toBeUndefined();
    expect(moves(['a', 'b'], ['a', 'b'])).toEqual([]);
  });
});

describe('benefitText and provenance', () => {
  it('counts in words and says what the claim rests on', () => {
    expect(benefitText('records_without_effect', 1)).toBe('1 record passed through unchanged in this run');
    expect(benefitText('new_metric', 3)).toBe('3 new metric');
    expect(provenance('verified_on_input', 'abc')).toEqual({ text: 'Verified on the input of run abc', tone: 'verified' });
    expect(provenance('differs_on_input', 'abc').tone).toBe('preview');
  });
});

describe('ruleTitle and benefitChangeText', () => {
  it('titles a known rule and shows an unknown one by its ID', () => {
    expect(ruleTitle('ottl.statement_never_matched')).toBe('Statement not tested');
    expect(ruleTitle('future.rule')).toBe('future.rule');
  });
  it('words a benefit that moved between two runs, and nothing when it did not or the metrics differ', () => {
    const m = (metric: string, count: number) => ({ metric, count });
    expect(benefitChangeText(m('evaluations_without_effect', 12), m('evaluations_without_effect', 3))).toBe('12 → 3 evaluations had no effect');
    expect(benefitChangeText(m('new_metric', 2), m('new_metric', 5))).toBe('2 → 5 new metric');
    expect(benefitChangeText(m('evaluations_without_effect', 3), m('evaluations_without_effect', 3))).toBeUndefined();
    expect(benefitChangeText(m('evaluations_without_effect', 3), m('records_without_effect', 1))).toBeUndefined();
  });
});

describe('componentNode', () => {
  it('uses the pipeline that holds the component, and no node for an unused one', () => {
    const r = report([]);
    expect(componentNode(r, 'processor/batch', 'logs/pager')).toBe('logs/archive|processor/batch');
    expect(componentNode(r, 'exporter/debug')).toBe('exporter/debug');
    expect(componentNode(r, 'processor/attributes')).toBeUndefined();
  });
});

describe('verification', () => {
  it('marks swaps, located removals and options with a value verifiable, by their report position', () => {
    const views = recommendationViews(report([
      { rule: 'config.unused_component', kind: 'redundancy', confidence: 'static', holds: 'any_input', summary: 'Unused.' },
      { rule: 'filter.after_batch', kind: 'ordering', confidence: 'observed', holds: 'this_input', pipeline: 'logs/archive', summary: 'Late.', change: { op: 'swap', processors: ['transform/route', 'batch'] } },
      { rule: 'sampling.late_spans', kind: 'risk', confidence: 'observed', holds: 'this_input', summary: 'Late.', change: { op: 'set_option', component: 'tail_sampling', option: 'decision_wait' } },
    ]), 'abc');
    expect(views.map((v) => [v.index, v.verifiable])).toEqual([[0, false], [1, true], [2, false]]);
    expect(verifiable({ op: 'remove_statement', component: 'transform/route', group: 0, index: 1, text: 'set(x, 1)', path: 'log_statements[0].statements[1]' })).toBe(true);
    expect(verifiable({ op: 'remove_statement', component: 'filter/drop', index: 0, text: 'x' })).toBe(false);
    expect(verifiable({ op: 'set_option', component: 'batch', option: 'metadata_keys', value: '["tenant"]' })).toBe(true);
    expect(verifiable({ op: 'swap', processors: [] })).toBe(false);
    expect(verifiable(undefined)).toBe(false);
  });

  const verification = (v: Partial<RecommendationVerification>) =>
    ({ run: 'r1', recommendation: 1, rule: 'filter.after_batch', candidate: { id: 'c1' }, ...v }) as RecommendationVerification;

  it('words what the server comparison found and keeps its reasons', () => {
    expect(verificationView(verification({ confidence: 'verified_on_input' }), 'abc')).toMatchObject({
      provenance: { text: 'Verified on the input of run abc', tone: 'verified' }, tone: 'pass', candidate: 'c1',
    });
    expect(verificationView(verification({ confidence: 'differs_on_input' }), 'abc')).toMatchObject({
      provenance: { text: 'Changes the output on the input of run abc', tone: 'preview' }, tone: 'warn',
    });
    const inconclusive = verificationView(verification({ comparison: { reasons: [{ code: 'x', message: 'Inputs differ.' }] } as RecommendationVerification['comparison'] }), 'abc');
    expect(inconclusive.provenance).toBeUndefined();
    expect(inconclusive).toMatchObject({ tone: 'neutral', reasons: ['Inputs differ.'] });
    expect(verificationView(verification({ error: 'boom' }), 'abc')).toMatchObject({ tone: 'fail', text: 'The candidate run failed: boom' });
  });
});

describe('verification assertions', () => {
  const verification = (v: Partial<RecommendationVerification>) =>
    ({ run: 'r1', recommendation: 0, rule: 'filter.after_batch', candidate: { id: 'c1' }, confidence: 'differs_on_input', holds: 'this_input', ...v }) as RecommendationVerification;

  it('names the assertions a different output broke', () => {
    const v = verificationView(verification({ comparison: { assertions: [
      { name: 'debug logs are dropped', before: 'pass', after: 'fail', changed: true },
      { name: 'unchanged', before: 'pass', after: 'pass', changed: false },
    ] } as RecommendationVerification['comparison'] }), 'abc');
    expect(v).toMatchObject({
      tone: 'fail',
      text: 'With the change, this input gives a different output and 1 assertion that passed no longer passes.',
      assertions: ['debug logs are dropped: pass in this run, fail with the change'],
    });
  });

  it('keeps the plain wording when no assertion changed', () => {
    expect(verificationView(verification({}), 'abc')).toMatchObject({ tone: 'warn', assertions: [] });
  });
});

describe('recommendationHint', () => {
  it('counts the recommendations and those about data at risk', () => {
    const views = recommendationViews(report([
      { rule: 'config.unused_component', kind: 'redundancy', confidence: 'static', holds: 'any_input', summary: 'Unused.' },
      { rule: 'sampling.late_spans', kind: 'risk', confidence: 'observed', holds: 'this_input', summary: 'Late.' },
    ]), 'abc');
    expect(recommendationHint(views)).toBe('2 recommendations, 1 about data at risk');
    expect(recommendationHint(views.slice(0, 1))).toBe('1 recommendation');
    expect(recommendationHint([])).toBeUndefined();
  });
});

describe('recommendationKinds', () => {
  it('groups by rule, data at risk first, then in report order', () => {
    const views = recommendationViews(report([
      { rule: 'config.unused_component', kind: 'redundancy', confidence: 'static', holds: 'any_input', summary: 'Unused.' },
      { rule: 'ottl.statement_never_matched', kind: 'redundancy', confidence: 'observed', holds: 'this_input', summary: 'A.' },
      { rule: 'sampling.late_spans', kind: 'risk', confidence: 'observed', holds: 'this_input', summary: 'Late.' },
      { rule: 'ottl.statement_never_matched', kind: 'redundancy', confidence: 'observed', holds: 'this_input', summary: 'B.' },
    ]), 'abc');
    expect(recommendationKinds(views).map((k) => [k.title, k.kind.label, k.count])).toEqual([
      ['Late spans skipped evaluation', 'Risk', 1],
      ['Unused component', 'Redundant', 1],
      ['Statement not tested', 'Redundant', 2],
    ]);
    expect(recommendationKinds([])).toEqual([]);
  });

  it('lists the kinds per group, in the order of the tab', () => {
    const views = recommendationViews(report([
      { rule: 'ottl.statement_never_matched', kind: 'untested', confidence: 'observed', holds: 'this_input', summary: 'A.' },
      { rule: 'sampling.late_spans', kind: 'risk', confidence: 'observed', holds: 'this_input', summary: 'Late.' },
      { rule: 'config.unused_component', kind: 'redundancy', confidence: 'static', holds: 'any_input', summary: 'Unused.' },
      { rule: 'ottl.statement_never_matched', kind: 'untested', confidence: 'observed', holds: 'this_input', summary: 'B.' },
    ]), 'abc');
    expect(recommendationKindGroups(views).map((g) => [g.title, g.kinds.map((k) => [k.title, k.count])])).toEqual([
      ['Holds for any input', [['Unused component', 1]]],
      ['Depends on this input', [['Late spans skipped evaluation', 1]]],
      ['Not tested by this input', [['Statement not tested', 2]]],
    ]);
  });
});

describe('verifyProgress', () => {
  it('estimates from the original run and never claims to be done', () => {
    expect(verifyProgress(4_000, 10_000)).toEqual({ text: "Running the change on this run's input: 4 s of about 10 s, the original run's time", fraction: 0.4 });
    expect(verifyProgress(9_900, 10_000).fraction).toBe(0.95);
    expect(verifyProgress(75_000, 10_000)).toEqual({ text: "Running the change on this run's input: 1 min 15 s, longer than the original run's 10 s", fraction: 0.95 });
  });
  it('shows only the elapsed time without an estimate', () => {
    expect(verifyProgress(3_000)).toEqual({ text: "Running the change on this run's input: 3 s" });
    expect(verifyProgress(3_000, 400)).toEqual({ text: "Running the change on this run's input: 3 s" });
  });
});

describe('effect and share', () => {
  it('words what a change does to the exported data, settled by a verification', () => {
    expect(effect('unchanged', 'observed')).toEqual({ label: 'Same output', text: "Exported data stays the same on this run's input.", tone: 'neutral' });
    expect(effect('unchanged', 'static')?.text).toBe('Exported data stays the same.');
    expect(effect('unchanged', 'verified_on_input')).toMatchObject({ label: 'Same output, verified', tone: 'pass' });
    expect(effect('unchanged', 'differs_on_input')?.tone).toBe('warn');
    expect(effect('regrouped', 'observed')).toEqual({ label: 'Regroups only', text: 'Same records, sent in other batches or groups.', tone: 'neutral' });
    expect(effect('regrouped', 'differs_on_input')?.tone).toBe('warn');
    expect(effect('changed', 'verified_on_input')).toEqual({ label: 'Changes output', text: 'Changes what the exporters receive.', tone: 'warn' });
    expect(effect(undefined, 'observed')).toBeUndefined();
  });

  it('puts a saving against its total, and gives none without a total', () => {
    expect(share(2217, 160000, 'x')).toEqual({ text: '1.4% of 160,000', title: '1.4% of the 160,000 x', fraction: 2217 / 160000 });
    expect(share(3, undefined, 'x')).toBeUndefined();
    expect(share(3, 0, 'x')).toBeUndefined();
    expect(share(9, 4, 'x')?.fraction).toBe(1);
  });

  it('names whose total it is', () => {
    const filter = { rule: 'filter.condition_never_matched', pipeline: 'logs', components: ['processor/filter/debug'] };
    expect(totalText(filter, 'evaluations_without_effect')).toBe('condition evaluations of filter/debug');
    expect(totalText({ ...filter, rule: 'ottl.statement_no_change', components: ['processor/transform/enrich'] }, 'evaluations_without_effect')).toBe('statement evaluations of transform/enrich');
    expect(totalText(filter, 'records_without_effect')).toBe('record passes through the processors of logs');
    expect(totalText({ rule: 'grouping.after_batch', components: ['processor/batch'] }, 'resource_blocks_merged')).toBe('resource blocks batch sent');
    expect(totalText({ rule: 'x' }, 'new_metric')).toBe('new metric');
  });

  it('words what a card saves, short', () => {
    const filter = { rule: 'filter.condition_never_matched' };
    expect(savesText(filter, 'evaluations_without_effect', 4423)).toBe('Saves 4,423 condition evaluations');
    expect(savesText({ rule: 'processor.no_effect' }, 'evaluations_without_effect', 1)).toBe('Saves 1 evaluation');
    expect(savesText(filter, 'records_not_processed', 577)).toBe('577 dropped records skip earlier processors');
    expect(savesText(filter, 'resource_blocks_merged', 2)).toBe('Merges 2 resource blocks');
    expect(savesText(filter, 'new_metric', 3)).toBe('3 new metric');
  });

  it('words a percentage without a false zero', () => {
    expect(percent(0)).toBe('0%');
    expect(percent(0.0004)).toBe('<0.1%');
    expect(percent(0.05)).toBe('5%');
    expect(percent(0.0567)).toBe('5.7%');
    expect(percent(0.456)).toBe('46%');
  });

  it('adds both to the card view, and formats large counts', () => {
    const report = { recommendations: [{ rule: 'processor.no_effect', kind: 'redundancy', confidence: 'observed', holds: 'this_input', summary: 's', output: 'unchanged', benefit: { metric: 'records_without_effect', count: 20000, of: 160000 } }] } as unknown as Report;
    const [v] = recommendationViews(report, 'r1');
    expect(v.saves).toBe('Saves 20,000 record passes');
    expect(v.share).toMatchObject({ text: '13% of 160,000', title: '13% of the 160,000 record passes through the processors' });
    expect(v.effect?.tone).toBe('neutral');
  });
});

describe('measured impact of a verification', () => {
  const verification = (v: Partial<RecommendationVerification>) =>
    ({ run: 'r1', recommendation: 0, rule: 'ottl.statement_never_matched', candidate: { id: 'c1' }, confidence: 'verified_on_input', holds: 'this_input', ...v }) as RecommendationVerification;

  it('words the counts the change moved, and always the records the exporters received', () => {
    const v = verification({
      measured: [
        { metric: 'evaluations', subject: 'processor/transform/enrich', before: 25000, after: 20000 },
        { metric: 'record_passes', subject: 'logs', before: 29423, after: 29423 },
        { metric: 'batch_sends', subject: 'logs', before: 10, after: 5 },
      ],
      comparison: { exporters: [{ exporter: 'otlp/a', before: 4000, after: 4000 }, { exporter: 'otlp/b', before: 423, after: 400 }] } as unknown as RecommendationVerification['comparison'],
    });
    expect(measuredViews(v)).toEqual([
      { label: 'Evaluations of transform/enrich', before: '25,000', after: '20,000', change: '−20%', tone: 'pass' },
      { label: 'Sends by the batch processors of logs', before: '10', after: '5', change: '−50%', tone: 'pass' },
      { label: 'Records the exporters received', before: '4,423', after: '4,400', change: '−0.5%', tone: 'warn' },
    ]);
    expect(verificationView(v, 'abc').measured).toHaveLength(3);
    expect(verificationView(verification({ error: 'boom', measured: v.measured }), 'abc').measured).toEqual([]);
  });

  it('words a relative change', () => {
    expect(relative(10, 10)).toBe('');
    expect(relative(0, 3)).toBe('new');
    expect(relative(200, 300)).toBe('+50%');
    expect(relative(100000, 99999)).toBe('−<0.1%');
  });
});

describe('holds and untested', () => {
  it('reads holds, and falls back to the confidence for a report without it', () => {
    expect(holdsOf({ confidence: 'observed', holds: 'any_input' })).toBe('any_input');
    expect(holdsOf({ confidence: 'static' })).toBe('any_input');
    expect(holdsOf({ confidence: 'observed' })).toBe('this_input');
  });

  it('groups by how far a recommendation holds, untested last, in report order', () => {
    const views = recommendationViews(report([
      { rule: 'ottl.statement_no_change', kind: 'redundancy', confidence: 'observed', holds: 'this_input', components: [], summary: 'a',
        change: { op: 'remove_statement', component: 'transform/route', group: 0, index: 1, path: 'processors::transform/route::log_statements', text: 'set(x, "y")' } },
      { rule: 'ottl.statement_never_matched', kind: 'untested', confidence: 'observed', holds: 'this_input', components: ['processor/transform/route'],
        subject: 'set(x, "y") where z', summary: 'b' },
      { rule: 'ottl.duplicate_statement', kind: 'redundancy', confidence: 'observed', holds: 'any_input', components: [], summary: 'c',
        change: { op: 'remove_statement', component: 'transform/route', group: 0, index: 2, text: 'set(x, "y")' } },
      { rule: 'config.unused_component', kind: 'redundancy', confidence: 'static', holds: 'any_input', components: [], summary: 'd' },
    ] as Report['recommendations']), 'abc');
    expect(views[1]).toMatchObject({ kind: { label: 'Untested' }, untested: { label: 'Statement', text: 'set(x, "y") where z' }, change: undefined, verifiable: false });
    expect(views[0]).toMatchObject({ checksOnly: true, verifiable: true, holds: 'this_input' });
    const groups = recommendationGroups(views);
    expect(groups.map((g) => [g.id, g.views.map((v) => v.summary)])).toEqual([
      ['any_input', ['c', 'd']],
      ['this_input', ['a']],
      ['untested', ['b']],
    ]);
    expect(recommendationGroups(views.slice(0, 1)).map((g) => g.id)).toEqual(['this_input']);
    expect(recommendationHint(views)).toBe('4 recommendations, 1 not tested by this input');
  });
});
