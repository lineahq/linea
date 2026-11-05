import type { RuntimeBuild } from '../api/types';
import { finished, mergeLog, type BuildLog } from './runtimes';

/**
 * Following a build (GET /runtime-builds/{id} and its /events stream): the
 * state a build page shows, and how each reply or stream event changes it.
 * Pure, so the stream handling is tested without a browser.
 */

export interface Followed {
  build?: RuntimeBuild;
  /** The log read so far; an event carries only the lines not yet sent. */
  log: BuildLog;
  /** The build ended and every line of its log was read. */
  ended: boolean;
}

export const NOT_FOLLOWED: Followed = { log: { lines: [], skipped: 0 }, ended: false };

/** BuildReply is what a reply or stream event is: a read of the build, a "build" event or the final "end" event. */
export type BuildReply = 'read' | 'build' | 'end';

/**
 * follow applies a build and its new log lines. The "end" event repeats the
 * build without its log, so the log read so far stands; a read of a build that
 * has already ended is the whole story too. Only a read carries the build's
 * manifest, so the manifest read stands through the events.
 */
export function follow(prev: Followed, reply: BuildReply, b: RuntimeBuild): Followed {
  const manifest = b.manifest_yaml ?? prev.build?.manifest_yaml;
  const build = manifest === undefined ? b : { ...b, manifest_yaml: manifest };
  if (reply === 'end') return { build, log: prev.log, ended: true };
  const log = mergeLog(prev.log, b);
  return { build, log, ended: reply === 'read' && finished(b.state) };
}

/** parseBuildEvent reads the data of a "build" or "end" server-sent event; undefined if it is not a build. */
export function parseBuildEvent(data: string): RuntimeBuild | undefined {
  try {
    const b = JSON.parse(data) as RuntimeBuild;
    return b && typeof b.id === 'string' && typeof b.state === 'string' ? b : undefined;
  } catch {
    return undefined;
  }
}
