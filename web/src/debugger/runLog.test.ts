import { describe, expect, it } from 'vitest';
import type { Run, RunLog } from '../api/types';
import { keepsRunLog, runLogView } from './runLog';

const run = (state: Run['state'], status?: string) =>
  ({ state, report: status ? { result: { status } } : undefined }) as unknown as Run;

describe('keepsRunLog', () => {
  it('asks only for a failed or partial run', () => {
    expect(keepsRunLog(run('error'))).toBe(true);
    expect(keepsRunLog(run('done', 'partial'))).toBe(true);
    expect(keepsRunLog(run('done', 'failed'))).toBe(true);
    expect(keepsRunLog(run('done', 'completed'))).toBe(false);
    expect(keepsRunLog(run('done', 'cancelled'))).toBe(false);
    expect(keepsRunLog(run('running'))).toBe(false);
    expect(keepsRunLog(run('cancelled'))).toBe(false);
    expect(keepsRunLog(undefined)).toBe(false);
  });
});

describe('runLogView', () => {
  const log = (patch: Partial<RunLog>): RunLog => ({ run_id: 'r', kept: true, log: '', truncated: false, ...patch });

  it('shows nothing for a run without a kept log', () => {
    expect(runLogView(undefined)).toBeUndefined();
    expect(runLogView(log({ kept: false, log: 'x\n' }))).toBeUndefined();
  });

  it('counts lines and explains an empty or truncated log', () => {
    expect(runLogView(log({}))).toEqual({ text: '', lines: 0, note: 'The Collector logged nothing at info level or above.' });
    expect(runLogView(log({ log: 'a\nb\n' }))).toEqual({ text: 'a\nb', lines: 2, note: undefined });
    expect(runLogView(log({ log: 'b\n', truncated: true }))?.note).toBe('Earlier lines were dropped; this is the last 64 KB.');
  });
});
