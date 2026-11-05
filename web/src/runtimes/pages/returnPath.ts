/**
 * The return path of runtime setup (application map, rule 5): a runtime is
 * set up from somewhere, usually the debugger experiment that needed it, and
 * the user comes back there afterwards to choose it beside Run. The origin
 * travels as the `from` search parameter of /runtimes and of a build page: an
 * in-app location such as "/" or "/?run=r1".
 */

/** ReturnTarget is where "back" goes and what it is called. */
export interface ReturnTarget {
  /** An in-app location: path, search and hash. */
  href: string;
  label: string;
  /** The origin is the debugger, so the page can name the draft being set up. */
  debugger: boolean;
}

/** FROM_PARAM is the search parameter that carries the origin. */
export const FROM_PARAM = 'from';

const MAX_FROM = 2048;

/**
 * safeFrom accepts only an in-app location: a string starting with a single
 * slash, so a crafted link can never send the user to another site.
 */
export function safeFrom(from: unknown): string | undefined {
  if (typeof from !== 'string' || from.length === 0 || from.length > MAX_FROM) return undefined;
  if (!from.startsWith('/') || from.startsWith('//') || from.includes('\\')) return undefined;
  if (/[\u0000-\u001f]/.test(from)) return undefined; // eslint-disable-line no-control-regex -- rejects control characters
  return from;
}

function pathOf(href: string): string {
  return href.split(/[?#]/, 1)[0] || '/';
}

/**
 * returnTarget says where the user returns to after runtime setup, or
 * undefined when the page was opened directly (or from a runtimes page, which
 * would be a loop).
 */
export function returnTarget(from: unknown): ReturnTarget | undefined {
  const href = safeFrom(from);
  if (!href) return undefined;
  const path = pathOf(href);
  const first = path.split('/').filter(Boolean)[0] ?? '';
  switch (first) {
    case '': return { href, label: 'Back to the experiment', debugger: true };
    case 'runs': return { href, label: 'Back to all runs', debugger: false };
    case 'captures': return { href, label: 'Back to Live capture', debugger: false };
    case 'fleet': return { href, label: 'Back to Fleet', debugger: false };
    case 'saved': return { href, label: 'Back to Saved work', debugger: false };
    case 'runtimes': return undefined;
    default: return { href, label: 'Back', debugger: false };
  }
}

/** withFrom appends the origin to an in-app path, keeping an existing query. */
export function withFrom(path: string, from: unknown): string {
  const safe = safeFrom(from);
  if (!safe) return path;
  return `${path}${path.includes('?') ? '&' : '?'}${FROM_PARAM}=${encodeURIComponent(safe)}`;
}

/** REBUILD_PARAM is the search parameter of /runtimes that opens the builder from an earlier build. */
export const REBUILD_PARAM = 'rebuild';

/** rebuildHref is the Runtimes page opened on the builder, seeded from a build, keeping the origin. */
export function rebuildHref(buildId: string, from: unknown): string {
  return withFrom(`/runtimes?${REBUILD_PARAM}=${encodeURIComponent(buildId)}`, from);
}

/** rebuildOf reads the build to rebuild from a router search object of unknown shape; ids are plain words. */
export function rebuildOf(search: unknown): string | undefined {
  if (!search || typeof search !== 'object') return undefined;
  const id = (search as Record<string, unknown>)[REBUILD_PARAM];
  return typeof id === 'string' && /^[\w.-]{1,128}$/.test(id) ? id : undefined;
}

/** fromOf reads the origin defensively from a router search object of unknown shape. */
export function fromOf(search: unknown): string | undefined {
  if (!search || typeof search !== 'object') return undefined;
  return safeFrom((search as Record<string, unknown>)[FROM_PARAM]);
}

/** setupTitle words what is being set up: the draft, when the origin is the debugger and has a named one. */
export function setupTitle(target: ReturnTarget, draftPath: string): { before: string; draft?: string } {
  if (target.debugger && draftPath.trim()) return { before: 'Setting up a runtime for your configuration', draft: draftPath.trim() };
  return { before: target.debugger ? 'Setting up a runtime for your experiment' : 'Setting up a runtime' };
}
