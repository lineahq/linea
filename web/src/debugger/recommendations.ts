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
  'filter.no_effect': 'Filter conditions not tested',
  'filter.condition_never_matched': 'Filter condition not tested',
  'filter.after_batch': 'Filter runs after batch',
  'filter.move_earlier': 'Filter can run earlier',
  'ottl.statement_never_matched': 'Statement not tested',
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
  'sampling.policy_never_decides': 'Sampling policy not tested',
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
  untested: { label: 'Untested', tone: 'plain' },
};

/** Holds is on which inputs a recommendation's claim holds. */
export type Holds = 'any_input' | 'this_input';

/**
 * holdsOf reads a recommendation's `holds`; a report from before the field
 * holds for any input only when the configuration alone gave it.
 */
export function holdsOf(r: Pick<Recommendation, 'confidence'> & { holds?: string }): Holds {
  if (r.holds === 'any_input' || r.holds === 'this_input') return r.holds;
  return r.confidence === 'static' ? 'any_input' : 'this_input';
}

/** untestedLabel names what an untested recommendation's subject is. */
function untestedLabel(rule: string): string {
  if (rule.startsWith('filter.')) return 'Condition';
  if (rule.startsWith('ottl.')) return 'Statement';
  if (rule.startsWith('sampling.')) return 'Policy';
  return 'Subject';
}

const BENEFITS: Record<string, [string, string]> = {
  evaluations_without_effect: ['evaluation had no effect', 'evaluations had no effect'],
  records_without_effect: ['record passed through unchanged', 'records passed through unchanged'],
  records_not_processed: ['record would skip the earlier processors', 'records would skip the earlier processors'],
  resource_blocks_merged: ['resource block would merge', 'resource blocks would merge'],
};

/**
 * totalText names what a recommendation's `benefit.of` counts, with its
 * subject: whose evaluations, which pipeline's processors.
 */
export function totalText(r: Pick<Recommendation, 'rule' | 'pipeline' | 'components'>, metric: string): string {
  const component = r.components?.[0];
  const name = component ? component.slice(component.indexOf('/') + 1) : undefined;
  switch (metric) {
    case 'evaluations_without_effect': {
      const kind = r.rule.startsWith('filter.') ? 'condition evaluations' : r.rule.startsWith('ottl.') ? 'statement evaluations' : 'evaluations';
      return name ? `${kind} of ${name}` : kind;
    }
    case 'records_without_effect':
    case 'records_not_processed':
      return r.pipeline ? `record passes through the processors of ${r.pipeline}` : 'record passes through the processors';
    case 'resource_blocks_merged':
      return name ? `resource blocks ${name} sent` : 'resource blocks the batch processor sent';
  }
  return metric.replaceAll('_', ' ');
}

export type Provenance = { text: string; tone: 'observed' | 'verified' | 'preview' };

export interface RecommendationAction {
  label: string;
  node: string;
}

export interface RecommendationView {
  id: string;
  /** index is the recommendation's position in the report, as the server names it. */
  index: number;
  /** verifiable: the server can run the change as a candidate. */
  verifiable: boolean;
  /**
   * checksOnly: the change removes something that changed nothing on this
   * input, so running it on the same input can only show that it loads and
   * runs, never another output.
   */
  checksOnly: boolean;
  holds: Holds;
  /** untested names what this input left untested: a condition, statement or policy. */
  untested?: { label: string; text: string };
  rule: string;
  title: string;
  kind: { label: string; tone: 'warn' | 'plain' };
  provenance: Provenance;
  pipeline?: string;
  summary: string;
  change?: Segment[];
  /** changeTitle says the change in full where `change` is brief. */
  changeTitle?: string;
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
  /** label is the card's short chip; text says it in full (its tooltip). */
  label: string;
  text: string;
  tone: 'pass' | 'warn' | 'neutral';
}

export interface Share {
  /** text is the short share ("47% of 9,423"); title names the total in full. */
  text: string;
  title: string;
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
      if (confidence === 'verified_on_input') return { label: 'Same output, verified', text: 'Exported data stays the same: verified on this input.', tone: 'pass' };
      if (confidence === 'differs_on_input') return { label: 'Output changed', text: 'Meant to keep the exported data the same, but it changed on this input.', tone: 'warn' };
      if (confidence === 'static') return { label: 'Same output', text: 'Exported data stays the same.', tone: 'neutral' };
      return { label: 'Same output', text: "Exported data stays the same on this run's input.", tone: 'neutral' };
    case 'regrouped':
      if (confidence === 'differs_on_input') return { label: 'Output changed', text: 'Meant to only regroup records, but the output changed on this input.', tone: 'warn' };
      return { label: 'Regroups only', text: 'Same records, sent in other batches or groups.', tone: 'neutral' };
    case 'changed':
      return { label: 'Changes output', text: 'Changes what the exporters receive.', tone: 'warn' };
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

/**
 * share puts a benefit against its total, named by `total` (totalText); a
 * benefit without a total has none.
 */
export function share(n: number, of: number | undefined, total: string): Share | undefined {
  if (!of || of <= 0) return undefined;
  const fraction = Math.min(1, Math.max(0, n / of));
  return { text: `${percent(fraction)} of ${count(of)}`, title: `${percent(fraction)} of the ${count(of)} ${total}`, fraction };
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

/**
 * changeText words a suggested change briefly: a processor order as the
 * moves that make it from the pipeline's current order (`current`, its
 * processor IDs), a removal as the statement or condition itself (the
 * summary names where it is). Where the moves are not clear (no current
 * order, more than two moves) it lists the whole order.
 */
export function changeText(c: RecommendationChange, pipeline?: string, current?: string[]): Segment[] {
  switch (c.op) {
    case 'swap': {
      const next = c.processors ?? [];
      const moved = current ? moves(current, next) : undefined;
      if (moved && moved.length > 0 && moved.length <= 2) {
        return moved.flatMap((m, i) => {
          const at = next.indexOf(m);
          const where = at === 0 ? [' first'] : at === next.length - 1 ? [' last'] : [' before ', code(next[at + 1])];
          return [...(i ? ['; '] : []), i ? 'move ' : 'Move ', code(m), ...where];
        });
      }
      return [pipeline ? 'Processors of ' : 'Processors: ', ...(pipeline ? [code(pipeline), ': '] : []), code(`[${next.join(', ')}]`)];
    }
    case 'remove_statement':
      return ['Remove ', code(c.text ?? '')];
    case 'set_option':
      return c.value
        ? ['Set ', code(c.option ?? ''), ' of ', code(c.component ?? ''), ' to ', code(c.value)]
        : ['Set ', code(c.option ?? ''), ' of ', code(c.component ?? ''), '; the value is yours to choose'];
  }
  return [c.op];
}

/** changeTitle says a change in full, for the brief text's tooltip. */
export function changeTitle(c: RecommendationChange): string | undefined {
  switch (c.op) {
    case 'swap':
      return `New order: ${(c.processors ?? []).join(', ')}`;
    case 'remove_statement':
      return c.group !== undefined
        ? `Remove statement ${c.index ?? ''} of group ${c.group} of ${c.component ?? ''}`
        : `Remove condition ${c.index ?? ''} of ${c.component ?? ''}`;
  }
  return undefined;
}

/**
 * moves names the processors a new order moves: those outside the longest
 * run kept in the same relative order. Undefined when the two orders hold
 * different processors.
 */
export function moves(current: string[], next: string[]): string[] | undefined {
  if (current.length !== next.length || [...current].sort().join() !== [...next].sort().join()) return undefined;
  const n = current.length;
  const lcs = Array.from({ length: n + 1 }, () => new Array<number>(n + 1).fill(0));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = n - 1; j >= 0; j--) {
      lcs[i][j] = current[i] === next[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
    }
  }
  const kept = new Set<string>();
  for (let i = 0, j = 0; i < n && j < n; ) {
    if (current[i] === next[j]) { kept.add(current[i]); i++; j++; }
    else if (lcs[i + 1][j] >= lcs[i][j + 1]) i++;
    else j++;
  }
  return next.filter((x) => !kept.has(x));
}

/**
 * savesText is a card's short line of what the change saves; the share
 * beside it says of how much.
 */
export function savesText(r: Pick<Recommendation, 'rule'>, metric: string, n: number): string {
  switch (metric) {
    case 'evaluations_without_effect': {
      const kind = r.rule.startsWith('filter.') ? 'condition ' : r.rule.startsWith('ottl.') ? 'statement ' : '';
      return `Saves ${count(n)} ${kind}${n === 1 ? 'evaluation' : 'evaluations'}`;
    }
    case 'records_without_effect':
      return `Saves ${count(n)} record ${n === 1 ? 'pass' : 'passes'}`;
    case 'records_not_processed':
      return `${count(n)} dropped ${n === 1 ? 'record skips' : 'records skip'} earlier processors`;
    case 'resource_blocks_merged':
      return `Merges ${count(n)} resource ${n === 1 ? 'block' : 'blocks'}`;
  }
  return `${count(n)} ${metric.replaceAll('_', ' ')}`;
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
    const processors = (report?.topology?.pipelines ?? []).find((p) => p.id === r.pipeline)?.processors;
    const current = processors?.map((ref) => ref.slice(ref.indexOf('/') + 1));
    return {
      id: `${i}:${r.rule}`,
      index: i,
      verifiable: verifiable(r.change),
      checksOnly: r.change?.op === 'remove_statement',
      holds: holdsOf(r),
      untested: r.kind === 'untested' && r.subject ? { label: untestedLabel(r.rule), text: r.subject } : undefined,
      rule: r.rule,
      title: ruleTitle(r.rule),
      kind: KINDS[r.kind] ?? { label: r.kind, tone: 'plain' },
      provenance: provenance(r.confidence, runName),
      pipeline: r.pipeline,
      summary: r.summary,
      change: r.change ? changeText(r.change, r.pipeline, current) : undefined,
      changeTitle: r.change ? changeTitle(r.change) : undefined,
      saves: r.benefit ? savesText(r, r.benefit.metric, r.benefit.count) : undefined,
      effect: effect(r.output, r.confidence),
      share: r.benefit ? share(r.benefit.count, r.benefit.of, totalText(r, r.benefit.metric)) : undefined,
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
  /** measured are the counts the candidate run moved, before and after the change. */
  measured: MeasuredView[];
}

export interface MeasuredView {
  label: string;
  before: string;
  after: string;
  /** change is the relative change ("−20%"); empty when nothing moved. */
  change: string;
  tone: 'pass' | 'warn' | 'neutral';
}

const short = (ref: string) => ref.slice(ref.indexOf('/') + 1);

/** measureLabel names what a measure counts. */
function measureLabel(metric: string, subject: string): string {
  switch (metric) {
    case 'record_passes':
      return `Record passes through the processors of ${subject}`;
    case 'evaluations':
      return `Evaluations of ${short(subject)}`;
    case 'batch_sends':
      return `Sends by the batch processors of ${subject}`;
  }
  return `${metric.replaceAll('_', ' ')} of ${subject}`;
}

/** relative words a count's change as a percentage of its value before. */
export function relative(before: number, after: number): string {
  if (before === after) return '';
  if (before === 0) return 'new';
  const p = ((after - before) / before) * 100;
  const abs = Math.abs(p);
  const n = abs < 0.1 ? '<0.1' : abs < 10 ? String(Number(abs.toFixed(1))) : String(Math.round(abs));
  return `${p < 0 ? '−' : '+'}${n}%`;
}

/**
 * measuredViews words what a verification measured: the server's counts of
 * the work the recommendation is about that the change moved, then the
 * records the exporters received (from the comparison), shown even when
 * equal since that is the data staying the same. Less work is the change's
 * aim; fewer exported records is a warning.
 */
export function measuredViews(v: RecommendationVerification): MeasuredView[] {
  const out: MeasuredView[] = (v.measured ?? []).filter((m) => m.before !== m.after).map((m) => ({
    label: measureLabel(m.metric, m.subject),
    before: count(m.before),
    after: count(m.after),
    change: relative(m.before, m.after),
    tone: m.after < m.before ? 'pass' : 'neutral',
  }));
  const exporters = v.comparison?.exporters ?? [];
  if (exporters.length) {
    const before = exporters.reduce((n, e) => n + e.before, 0);
    const after = exporters.reduce((n, e) => n + e.after, 0);
    out.push({ label: 'Records the exporters received', before: count(before), after: count(after), change: relative(before, after),
      tone: after === before ? 'neutral' : 'warn' });
  }
  return out;
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
  const base = { reasons, assertions, candidate, measured: v.error ? [] : measuredViews(v) };
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
  const untested = views.filter((v) => v.kind.label === 'Untested').length;
  const parts = [`${views.length} ${views.length === 1 ? 'recommendation' : 'recommendations'}`];
  if (risky) parts.push(`${risky} about data at risk`);
  if (untested) parts.push(`${untested} not tested by this input`);
  return parts.join(', ');
}

/** RecommendationGroup is one section of the Recommendations tab. */
export interface RecommendationGroup {
  id: 'any_input' | 'this_input' | 'untested';
  title: string;
  hint: string;
  views: RecommendationView[];
}

/**
 * recommendationGroups sorts a run's recommendations by how far they can be
 * trusted: those that hold for any input, those this input shows, and what
 * this input left untested; report order within each. Empty groups are left
 * out.
 */
export function recommendationGroups(views: RecommendationView[]): RecommendationGroup[] {
  const groups: RecommendationGroup[] = [
    { id: 'any_input', title: 'Holds for any input', hint: 'The configuration or the statement itself proves these.', views: [] },
    { id: 'this_input', title: 'Depends on this input', hint: "This run's input shows these; check them against your own traffic.", views: [] },
    { id: 'untested', title: 'Not tested by this input', hint: 'No record in this input reached these. Add records they should act on to the scenario to test them.', views: [] },
  ];
  for (const v of views) {
    groups[v.untested || v.kind.label === 'Untested' ? 2 : v.holds === 'any_input' ? 0 : 1].views.push(v);
  }
  return groups.filter((g) => g.views.length > 0);
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
