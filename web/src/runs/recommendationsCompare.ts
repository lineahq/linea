// View model of the comparison's `recommendations` section: which of the
// before run's recommendations the change resolved, which are new in the
// after run and which persist. Pure wording over the platform's result; it
// matches, counts and ranks nothing, and never changes the comparison's
// outcome.
import type { RecommendationDiff, RunComparison } from '../api/types';
import { benefitChangeText, ruleTitle } from '../debugger/recommendations';

export type RecommendationStatus = 'resolved' | 'new' | 'persists';

type Side = 'before' | 'after';

export interface RecommendationChangeRow {
  key: string;
  rule: string;
  /** The rule's short title; an unknown rule shows its ID. */
  title: string;
  status: string;
  statusLabel: string;
  /** pass: the change resolved it; warn: it is new; neutral: it stayed. */
  tone: 'pass' | 'warn' | 'neutral';
  pipeline?: string;
  /** Component refs the recommendation names, such as `processor/transform`. */
  components: string[];
  /** The item inside the component: a statement, an attribute path or a policy. */
  subject?: string;
  /** The server's summary: the after run's for new and persisting, else the before run's. */
  summary: string;
  /** For a persisting recommendation whose benefit count moved: "12 → 3 evaluations had no effect". */
  benefitChange?: string;
  /** The runs that hold it, to open: the before run, the after run, or both. */
  runs: Side[];
}

export interface RecommendationChangesView {
  summary: string;
  rows: RecommendationChangeRow[];
}

const STATUSES: Record<RecommendationStatus, { label: string; tone: RecommendationChangeRow['tone']; runs: Side[] }> = {
  resolved: { label: 'Resolved', tone: 'pass', runs: ['before'] },
  new: { label: 'New', tone: 'warn', runs: ['after'] },
  persists: { label: 'Persists', tone: 'neutral', runs: ['before', 'after'] },
};

/** recommendationsSummary is one line over the counts, leaving out the zero ones. */
export function recommendationsSummary(r: { resolved: number; new: number; persists: number }): string {
  const parts = [r.resolved && `${r.resolved} resolved`, r.new && `${r.new} new`, r.persists && `${r.persists} persist`].filter(Boolean);
  return parts.length ? parts.join(', ') : 'No recommendation in either run';
}

function rowOf(d: RecommendationDiff, i: number): RecommendationChangeRow {
  const known = (STATUSES as Record<string, (typeof STATUSES)[RecommendationStatus] | undefined>)[d.status];
  const shown = d.status === 'resolved' ? d.before ?? d.after : d.after ?? d.before;
  const a = d.before?.benefit, b = d.after?.benefit;
  const present: Side[] = [d.before && 'before', d.after && 'after'].filter((x): x is Side => !!x);
  return {
    key: `${i}:${d.status}:${d.rule}:${d.before?.index ?? ''}:${d.after?.index ?? ''}`,
    rule: d.rule,
    title: ruleTitle(d.rule),
    status: d.status,
    statusLabel: known?.label ?? d.status,
    tone: known?.tone ?? 'neutral',
    pipeline: d.pipeline,
    components: d.components ?? [],
    subject: d.subject,
    summary: shown?.summary ?? '',
    benefitChange: d.status === 'persists' && a && b ? benefitChangeText(a, b) : undefined,
    runs: known?.runs ?? present,
  };
}

/**
 * recommendationChanges words the comparison's recommendations section, in the
 * platform's order (the before run's, then the after run's new ones); a
 * comparison without the section gives none.
 */
export function recommendationChanges(c: Pick<RunComparison, 'recommendations'>): RecommendationChangesView | undefined {
  const r = c.recommendations;
  if (!r) return undefined;
  return { summary: recommendationsSummary(r), rows: (r.items ?? []).map(rowOf) };
}
