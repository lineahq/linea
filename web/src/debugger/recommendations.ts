// Recommendations of a finished run, as the report lists them (the server's
// rules in internal/debugger/recommend). This module only words them: it adds
// no recommendation and drops none, and a change stays a suggestion the user
// applies to their own configuration.
import type { Recommendation, RecommendationChange, RecommendationVerification, Report } from '../api/types';
import { nodeKey } from '../graph/layout';
import type { Segment } from './findings';

const code = (s: string): Segment => ({ code: s });

/** Short titles of the known rules; an unknown rule shows its ID. */
const TITLES: Record<string, string> = {
  'config.memory_limiter_not_first': 'memory_limiter is not first',
  'config.unused_component': 'Unused component',
  'batch.chained': 'Several batch processors in one pipeline',
  'filter.no_effect': 'Filter dropped nothing',
  'filter.condition_never_matched': 'Filter condition never matched',
  'filter.after_batch': 'Filter runs after batch',
  'filter.move_earlier': 'Filter can run earlier',
  'ottl.statement_never_matched': 'Statement never matched',
  'ottl.statement_no_change': 'Statement changed nothing',
  'ottl.duplicate_statement': 'Repeated statement',
  'attribute.dead_write': 'Attribute overwritten before use',
  'processor.no_effect': 'Processor changed nothing',
  'grouping.no_effect': 'Grouping changed nothing',
  'batch.size_never_reached': 'Batch size never reached',
  'batch.tiny_batchers': 'metadata_keys split batches small',
  'batch.drops_client_context': 'Batch drops the client context',
  'grouping.after_batch': 'Group after batching',
  'sampling.late_spans': 'Late spans skipped evaluation',
  'sampling.policy_never_decides': 'Sampling policy never decided',
  'sampling.policy_shadowed': 'Sampling policy shadowed',
  'sampling.late_in_pipeline': 'Work spent on unsampled traces',
};

const KINDS: Record<string, { label: string; tone: 'warn' | 'plain' }> = {
  ordering: { label: 'Ordering', tone: 'plain' },
  redundancy: { label: 'Redundant', tone: 'plain' },
  inefficiency: { label: 'Inefficient', tone: 'plain' },
  lossy: { label: 'Drops data', tone: 'warn' },
  risk: { label: 'Risk', tone: 'warn' },
};

const BENEFITS: Record<string, [string, string]> = {
  evaluations_without_effect: ['evaluation had no effect', 'evaluations had no effect'],
  records_without_effect: ['record passed through unchanged', 'records passed through unchanged'],
  records_not_processed: ['record would skip the earlier processors', 'records would skip the earlier processors'],
  resource_blocks_merged: ['resource block would merge', 'resource blocks would merge'],
};

export type Provenance = { text: string; tone: 'observed' | 'verified' | 'preview' };

export interface RecommendationAction {
  label: string;
  node: string;
}

export interface RecommendationView {
  id: string;
  /** index is the recommendation's position in the report, as the server names it. */
  index: number;
  /** verifiable: the server can run the change as a candidate (a processor order). */
  verifiable: boolean;
  rule: string;
  title: string;
  kind: { label: string; tone: 'warn' | 'plain' };
  provenance: Provenance;
  pipeline?: string;
  summary: string;
  change?: Segment[];
  saves?: string;
  blockers: string[];
  coverage: string[];
  actions: RecommendationAction[];
}

/** provenance says what a recommendation rests on. */
export function provenance(confidence: string, runName: string): Provenance {
  switch (confidence) {
    case 'static':
      return { text: 'From the configuration', tone: 'observed' };
    case 'verified_on_input':
      return { text: `Verified on the input of run ${runName}`, tone: 'verified' };
    case 'differs_on_input':
      return { text: `Changes the output on the input of run ${runName}`, tone: 'preview' };
  }
  return { text: `Observed in run ${runName}`, tone: 'observed' };
}

/** changeText words a suggested change, as the CLI does. */
export function changeText(c: RecommendationChange, pipeline?: string): Segment[] {
  const at = c.index !== undefined ? ` ${c.index}` : '';
  switch (c.op) {
    case 'swap':
      return [pipeline ? 'Processors of ' : 'Processors: ', ...(pipeline ? [code(pipeline), ': '] : []), code(`[${(c.processors ?? []).join(', ')}]`)];
    case 'remove_statement':
      return c.group !== undefined
        ? [`Remove statement${at} of group ${c.group} of `, code(c.component ?? ''), ': ', code(c.text ?? '')]
        : [`Remove condition${at} of `, code(c.component ?? ''), ': ', code(c.text ?? '')];
    case 'set_option':
      return c.value
        ? ['Set ', code(c.option ?? ''), ' of ', code(c.component ?? ''), ' to ', code(c.value)]
        : ['Set ', code(c.option ?? ''), ' of ', code(c.component ?? ''), '; the value is yours to choose'];
  }
  return [c.op];
}

/** benefitText: what the run shows the change would save. */
export function benefitText(metric: string, count: number): string {
  const words = BENEFITS[metric];
  if (!words) return `${count} ${metric.replaceAll('_', ' ')}`;
  return `${count} ${count === 1 ? words[0] : words[1]} in this run`;
}

/**
 * componentNode is the graph node of a component ref in a pipeline: the
 * recommendation's own, else the first pipeline that holds the component.
 * A component no pipeline uses has no node.
 */
export function componentNode(report: Report, ref: string, pipeline?: string): string | undefined {
  const kind = ref.slice(0, ref.indexOf('/'));
  const pipelines = report.topology?.pipelines ?? [];
  const holds = (p: (typeof pipelines)[number]) =>
    [...(p.receivers ?? []), ...(p.processors ?? []), ...(p.exporters ?? [])].includes(ref);
  const p = pipelines.find((x) => x.id === pipeline && holds(x)) ?? pipelines.find(holds);
  if (!p) return undefined;
  return nodeKey(ref, kind, kind === 'processor' ? p.id : '');
}

/** recommendationViews words a finished run's recommendations in report order. */
export function recommendationViews(report: Report | undefined, runName: string): RecommendationView[] {
  return (report?.recommendations ?? []).map((r: Recommendation, i) => {
    const actions: RecommendationAction[] = [];
    for (const ref of r.components ?? []) {
      const node = componentNode(report!, ref, r.pipeline);
      if (node) actions.push({ label: `Open ${ref.slice(ref.indexOf('/') + 1)}`, node });
    }
    return {
      id: `${i}:${r.rule}`,
      index: i,
      verifiable: r.change?.op === 'swap',
      rule: r.rule,
      title: TITLES[r.rule] ?? r.rule,
      kind: KINDS[r.kind] ?? { label: r.kind, tone: 'plain' },
      provenance: provenance(r.confidence, runName),
      pipeline: r.pipeline,
      summary: r.summary,
      change: r.change ? changeText(r.change, r.pipeline) : undefined,
      saves: r.benefit ? benefitText(r.benefit.metric, r.benefit.count) : undefined,
      blockers: r.blockers ?? [],
      coverage: r.coverage ?? [],
      actions,
    };
  });
}

export interface VerificationView {
  /** provenance replaces the recommendation's own once a verification settles it. */
  provenance?: Provenance;
  /** text says what the candidate run showed. */
  text: string;
  tone: 'pass' | 'warn' | 'fail' | 'neutral';
  reasons: string[];
  /** assertions are the scenario assertions whose outcome the change changed. */
  assertions: string[];
  /** candidate is the candidate run's ID, to compare with the run. */
  candidate: string;
}

/**
 * verificationView words a verification's result. It decides nothing: the
 * confidence and the reasons are the server's comparison.
 */
export function verificationView(v: RecommendationVerification, runName: string): VerificationView {
  const candidate = v.candidate.id;
  const reasons = (v.comparison?.reasons ?? []).map((r) => r.message);
  const assertions = (v.comparison?.assertions ?? []).filter((a) => a.changed)
    .map((a) => `${a.name}: ${a.before ?? 'not evaluated'} in this run, ${a.after ?? 'not evaluated'} with the change`);
  const base = { reasons, assertions, candidate };
  if (v.error) return { ...base, text: `The candidate run failed: ${v.error}`, tone: 'fail' };
  switch (v.confidence) {
    case 'verified_on_input':
      return { ...base, provenance: provenance(v.confidence, runName), text: 'With the change, this input gives the same output.', tone: 'pass' };
    case 'differs_on_input': {
      const broke = (v.comparison?.assertions ?? []).filter((a) => a.changed && a.before === 'pass' && a.after !== 'pass').length;
      const text = broke
        ? `With the change, this input gives a different output and ${broke === 1 ? '1 assertion that passed no longer passes' : `${broke} assertions that passed no longer pass`}.`
        : 'With the change, this input gives a different output. Compare the runs to see what changed.';
      return { ...base, provenance: provenance(v.confidence, runName), text, tone: broke ? 'fail' : 'warn' };
    }
  }
  return { ...base, text: 'The comparison was inconclusive.', tone: 'neutral' };
}

/**
 * recommendationHint is the Overview's one line about a finished run's
 * recommendations; none gives no hint.
 */
export function recommendationHint(views: RecommendationView[]): string | undefined {
  if (!views.length) return undefined;
  const risky = views.filter((v) => v.kind.tone === 'warn').length;
  const count = `${views.length} ${views.length === 1 ? 'recommendation' : 'recommendations'}`;
  return risky ? `${count}, ${risky} about data at risk` : count;
}
