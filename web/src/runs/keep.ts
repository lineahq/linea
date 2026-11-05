import { ApiError } from '../api/client';
import type { Run } from '../api/types';

/** KEEP_HELP says what keeping a run does; it is the toggle's tooltip. */
export const KEEP_HELP = 'Kept runs are never removed by retention or prune.';

/** isKept reads the run's mark; the server leaves `kept` out for a run that is not kept. */
export function isKept(run?: Run): boolean {
  return run?.kept === true;
}

/** keepLabel is the toggle's text: the state it shows, which a click reverses. */
export function keepLabel(run?: Run): string {
  return isKept(run) ? 'Kept' : 'Keep';
}

/** keepTitle is the toggle's tooltip: what a click does, then what keeping means. */
export function keepTitle(run?: Run): string {
  return `${isKept(run) ? 'Click to let retention remove this run again.' : 'Click to keep this run.'} ${KEEP_HELP}`;
}

/** withKept is the cached run with the mark a keep or unkeep returned; its report and the rest stay. */
export function withKept(run: Run, returned: Pick<Run, 'kept'>): Run {
  const { kept: _was, ...rest } = run;
  return returned.kept === true ? { ...rest, kept: true } : rest;
}

/** keepProblem words a failed keep or unkeep. */
export function keepProblem(error: Error): string {
  if (error instanceof ApiError) {
    if (error.status === 403) return "Only the run's owner or an admin may keep or unkeep it.";
    if (error.status === 404) return 'This run no longer exists.';
    if (error.status === 503) return 'This server keeps no run history, so a run cannot be kept.';
  }
  return `Could not change whether the run is kept: ${error.message}`;
}
