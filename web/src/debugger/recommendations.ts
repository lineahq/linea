// Recommendations of a finished run, as the report lists them (the server's
// rules in internal/debugger/recommend). This module only words them: it adds
// no recommendation and drops none, and a change stays a suggestion the user
// applies to their own configuration.
import type { Recommendation, RecommendationChange, RecommendationVerification, Report } from '../api/types';
import { nodeKey } from '../graph/layout';
import type { Segment } from './findings';
import { elapsedText } from './runFacts';

const code = (s: string): Segment => ({ code: s });

/** Short titles of the known rules; an unknown rule shows its ID (`ruleTitle`). */
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

/** ruleTitle is the short title of a rule; an unknown rule shows its ID. */
export function ruleTitle(rule: string): string {
  return TITLES[rule] ?? rule;
}

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

/** What the change's total is, per metric: the thing `benefit.of` counts. */
const TOTALS: Record<string, string> = {
  evaluations_without_effect: 'evaluations of its statements or conditions',
  records_without_effect: "record passes through the pipeline's processors",
  records_not_processed: "record passes through the pipeline's processors",
  resource_blocks_merged: 'resource blocks the batch processor sent',
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
  /** effect says what the change does to the exported data; old reports give none. */
  effect?: Effect;
  /** share puts the saving against its total in this run. */
  share?: Share;
  blockers: string[];
  coverage: string[];
  actions: RecommendationAction[];
}

export interface Effect {
  text: string;
  tone: 'pass' | 'warn' | 'neutral';
}

export interface Share {
  text: string;
  /** fraction is the saving over its total, from 0 to 1. */
  fraction: number;
}

const count = (n: number) => n.toLocaleString('en-US');

/**
 * effect words what applying a recommendation does to the exported data: the
 * rule's declared output, settled by a verification when there is one.
 */
export function effect(output: string | undefined, confidence: string): Effect | undefined {
  switch (output) {
    case 'unchanged':
      if (confidence === 'verified_on_input') return { text: 'Exported data stays the same: verified on this input.', tone: 'pass' };
      if (confidence === 'differs_on_input') return { text: 'Meant to keep the exported data the same, but it changed on this input.', tone: 'warn' };
      if (confidence === 'static') return { text: 'Exported data stays the same.', tone: 'neutral' };
      return { text: "Exported data stays the same on this run's input.", tone: 'neutral' };
    case 'regrouped':
      if (confidence === 'differs_on_input') return { text: 'Meant to only regroup records, but the output changed on this input.', tone: 'warn' };
      return { text: 'Same records, sent in other batches or groups.', tone: 'neutral' };
    case 'changed':
      return { text: 'Changes what the exporters receive.', tone: 'warn' };
  }
  return undefined;
}

/** percent words a fraction for a share: one decimal below 10%, never a false 0%. */
export function percent(fraction: number): string {
  const p = fraction * 100;
  if (p > 0 && p < 0.1) return '<0.1%';
  if (p < 10) return `${Number(p.toFixed(1))}%`;
  return `${Math.round(p)}%`;
}

/** share puts a benefit against its total; a benefit without a total has none. */
export function share(metric: string, n: number, of: number | undefined): Share | undefined {
  if (!of || of <= 0) return undefined;
  const fraction = Math.min(1, Math.max(0, n / of));
  const total = TOTALS[metric] ?? metric.replaceAll('_', ' ');
  return { text: `${percent(fraction)} of the ${count(of)} ${total}`, fraction };
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

/**
 * verifiable mirrors the server's check: a processor swap, a removed
 * statement or condition the run located, or an option with a value. The
 * server may still refuse one against the run's configuration.
 */
export function verifiable(c: RecommendationChange | undefined): boolean {
  switch (c?.op) {
    case 'swap':
      return (c.processors?.length ?? 0) > 0;
    case 'remove_statement':
      return !!c.path && !!c.text;
    case 'set_option':
      return !!c.option && !!c.value;
  }
  return false;
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
export function benefitText(metric: string, n: number): string {
  const words = BENEFITS[metric];
  if (!words) return `${count(n)} ${metric.replaceAll('_', ' ')}`;
  return `${count(n)} ${n === 1 ? words[0] : words[1]} in this run`;
}

/**
 * benefitChangeText words how what a change would save moved between two runs
 * ("12 → 3 evaluations had no effect"). Counts of two different metrics are not
 * comparable, so they give no text.
 */
export function benefitChangeText(before: { metric: string; count: number }, after: { metric: string; count: number }): string | undefined {
  if (before.metric !== after.metric || before.count === after.count) return undefined;
  const words = BENEFITS[after.metric]?.[1] ?? after.metric.replaceAll('_', ' ');
  return `${before.count} → ${after.count} ${words}`;
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
      verifiable: verifiable(r.change),
      rule: r.rule,
      title: ruleTitle(r.rule),
      kind: KINDS[r.kind] ?? { label: r.kind, tone: 'plain' },
      provenance: provenance(r.confidence, runName),
      pipeline: r.pipeline,
      summary: r.summary,
      change: r.change ? changeText(r.change, r.pipeline) : undefined,
      saves: r.benefit ? benefitText(r.benefit.metric, r.benefit.count) : undefined,
      effect: effect(r.output, r.confidence),
      share: r.benefit ? share(r.benefit.metric, r.benefit.count, r.benefit.of) : undefined,
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

/** RecommendationKind is one rule's line in the Overview card: its title, kind and how often the run reports it. */
export interface RecommendationKind {
  rule: string;
  title: string;
  kind: RecommendationView['kind'];
  count: number;
}

/**
 * recommendationKinds groups a run's recommendations by rule for the
 * Overview card: data at risk first, then in report order.
 */
export function recommendationKinds(views: RecommendationView[]): RecommendationKind[] {
  const byRule = new Map<string, RecommendationKind>();
  for (const v of views) {
    const k = byRule.get(v.rule);
    if (k) k.count++;
    else byRule.set(v.rule, { rule: v.rule, title: v.title, kind: v.kind, count: 1 });
  }
  const all = [...byRule.values()];
  return [...all.filter((k) => k.kind.tone === 'warn'), ...all.filter((k) => k.kind.tone !== 'warn')];
}

/** VerifyProgress is what a pending verification shows: a sentence and, with an estimate, how far along it probably is. */
export interface VerifyProgress { text: string; fraction?: number }


/**
 * verifyProgress words a verification that is still running. The server
 * answers only at the end, so the estimate is the original run's own time:
 * the candidate runs the same input. The bar never claims to be done.
 */
export function verifyProgress(elapsedMs: number, expectedMs?: number): VerifyProgress {
  const lead = "Running the change on this run's input";
  if (!expectedMs || expectedMs < 1000) return { text: `${lead}: ${elapsedText(elapsedMs)}` };
  if (elapsedMs <= expectedMs) {
    return { text: `${lead}: ${elapsedText(elapsedMs)} of about ${elapsedText(expectedMs)}, the original run's time`, fraction: Math.min(elapsedMs / expectedMs, 0.95) };
  }
  return { text: `${lead}: ${elapsedText(elapsedMs)}, longer than the original run's ${elapsedText(expectedMs)}`, fraction: 0.95 };
}
