import type { OTTLReport, StatementTest, StatementTestRequest } from '../api/types';

/**
 * statementTestRequest replaces one statement in the run's recorded
 * configuration (not the editor's draft): the server tests it on the run's
 * recorded inputs, on the run's runtime, with the run's assertions.
 */
export function statementTestRequest(report: OTTLReport, group: number, index: number, recorded: string, text: string): StatementTestRequest {
  return { edit: { processor: report.processor, signal: report.signal, group, index, op: 'replace', text, expected: recorded } };
}

export type StatementTestOutcome =
  | { kind: 'invalid'; message: string }
  | { kind: 'queued'; runId: string; note: string };

/** statementTestOutcome says what a statement test did: refused the edit, or queued a run. */
export function statementTestOutcome(t: StatementTest): StatementTestOutcome {
  if (!t.candidate.valid || !t.run) {
    const first = t.candidate.diagnostics?.find((d) => d.severity === 'error') ?? t.candidate.diagnostics?.[0];
    return { kind: 'invalid', message: `The edited configuration does not validate${first ? `: ${first.message}` : ''}. No run was started.` };
  }
  return {
    kind: 'queued',
    runId: t.run.id,
    note: t.has_assertions
      ? 'It runs the recorded inputs with the edited statement and checks the run’s assertions.'
      : 'It runs the recorded inputs with the edited statement. The run had no assertions: open the test run to see what the edit changed.',
  };
}
