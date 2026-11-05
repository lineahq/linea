/**
 * The URL parameters New capture (`/captures/new`) understands, so a capture
 * can be started from another screen with its targets already chosen:
 *
 *   collectors  Collector IDs to preselect (an array, or a comma-separated string)
 *   group       a saved Collector group's ID
 *   from        a capture ID whose request to copy (Duplicate)
 *
 * `parseNewCaptureSearch` reads whatever the router parsed, so it works with or
 * without a `validateSearch` on the route. When `validateSearch` is added to
 * `newCaptureRoute`, it should be exactly `parseNewCaptureSearch`.
 */
export interface NewCaptureSearch {
  collectors?: string[];
  group?: string;
  from?: string;
}

const MAX_PRESELECTED = 16;

const text = (v: unknown): string | undefined => (typeof v === 'string' && v.trim() ? v.trim() : typeof v === 'number' ? String(v) : undefined);

export function parseNewCaptureSearch(raw: Record<string, unknown>): NewCaptureSearch {
  const list = Array.isArray(raw.collectors) ? raw.collectors.map(text) : typeof raw.collectors === 'string' ? raw.collectors.split(',').map(text) : [];
  const collectors = [...new Set(list.filter((v): v is string => !!v))].slice(0, MAX_PRESELECTED);
  const group = text(raw.group);
  const from = text(raw.from);
  return { ...(collectors.length ? { collectors } : {}), ...(group ? { group } : {}), ...(from ? { from } : {}) };
}

/** Which way New capture opens: copy a capture, a saved group, chosen Collectors, or nothing chosen. */
export function initialTargetMode(search: NewCaptureSearch): 'from' | 'group' | 'pick' {
  if (search.from) return 'from';
  if (search.group) return 'group';
  return 'pick';
}
