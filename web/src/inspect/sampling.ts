// Sampling decisions: a tail_sampling processor keeps or drops whole traces
// by policies evaluated on all of a trace's spans (internal/debugger/sampling). These
// helpers turn the report's explanation into what the Decisions tab shows:
// per trace, the decision, the policy that made it, and what each policy
// found in the trace.
import type { RecordLineage, Sampling } from '../api/types';
import { findLineage } from './journey';

export type SampledTrace = NonNullable<Sampling['traces']>[number];

export type Tone = 'kept' | 'dropped' | 'pending';

export interface PolicyCell {
  policy: string;
  /** match: the policy samples the trace on its own; drop: a drop policy matches. */
  outcome: 'match' | 'no-match' | 'drop' | 'error';
  label: string;
  evidence?: string;
  /** The policy decided the trace. */
  decisive: boolean;
}

export interface DecisionRow {
  trace: SampledTrace;
  tone: Tone;
  label: string;
  reason: string;
  late?: string;
  cells: PolicyCell[];
  /** Set when the replay does not reproduce the recorded decision. */
  warning?: string;
  /** Journey of the trace's first span, when the run followed it. */
  lineage?: string;
}

/** samplingFor finds the explanation of one processor instance. */
export function samplingFor(sampling: Sampling[] | undefined, component: string, pipeline?: string): Sampling | undefined {
  return sampling?.find((s) => s.processor === component && s.pipeline === pipeline);
}

const LABELS: Record<string, [Tone, string]> = {
  sampled: ['kept', 'Kept'],
  not_sampled: ['dropped', 'Dropped'],
  dropped: ['dropped', 'Dropped by policy'],
  pending_dropped: ['dropped', 'Dropped at shutdown'],
  pending: ['pending', 'Undecided'],
};

function reason(s: Sampling, t: SampledTrace): string {
  switch (t.decision) {
    case 'sampled':
      return `policy ${t.policy} sampled it`;
    case 'not_sampled':
      return 'no policy sampled it';
    case 'dropped':
      return `drop policy ${t.policy} matched it`;
    case 'pending_dropped':
      return `decision_wait (${s.decision_wait}) had not passed when the processor shut down, and drop_pending_traces_on_shutdown discards undecided traces`;
    case 'pending':
      return 'the run ended before the processor decided';
  }
  return t.decision;
}

function cell(t: SampledTrace, r: NonNullable<SampledTrace['policies']>[number]): PolicyCell {
  const decisive = !!t.policy && t.policy === r.policy;
  switch (r.result) {
    case 'sampled':
      return { policy: r.policy, outcome: 'match', label: 'samples', evidence: r.evidence, decisive };
    case 'dropped':
      return { policy: r.policy, outcome: 'drop', label: 'drops', evidence: r.evidence, decisive };
    case 'not_sampled':
      return { policy: r.policy, outcome: 'no-match', label: 'no match', evidence: r.evidence, decisive };
  }
  return { policy: r.policy, outcome: 'error', label: r.result, evidence: r.evidence, decisive };
}

const WARNINGS: Record<string, string> = {
  replay_differs: 'Replaying the configuration on these spans decided differently, so the policy results may not explain this decision.',
  replay_failed: 'The configuration could not be replayed on these spans.',
};

/** recordRef splits a sampling record ref ("<seq>:<entity>"). */
export function recordRef(ref: string): { seq: number; entity: string } | undefined {
  const i = ref.indexOf(':');
  const seq = Number(ref.slice(0, i));
  return i > 0 && Number.isInteger(seq) ? { seq, entity: ref.slice(i + 1) } : undefined;
}

/** decisionRows explains each trace of a sampling processor. */
export function decisionRows(s: Sampling, lineage?: RecordLineage[]): DecisionRow[] {
  return (s.traces ?? []).map((t) => {
    const [tone, label] = LABELS[t.decision] ?? ['pending', t.decision];
    const first = t.records?.[0] ? recordRef(t.records[0]) : undefined;
    return {
      trace: t,
      tone,
      label,
      reason: reason(s, t),
      late: t.late_spans ? `${t.late_spans} span(s) arrived after the decision and were ${t.late} with it` : undefined,
      cells: (t.policies ?? []).map((r) => cell(t, r)),
      warning: WARNINGS[t.verification],
      lineage: first && findLineage(lineage, first.seq, first.entity)?.id,
    };
  });
}

/** decisionSummary counts kept traces, e.g. "2 of 4 traces kept". */
export function decisionSummary(rows: DecisionRow[]): string {
  const kept = rows.filter((r) => r.tone === 'kept').length;
  return `${kept} of ${rows.length} trace${rows.length === 1 ? '' : 's'} kept`;
}
