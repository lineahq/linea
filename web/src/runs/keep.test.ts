import { describe, expect, it } from 'vitest';
import { ApiError } from '../api/client';
import type { Run } from '../api/types';
import { isKept, KEEP_HELP, keepLabel, keepProblem, keepTitle, withKept } from './keep';

const run = (patch: Partial<Run> = {}) => ({ id: 'r1', state: 'done', created: '2026-10-30T00:00:00Z', project_id: 'default', ...patch }) as Run;

describe('run keep mark', () => {
  it('reads kept from the run, absent meaning not kept', () => {
    expect(isKept(run())).toBe(false);
    expect(isKept(run({ kept: false }))).toBe(false);
    expect(isKept(run({ kept: true }))).toBe(true);
    expect(isKept(undefined)).toBe(false);
  });

  it('words the toggle by its state and always says what keeping means', () => {
    expect(keepLabel(run())).toBe('Keep');
    expect(keepLabel(run({ kept: true }))).toBe('Kept');
    expect(keepTitle(run())).toBe(`Click to keep this run. ${KEEP_HELP}`);
    expect(keepTitle(run({ kept: true }))).toBe(`Click to let retention remove this run again. ${KEEP_HELP}`);
    expect(KEEP_HELP).toBe('Kept runs are never removed by retention or prune.');
  });

  it('puts the returned mark into the cached run and keeps its report', () => {
    const cached = run({ report: { schema: 'x' } as unknown as Run['report'] });
    const kept = withKept(cached, { kept: true });
    expect(kept.kept).toBe(true);
    expect(kept.report).toBe(cached.report);
    const unkept = withKept(kept, {});
    expect('kept' in unkept).toBe(false);
    expect(unkept.report).toBe(cached.report);
  });
});

describe('keepProblem', () => {
  it('words a refusal, a missing run and a server without history', () => {
    expect(keepProblem(new ApiError(403, 'forbidden: only an admin may change a run another principal submitted'))).toBe("Only the run's owner or an admin may keep or unkeep it.");
    expect(keepProblem(new ApiError(404, 'run was deleted'))).toBe('This run no longer exists.');
    expect(keepProblem(new ApiError(503, 'no saved work'))).toBe('This server keeps no run history, so a run cannot be kept.');
  });

  it('quotes other failures', () => {
    expect(keepProblem(new ApiError(500, 'boom'))).toBe('Could not change whether the run is kept: boom');
    expect(keepProblem(new Error('offline'))).toBe('Could not change whether the run is kept: offline');
  });
});
