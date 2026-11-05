import type { RuntimeBuild } from '../../api/types';
import { finished } from '../runtimes';
import { formatElapsed } from './elapsed';

/** The pure parts of the build page: elapsed time, the numbered log and how a build ended. */

/**
 * buildElapsed is how long the build has run (to `now`) or ran (to its end).
 * A queued build, or one with unreadable times, has none.
 */
export function buildElapsed(b: Pick<RuntimeBuild, 'state' | 'started' | 'finished'>, now: number): string | undefined {
  if (!b.started) return undefined;
  const start = Date.parse(b.started);
  if (Number.isNaN(start)) return undefined;
  const end = finished(b.state) && b.finished ? Date.parse(b.finished) : now;
  if (Number.isNaN(end)) return undefined;
  return formatElapsed(end - start);
}

export interface NumberedLine {
  /** 1-based line number in the build's whole log. */
  n: number;
  text: string;
}

/** numberedLog numbers the lines kept, counting the earlier ones the server dropped. */
export function numberedLog(lines: string[], skipped: number): NumberedLine[] {
  return lines.map((text, i) => ({ n: skipped + i + 1, text }));
}

/** logText is the log as text for copying, with a note when earlier lines were dropped. */
export function logText(lines: string[], skipped: number): string {
  const head = skipped > 0 ? `[${skipped} earlier log lines are not kept]\n` : '';
  return head + lines.join('\n');
}

/** BuildOutcome words how a finished build ended, with the tone of its callout. */
export function buildOutcome(b: Pick<RuntimeBuild, 'state' | 'name' | 'error' | 'reused'>): { tone: 'pass' | 'fail' | 'warn'; title: string; body: string } | undefined {
  switch (b.state) {
    case 'done':
      return {
        tone: 'pass',
        title: `Built ${b.name}`,
        body: b.reused
          ? 'An identical build already existed, so its binary was reused and nothing was compiled. The runtime is registered under this name and runs on the pool workers.'
          : 'The runtime is registered under this name and runs on the pool workers.',
      };
    case 'cancelled': return { tone: 'warn', title: 'Build cancelled', body: `${b.name} was not built; runs keep using the runtime it already names.` };
    case 'failed': return { tone: 'fail', title: 'Build failed', body: b.error || 'The build ended with an error; the log shows where.' };
    default: return undefined;
  }
}

/** buildTime words when a build was created, for the build list. */
export function buildTime(b: Pick<RuntimeBuild, 'created'>): string {
  const d = new Date(b.created);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
}
