import type { CaptureHandoff } from '../capture/handoff';

/** handoffRecordCount is how many captured records a handoff carries. */
export function handoffRecordCount(h: Pick<CaptureHandoff, 'streams'>): number {
  return h.streams.reduce((n, s) => n + s.sequences.length, 0);
}
