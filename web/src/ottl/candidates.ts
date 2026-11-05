import { ApiError } from '../api/client';
import type { OperationError } from '../api/types';

/**
 * editProblem says why the server would not build a statement edit. A stale
 * edit (409) quotes the statement as it now reads in the configuration draft.
 */
export function editProblem(e: unknown): string {
  if (e instanceof ApiError && e.status === 409) {
    const current = (e.body as OperationError | undefined)?.current_statement;
    return current
      ? `The statement has changed in the editor since this run (it now reads ${current}); rerun first.`
      : 'The statement has changed in the editor since this run; rerun first.';
  }
  return e instanceof Error ? e.message : String(e);
}
