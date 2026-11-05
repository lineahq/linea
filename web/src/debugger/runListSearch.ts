// The run list's filters in the URL (`/runs?scenario=&runtime=&result=&mode=&q=`),
// so a filtered list is linkable and survives a reload.
import { NO_FILTER, RESULTS, type RunFilter } from './runFacts';

/** RunListSearch is the run list route's search: the filters, each present only when set. */
export interface RunListSearch {
  scenario?: string;
  runtime?: string;
  /** One of `RESULTS`; an unknown value is dropped, since the server refuses it. */
  result?: string;
  mode?: string;
  /** The in-browser search words. */
  q?: string;
}

/** text reads a search value as a string: the router turns "42" into a number. */
function text(v: unknown): string | undefined {
  if (typeof v === 'string') return v || undefined;
  if (typeof v === 'number' && Number.isFinite(v)) return String(v);
  return undefined;
}

/** parseRunListSearch is the route's validateSearch. */
export function parseRunListSearch(s: Record<string, unknown>): RunListSearch {
  const result = text(s.result);
  return {
    scenario: text(s.scenario),
    runtime: text(s.runtime),
    result: RESULTS.some(([v]) => v === result) ? result : undefined,
    mode: text(s.mode),
    q: text(s.q),
  };
}

/** filterFromSearch is the filter a URL names. */
export function filterFromSearch(s: RunListSearch): RunFilter {
  const p = parseRunListSearch(s as Record<string, unknown>);
  return { ...NO_FILTER, scenario: p.scenario ?? '', runtime: p.runtime ?? '', result: p.result ?? '', mode: p.mode ?? '', query: p.q ?? '' };
}

/** searchFromFilter is the URL search of a filter; an unset filter leaves no key. */
export function searchFromFilter(f: RunFilter): RunListSearch {
  return {
    scenario: f.scenario || undefined,
    runtime: f.runtime || undefined,
    result: f.result || undefined,
    mode: f.mode || undefined,
    q: f.query || undefined,
  };
}
