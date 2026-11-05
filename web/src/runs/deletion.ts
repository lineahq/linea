import { ApiError } from '../api/client';
import type { Run } from '../api/types';
import { runActive } from '../api/runState';

/** DELETE_WARNING is the inline confirmation's text: deletion cannot be undone. */
export const DELETE_WARNING =
  'Delete this run? Its report, recorded evidence, captured inputs and recorded assertion evaluations are removed for good. Saved sources are kept.';

/** canDelete: only a finished run can be deleted; a queued or running one is cancelled first. */
export function canDelete(run?: Run): boolean {
  return !!run && !runActive(run.state);
}

/** deletedRun reports whether a failed run read means the run was deleted. */
export function deletedRun(error?: Error | null): boolean {
  return error instanceof ApiError && error.status === 404 && error.message === 'run was deleted';
}

/** runLoadText words a run that could not be loaded. */
export function runLoadText(error: Error): string {
  return deletedRun(error) ? 'This run was deleted.' : `Could not load this run: ${error.message}`;
}

/**
 * deleteWarning is DELETE_WARNING for one or more runs: the same consequences,
 * counted. One run reads exactly as DELETE_WARNING.
 */
export function deleteWarning(count: number): string {
  return count === 1
    ? DELETE_WARNING
    : `Delete these ${count} runs? Their reports, recorded evidence, captured inputs and recorded assertion evaluations are removed for good. Saved sources are kept.`;
}
