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
