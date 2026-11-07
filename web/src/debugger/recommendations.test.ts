import { describe, expect, it } from 'vitest';
import type { Report } from '../api/types';
import { benefitText, changeText, componentNode, provenance, recommendationViews } from './recommendations';

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
      { rule: 'config.unused_component', kind: 'redundancy', confidence: 'static', components: ['processor/attributes'], summary: 'attributes is configured but no pipeline uses it.' },
      {
        rule: 'ottl.statement_never_matched', kind: 'redundancy', confidence: 'observed', components: ['processor/transform/route'],
        summary: 'Statement 1 of group 0 of transform/route never matched.',
        change: { op: 'remove_statement', component: 'transform/route', group: 0, index: 0, text: 'set(attributes["route"], "pager") where severity_text == "FATAL"' },
        benefit: { metric: 'evaluations_without_effect', count: 6 },
        coverage: ['Observed on 2 requests; another input may differ.'],
      },
      { rule: 'sampling.late_spans', kind: 'risk', confidence: 'observed', pipeline: 'logs/pager', components: ['processor/transform/route'], summary: 'Late.', change: { op: 'set_option', component: 'tail_sampling', option: 'decision_wait' } },
    ]), 'abc');
    expect(views.map((v) => v.title)).toEqual(['Unused component', 'Statement never matched', 'Late spans skipped evaluation']);
    expect(views[0]).toMatchObject({ provenance: { text: 'From the configuration' }, actions: [], change: undefined, saves: undefined });
    expect(views[1]).toMatchObject({
      kind: { label: 'Redundant', tone: 'plain' },
      provenance: { text: 'Observed in run abc', tone: 'observed' },
      saves: '6 evaluations had no effect in this run',
      coverage: ['Observed on 2 requests; another input may differ.'],
      actions: [{ label: 'Open transform/route', node: 'logs/archive|processor/transform/route' }],
    });
    expect(views[2].kind).toEqual({ label: 'Risk', tone: 'warn' });
    expect(views[2].actions[0].node).toBe('logs/pager|processor/transform/route');
  });

  it('is empty without recommendations and shows an unknown rule by its ID', () => {
    expect(recommendationViews(undefined, 'abc')).toEqual([]);
    expect(recommendationViews(report(undefined), 'abc')).toEqual([]);
    const [v] = recommendationViews(report([{ rule: 'future.rule', kind: 'novel', confidence: 'observed', components: [], summary: 's' }]), 'abc');
    expect(v).toMatchObject({ title: 'future.rule', kind: { label: 'novel', tone: 'plain' } });
  });
});

describe('changeText', () => {
  const text = (...a: Parameters<typeof changeText>) => changeText(...a).map((s) => (typeof s === 'string' ? s : `\`${s.code}\``)).join('');

  it('words each operation', () => {
    expect(text({ op: 'swap', processors: ['memory_limiter', 'batch'] }, 'logs')).toBe('Processors of `logs`: `[memory_limiter, batch]`');
    expect(text({ op: 'remove_statement', component: 'filter/health', index: 1, text: 'x' })).toBe('Remove condition 1 of `filter/health`: `x`');
    expect(text({ op: 'remove_statement', component: 't', group: 0, index: 2, text: 'y' })).toBe('Remove statement 2 of group 0 of `t`: `y`');
    expect(text({ op: 'set_option', component: 'tail_sampling', option: 'decision_wait' })).toBe('Set `decision_wait` of `tail_sampling`; the value is yours to choose');
    expect(text({ op: 'set_option', component: 'batch', option: 'timeout', value: '5s' })).toBe('Set `timeout` of `batch` to `5s`');
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

describe('componentNode', () => {
  it('uses the pipeline that holds the component, and no node for an unused one', () => {
    const r = report([]);
    expect(componentNode(r, 'processor/batch', 'logs/pager')).toBe('logs/archive|processor/batch');
    expect(componentNode(r, 'exporter/debug')).toBe('exporter/debug');
    expect(componentNode(r, 'processor/attributes')).toBeUndefined();
  });
});
