import { describe, expect, it } from 'vitest';
import type { OTTLReport, StatementTest } from '../api/types';
import { statementTestOutcome, statementTestRequest } from './statementTest';

const report = { processor: 'transform/scrub', signal: 'logs' } as OTTLReport;
const candidate = { patch: '', proposed_yaml: 'x', valid: true };

describe('statement test on a run', () => {
  it('replaces the statement as the run recorded it', () => {
    expect(statementTestRequest(report, 1, 2, 'set(a, 1)', 'set(a, 2)')).toEqual({
      edit: { processor: 'transform/scrub', signal: 'logs', group: 1, index: 2, op: 'replace', text: 'set(a, 2)', expected: 'set(a, 1)' },
    });
  });

  it('names the first error of an invalid candidate', () => {
    const t = { candidate: { ...candidate, valid: false, diagnostics: [{ severity: 'warning', message: 'w' }, { severity: 'error', message: 'bad path' }] }, has_assertions: false } as StatementTest;
    expect(statementTestOutcome(t)).toEqual({ kind: 'invalid', message: 'The edited configuration does not validate: bad path. No run was started.' });
  });

  it('points to the queued run and says whether assertions are checked', () => {
    const run = { id: 'run-2' } as StatementTest['run'];
    const checked = statementTestOutcome({ candidate, run, has_assertions: true } as StatementTest);
    expect(checked).toMatchObject({ kind: 'queued', runId: 'run-2' });
    expect(checked.kind === 'queued' && checked.note).toContain('checks the run’s assertions');
    const unchecked = statementTestOutcome({ candidate, run, has_assertions: false } as StatementTest);
    expect(unchecked.kind === 'queued' && unchecked.note).toContain('no assertions: open the test run');
  });
});
