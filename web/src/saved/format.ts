import type { ArtifactOrigin } from '../api/types';

/**
 * Formatting for Saved work: short identifiers, times, kinds and origins.
 * Pure, so the pages stay thin.
 */

/**
 * shortId shortens a generated identifier for display ("run-0123456789abcdef"
 * becomes "run-01234567"); the full value stays in titles and links.
 */
export function shortId(id: string): string {
  const m = /^([a-z]+-)([0-9a-f]{9,})$/.exec(id);
  return m ? m[1] + m[2].slice(0, 8) : id;
}

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
const pad = (n: number) => String(n).padStart(2, '0');

function startOfDay(d: Date): number {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
}

/**
 * whenText words a time the way the lists do, in local time: "Today 14:12",
 * "Yesterday 09:30", "Oct 14", or "Oct 14 2025" in another year. A time that
 * cannot be read is returned as it came.
 */
export function whenText(iso: string, now: Date = new Date()): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const time = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  const days = Math.round((startOfDay(now) - startOfDay(d)) / 86_400_000);
  if (days === 0) return `Today ${time}`;
  if (days === 1) return `Yesterday ${time}`;
  const date = `${MONTHS[d.getMonth()]} ${d.getDate()}`;
  return d.getFullYear() === now.getFullYear() ? date : `${date} ${d.getFullYear()}`;
}

/** The saved artifact kinds the server stores, in the order lists show them. */
export const ARTIFACT_KINDS = ['configuration', 'scenario', 'fixture', 'setup', 'manifest'] as const;

const KIND_LABELS: Record<string, string> = {
  configuration: 'Configuration',
  scenario: 'Scenario',
  fixture: 'Fixture',
  setup: 'Setup',
  manifest: 'Manifest',
};

/** kindLabel names an artifact kind; an unknown kind is shown as it is. */
export function kindLabel(kind: string): string {
  return KIND_LABELS[kind] ?? kind;
}

/** plural writes "1 scenario" or "3 scenarios". */
export function plural(n: number, noun: string): string {
  return `${n} ${noun}${n === 1 ? '' : 's'}`;
}

/**
 * originText says where a revision came from. The platform records the
 * origin of derived revisions only; a revision without one was written by the
 * user (typed, opened from a file or saved from a draft).
 */
export function originText(origin?: ArtifactOrigin): string {
  if (!origin) return 'written';
  switch (origin.kind) {
    case 'run':
      return origin.run_id ? `from run ${shortId(origin.run_id)}` : 'from a run';
    case 'candidate':
      return 'from a statement edit';
    case 'capture':
      return 'from a capture';
    default:
      return `from ${origin.kind}`;
  }
}

/** The run an origin names, if any (for a link into the debugger). */
export function originRun(origin?: ArtifactOrigin): string | undefined {
  return origin?.kind === 'run' && origin.run_id ? origin.run_id : undefined;
}

/** savedSection says which Saved work section a path belongs to. */
export function savedSection(pathname: string): 'sources' | 'suites' | 'library' {
  if (pathname === '/saved/library') return 'library';
  return pathname === '/saved/suites' || pathname.startsWith('/saved/suites/') ? 'suites' : 'sources';
}
