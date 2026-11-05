import type { UpgradeCheck } from '../../api/types';

/**
 * The state of an upgrade check on its page. A check is one request that
 * waits for the whole comparison (api.checkUpgrade), so it belongs to the
 * page that started it: leaving cancels it (owner decision "Warn on leaving").
 * This reducer is pure; the page owns the AbortController and the blocker.
 */

/** CheckSubject is what a check was started on, kept to word the result. */
export interface CheckSubject {
  /** The configuration draft's path, or empty for an unsaved draft. */
  configLabel: string;
  scenarios: number;
  from: string;
  to: string;
  startedAt: number;
}

/** Failure is a refused or broken request, worded by problemOf. */
export interface Failure {
  message: string;
  details: string[];
}

/**
 * CheckState is one page-bound request's state. R is its result and S what
 * it was started on; the config bisect reuses it with its own.
 */
export type CheckState<R = UpgradeCheck, S extends { startedAt: number } = CheckSubject> =
  | { phase: 'idle'; id: number }
  | { phase: 'running'; id: number; subject: S }
  | { phase: 'done'; id: number; subject: S; check: R; elapsedMs: number }
  | { phase: 'failed'; id: number; subject: S; failure: Failure }
  | { phase: 'cancelled'; id: number; subject: S };

export type CheckAction<R = UpgradeCheck, S extends { startedAt: number } = CheckSubject> =
  | { type: 'start'; subject: S }
  | { type: 'finish'; id: number; check: R; now: number }
  | { type: 'fail'; id: number; failure: Failure }
  | { type: 'cancel' }
  | { type: 'reset' };

export const initialCheck = { phase: 'idle' as const, id: 0 };

/**
 * checkReducer moves a check through its phases. Only the running check can
 * finish or fail, and only by the id it was started with, so the late answer
 * of a cancelled request (or of an earlier one) is ignored. A check cannot
 * start while another runs.
 */
export function checkReducer<R, S extends { startedAt: number }>(state: CheckState<R, S>, action: CheckAction<R, S>): CheckState<R, S> {
  switch (action.type) {
    case 'start':
      if (state.phase === 'running') return state;
      return { phase: 'running', id: state.id + 1, subject: action.subject };
    case 'finish':
      if (state.phase !== 'running' || state.id !== action.id) return state;
      return { phase: 'done', id: state.id, subject: state.subject, check: action.check, elapsedMs: Math.max(0, action.now - state.subject.startedAt) };
    case 'fail':
      if (state.phase !== 'running' || state.id !== action.id) return state;
      return { phase: 'failed', id: state.id, subject: state.subject, failure: action.failure };
    case 'cancel':
      if (state.phase !== 'running') return state;
      return { phase: 'cancelled', id: state.id, subject: state.subject };
    case 'reset':
      if (state.phase === 'running') return state;
      return { phase: 'idle', id: state.id };
  }
}

/** isRunning is true while a check's request is in flight. */
export function isRunning(state: { phase: CheckState['phase'] }): boolean {
  return state.phase === 'running';
}

/** blocksLeaving is true when navigating away would cancel work: the check is running. */
export function blocksLeaving(state: { phase: CheckState['phase'] }): boolean {
  return isRunning(state);
}

/** LEAVE_TITLE and LEAVE_BODY are the banner while a check runs. */
export const LEAVE_TITLE = 'Leaving this page cancels the check';
export const LEAVE_BODY = 'Checks do not run in the background yet: the comparison is tied to this page.';

/** leaveQuestion asks before a navigation that would cancel the running check. */
export function leaveQuestion(nextPath: string | undefined): string {
  return `Cancel the check and leave${nextPath ? ` for ${nextPath}` : ''}? Its runs are cancelled and no result is kept.`;
}

/** isAbort is true for the rejection of an aborted request. */
export function isAbort(e: unknown): boolean {
  return typeof e === 'object' && e !== null && (e as { name?: string }).name === 'AbortError';
}
