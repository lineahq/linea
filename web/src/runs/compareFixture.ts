// A comparison for the compare view's tests.
import type { RunComparison } from '../api/types';

const info = (id: string) => ({ id, mode: 'scenario', end: 'done', execution_status: 'completed', complete: true });
const delivered = { lineage_id: 'lin-1', delivered: ['otlp/backend (logs)'], stopped: [], path: [{ component: 'otlp' }, { component: 'filter/health', pipeline: 'logs' }, { component: 'otlp/backend', end: 'delivered' }] };

/** Shaped like `diff --format json` on the logs-cleanup sample with one statement edited. */
export const different: RunComparison = {
  comparison_schema: '0.1', outcome: 'different', before: info('run-a'), after: info('run-b'), inputs_identical: true,
  exporters: [{ exporter: 'otlp/backend', signal: 'logs', before: 3, after: 2, equal: false }, { exporter: 'debug', signal: 'logs', before: 1, after: 1, equal: true }],
  records: {
    aligned: 4, changed: 2, unaligned: 0, uncertain: 1, items: [
      { input: 1, ref: 'resource[0].scope[0].log_record[0]', signal: 'logs', name: 'login retry', before: delivered, after: delivered, fate_changed: false,
        changes: [
          { exporter: 'otlp/backend (logs)', path: 'log.severity_text', op: 'modified', before: { value: 'WARNING', type: 'Str' }, after: { value: 'warning', type: 'Str' } },
          { exporter: 'otlp/backend (logs)', path: 'resource.attributes["env"]', op: 'type_changed', before: { value: 1, type: 'Int' }, after: { value: '1', type: 'Str' } },
          { exporter: 'otlp/backend (logs)', path: 'log.attributes["note"]', op: 'added', after: { value: '', type: 'Str' } },
        ] },
      { input: 1, ref: 'resource[0].scope[0].log_record[2]', signal: 'logs', before: delivered, after: { lineage_id: 'lin-2', delivered: [], stopped: ['dropped at filter/health (logs)'], path: [{ component: 'otlp' }, { component: 'filter/health', pipeline: 'logs', end: 'dropped' }] }, fate_changed: true, diverges_at: 'filter/health (logs)', confidence: 'medium' },
    ],
  },
  routes: [], errors: [], assertions: [], ignored: [],
};

/** The comparison's recommendations section: one resolved, one new and one persisting (its benefit fell). */
export const recommendationDiffs: NonNullable<RunComparison['recommendations']> = {
  resolved: 1, new: 1, persists: 1,
  items: [
    { rule: 'ottl.statement_never_matched', status: 'resolved', pipeline: 'logs', components: ['processor/transform'], subject: 'set(log.attributes["a"], "b") where false',
      before: { index: 0, kind: 'redundancy', confidence: 'observed', summary: 'Statement 0 never matched in this run.' } },
    { rule: 'ottl.statement_no_change', status: 'persists', pipeline: 'logs', components: ['processor/transform'], subject: 'set(log.attributes["c"], "d")',
      before: { index: 1, kind: 'redundancy', confidence: 'observed', summary: 'Statement 1 changed nothing in 12 evaluations.', benefit: { metric: 'evaluations_without_effect', count: 12 } },
      after: { index: 0, kind: 'redundancy', confidence: 'observed', summary: 'Statement 1 changed nothing in 3 evaluations.', benefit: { metric: 'evaluations_without_effect', count: 3 } } },
    { rule: 'sampling.policy_shadowed', status: 'new', pipeline: 'traces', components: ['processor/tail_sampling'], subject: 'slow',
      after: { index: 1, kind: 'inefficiency', confidence: 'observed', summary: 'Policy slow is shadowed by an earlier policy.' } },
  ],
};
