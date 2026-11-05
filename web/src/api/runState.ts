/** Run lifecycle states the debugger API reports. */

/**
 * runActive: the run has not ended. Assigned means a worker claimed the run
 * and has not started it yet.
 */
export function runActive(state: string | undefined): boolean {
  return state === 'queued' || state === 'assigned' || state === 'running';
}
