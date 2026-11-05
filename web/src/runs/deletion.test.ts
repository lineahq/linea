import { describe, expect, it } from 'vitest';
import { ApiError } from '../api/client';
import type { Run } from '../api/types';
import { canDelete, DELETE_WARNING, deletedRun, deleteWarning, runLoadText } from './deletion';

const run = (state: Run['state']) => ({ id: 'r1', state, created: '2026-10-13T12:00:00Z' }) as Run;

describe('run deletion', () => {
  it('offers deletion only for finished runs', () => {
    expect(['done', 'error', 'cancelled'].map((s) => canDelete(run(s as Run['state'])))).toEqual([true, true, true]);
    expect(['queued', 'running'].map((s) => canDelete(run(s as Run['state'])))).toEqual([false, false]);
    expect(canDelete(undefined)).toBe(false);
  });

  it('tells a deleted run from other failed reads', () => {
    const deleted = new ApiError(404, 'run was deleted', { error: 'run was deleted' });
    expect(deletedRun(deleted)).toBe(true);
    expect(runLoadText(deleted)).toBe('This run was deleted.');
    expect(deletedRun(new ApiError(404, 'unknown run'))).toBe(false);
    expect(runLoadText(new ApiError(404, 'unknown run'))).toBe('Could not load this run: unknown run');
    expect(deletedRun(new Error('run was deleted'))).toBe(false);
  });

  it('words the confirmation for one run or several', () => {
    expect(deleteWarning(1)).toBe(DELETE_WARNING);
    expect(deleteWarning(2)).toBe('Delete these 2 runs? Their reports, recorded evidence, captured inputs and recorded assertion evaluations are removed for good. Saved sources are kept.');
  });
});
