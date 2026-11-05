import type { EvaluationInfo } from '../api/types';
import type { IconName } from '../ui/Icon';

/** Who recorded an evaluation: the platform names the client that called it. */
const CLIENTS: Record<string, string> = { mcp: 'an MCP client', http: 'the HTTP API', cli: 'the CLI' };
const ICON: Record<string, IconName> = { pass: 'circle-check', fail: 'circle-x', inconclusive: 'triangle-alert', error: 'circle-x' };

export interface EvaluationRow {
  id: string;
  outcome: string;
  icon: IconName;
  /** The class suffix of `od-assert--*`. */
  tone: string;
  by: string;
  created: string;
}

/**
 * evaluationRows lists the assertion checks recorded beside a run (MCP's
 * linea_evaluate_assertions records one; the web's own checks are not
 * recorded), newest first.
 */
export function evaluationRows(list: EvaluationInfo[] | undefined): EvaluationRow[] {
  return (Array.isArray(list) ? [...list] : [])
    .sort((a, b) => b.created.localeCompare(a.created))
    .map((e) => ({
      id: e.id,
      outcome: e.outcome,
      icon: ICON[e.outcome] ?? 'circle-help',
      tone: e.outcome === 'error' ? 'fail' : e.outcome,
      by: CLIENTS[e.client] ?? e.client,
      created: e.created,
    }));
}
