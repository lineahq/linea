import type { ArtifactMetadata } from '../api/types';
import { kindLabel } from './format';

/**
 * The Saved work collection: the kind filters (including Tests), search,
 * ordering, paging and the selection a suite runs from. Pure.
 */

export type KindFilter = 'all' | 'configuration' | 'scenario' | 'test' | 'fixture' | 'setup' | 'manifest';

export const FILTERS: { id: KindFilter; label: string }[] = [
  { id: 'all', label: 'All' },
  { id: 'configuration', label: 'Configurations' },
  { id: 'scenario', label: 'Scenarios' },
  { id: 'test', label: 'Tests' },
  { id: 'fixture', label: 'Fixtures' },
  { id: 'setup', label: 'Setups' },
  { id: 'manifest', label: 'Manifests' },
];

/** Rows per page of the collection. */
export const PAGE_SIZE = 25;

/** The most scenarios whose content is read to find the tests among them. */
export const PROFILE_LIMIT = 100;

/**
 * What reading an artifact's head revision tells: its origin text and, for a
 * scenario, how many assertions it has (undefined when unreadable).
 */
export interface Profile {
  origin: string;
  assertions?: number;
  /** The scenario text could not be read, so it has no known assertions. */
  unreadable?: boolean;
}

export type Profiles = ReadonlyMap<string, Profile>;

/** isTest: a test is a scenario with at least one assertion. */
export function isTest(item: ArtifactMetadata, profiles: Profiles): boolean {
  return item.kind === 'scenario' && (profiles.get(item.id)?.assertions ?? 0) > 0;
}

/** matchesFilter says whether an artifact belongs under a filter. */
export function matchesFilter(item: ArtifactMetadata, filter: KindFilter, profiles: Profiles): boolean {
  if (filter === 'all') return true;
  if (filter === 'test') return isTest(item, profiles);
  return item.kind === filter;
}

/**
 * The scenarios whose content is read: the newest `limit`. Older ones are
 * not checked for assertions, so they never count as tests.
 */
export function scenarioScope(items: ArtifactMetadata[], limit = PROFILE_LIMIT): ArtifactMetadata[] {
  return items.filter((m) => m.kind === 'scenario').sort(byUpdated).slice(0, limit);
}

/** unscopedScenarios counts the scenarios left out of the checked scope. */
export function unscopedScenarios(items: ArtifactMetadata[], limit = PROFILE_LIMIT): number {
  return Math.max(0, items.filter((m) => m.kind === 'scenario').length - limit);
}

/**
 * profilesComplete: whether every scenario in scope has been read. Until
 * then the Tests count is not shown, as it would only be a lower bound.
 */
export function profilesComplete(items: ArtifactMetadata[], profiles: Profiles, limit = PROFILE_LIMIT): boolean {
  return scenarioScope(items, limit).every((m) => {
    const p = profiles.get(m.id);
    return p !== undefined && (p.assertions !== undefined || p.unreadable === true);
  });
}

/** filterCounts counts each filter's artifacts; Tests is undefined until known. */
export function filterCounts(items: ArtifactMetadata[], profiles: Profiles): Record<KindFilter, number | undefined> {
  const out = {} as Record<KindFilter, number | undefined>;
  for (const f of FILTERS) out[f.id] = items.filter((m) => matchesFilter(m, f.id, profiles)).length;
  if (!profilesComplete(items, profiles)) out.test = undefined;
  return out;
}

/** byUpdated orders artifacts newest first, then by label. */
export function byUpdated(a: ArtifactMetadata, b: ArtifactMetadata): number {
  return b.updated.localeCompare(a.updated) || a.label.localeCompare(b.label) || a.id.localeCompare(b.id);
}

/** filterItems applies a kind filter and a search of the name or ID, newest first. */
export function filterItems(items: ArtifactMetadata[], filter: KindFilter, query: string, profiles: Profiles): ArtifactMetadata[] {
  const q = query.trim().toLowerCase();
  return items
    .filter((m) => matchesFilter(m, filter, profiles))
    .filter((m) => !q || m.label.toLowerCase().includes(q) || m.id.toLowerCase().includes(q) || kindLabel(m.kind).toLowerCase().includes(q))
    .sort(byUpdated);
}

export interface PageView<T> {
  rows: T[];
  /** The page shown (1-based), clamped to the pages that exist. */
  page: number;
  pages: number;
  /** 1-based position of the first and last row shown; 0 when empty. */
  from: number;
  to: number;
  total: number;
}

/** paginate cuts one page out of a list. */
export function paginate<T>(items: T[], page: number, size = PAGE_SIZE): PageView<T> {
  const pages = Math.max(1, Math.ceil(items.length / size));
  const p = Math.min(Math.max(1, Math.floor(page) || 1), pages);
  const start = (p - 1) * size;
  const rows = items.slice(start, start + size);
  return { rows, page: p, pages, from: rows.length ? start + 1 : 0, to: start + rows.length, total: items.length };
}

/** toggleSelected adds or removes an ID, returning a new set. */
export function toggleSelected(selected: ReadonlySet<string>, id: string): Set<string> {
  const next = new Set(selected);
  if (!next.delete(id)) next.add(id);
  return next;
}

/** setSelected adds or removes many IDs at once (select all on a page). */
export function setSelected(selected: ReadonlySet<string>, ids: string[], on: boolean): Set<string> {
  const next = new Set(selected);
  for (const id of ids) {
    if (on) next.add(id);
    else next.delete(id);
  }
  return next;
}

/** isRunnable: only scenarios can be members of a suite. */
export function isRunnable(item: ArtifactMetadata): boolean {
  return item.kind === 'scenario';
}

/**
 * selectedMembers returns the selected scenarios in list order, dropping
 * IDs that no longer exist or are not scenarios.
 */
export function selectedMembers(items: ArtifactMetadata[], selected: ReadonlySet<string>): ArtifactMetadata[] {
  return items.filter((m) => isRunnable(m) && selected.has(m.id)).sort(byUpdated);
}

/**
 * profileTargets chooses the artifacts whose head revision is read: every
 * scenario up to the limit (the Tests filter needs them all) and the rows
 * on the page shown (for their origin).
 */
export function profileTargets(items: ArtifactMetadata[], pageRows: ArtifactMetadata[], limit = PROFILE_LIMIT): ArtifactMetadata[] {
  const out = new Map<string, ArtifactMetadata>();
  let scenarios = 0;
  for (const m of [...items].sort(byUpdated)) {
    if (m.kind === 'scenario' && scenarios < limit) {
      out.set(m.id, m);
      scenarios++;
    }
  }
  for (const m of pageRows) out.set(m.id, m);
  return [...out.values()];
}
