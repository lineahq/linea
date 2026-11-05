import type { Run, RunLog } from '../api/types';

/**
 * keepsRunLog mirrors the server's rule: a run that failed, or whose
 * execution ended partial or failed, keeps the tail of its Collector log.
 */
export function keepsRunLog(run: Pick<Run, 'state' | 'report'> | undefined): boolean {
  if (!run) return false;
  if (run.state === 'error') return true;
  const status = run.report?.result?.status;
  return run.state === 'done' && (status === 'partial' || status === 'failed');
}

/** RunLogView is what the Overview shows of a kept log. */
export interface RunLogView {
  text: string;
  lines: number;
  /** note explains an empty or truncated log. */
  note?: string;
}

/** runLogView describes a kept log; undefined when the run kept none. */
export function runLogView(log: RunLog | undefined): RunLogView | undefined {
  if (!log?.kept) return undefined;
  const text = log.log.replace(/\n$/, '');
  if (!text) return { text: '', lines: 0, note: 'The Collector logged nothing at info level or above.' };
  const lines = text.split('\n').length;
  return {
    text,
    lines,
    note: log.truncated ? 'Earlier lines were dropped; this is the last 64 KB.' : undefined,
  };
}
