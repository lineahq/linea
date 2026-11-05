import type { RuntimePruneRequest, RuntimeStorage } from '../../api/types';
import { formatSize, type PruneScope } from '../runtimes';

/**
 * The view model of the Storage page: what the runtimes take on Docker's disk
 * and what a clean-up would remove. Clean-up covers replaced and stale runtime
 * images, the build caches, and the stopped build and worker containers that
 * every prune removes. Runs and captures are not touched by it.
 */

export type CleanupId = 'replaced' | 'stale' | 'caches' | 'containers';

/** CleanupSelection is which optional items a clean-up includes. */
export interface CleanupSelection {
  replaced: boolean;
  stale: boolean;
  caches: boolean;
}

/** The unused runtime images are offered already ticked; the rest are chosen. */
export const DEFAULT_SELECTION: CleanupSelection = { replaced: true, stale: false, caches: false };

export interface CleanupRow {
  id: CleanupId;
  title: string;
  why: string;
  /** Bytes freed; undefined when Docker does not size them (containers). */
  frees?: number;
  count: number;
  /** A stopped container goes with every clean-up, so it cannot be left out. */
  fixed: boolean;
}

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;
const size = (n: number) => Math.max(n, 0);

/**
 * cleanupRows lists what can be removed, never an image a run uses and never
 * a running container. A kind with nothing to remove has no row.
 */
export function cleanupRows(st: RuntimeStorage): CleanupRow[] {
  const images = st.runtimes ?? [];
  const replaced = images.filter((r) => r.state === 'replaced' && !r.in_use);
  const stale = images.filter((r) => r.state === 'stale' && !r.in_use);
  const caches = st.caches ?? [];
  const stopped = (st.leftovers ?? []).filter((c) => !c.running);
  const rows: CleanupRow[] = [];
  if (replaced.length) {
    rows.push({ id: 'replaced', title: plural(replaced.length, 'unused runtime image'), why: 'a rebuild took their names; no runtime points at them', frees: replaced.reduce((n, r) => n + size(r.size), 0), count: replaced.length, fixed: false });
  }
  if (stale.length) {
    rows.push({ id: 'stale', title: plural(stale.length, 'stale runtime image'), why: 'built for another worker protocol; this debugger cannot run them', frees: stale.reduce((n, r) => n + size(r.size), 0), count: stale.length, fixed: false });
  }
  if (caches.length) {
    rows.push({ id: 'caches', title: caches.length === 1 ? 'Build cache' : `${caches.length} build caches`, why: 'rebuilt on the next build, which is slower', frees: caches.reduce((n, v) => n + size(v.size), 0), count: caches.length, fixed: false });
  }
  if (stopped.length) {
    rows.push({ id: 'containers', title: plural(stopped.length, 'stopped build or worker container'), why: 'removed with every clean-up', count: stopped.length, fixed: true });
  }
  return rows;
}

/** cleanupScope totals the selected rows; stopped containers are always part of it. */
export function cleanupScope(rows: CleanupRow[], sel: CleanupSelection): PruneScope {
  const by = (id: CleanupId) => rows.find((r) => r.id === id);
  const replaced = sel.replaced ? by('replaced') : undefined;
  const stale = sel.stale ? by('stale') : undefined;
  const caches = sel.caches ? by('caches') : undefined;
  const containers = by('containers');
  const images = (replaced?.count ?? 0) + (stale?.count ?? 0);
  const bytes = (replaced?.frees ?? 0) + (stale?.frees ?? 0) + (caches?.frees ?? 0);
  return {
    images, bytes, containers: containers?.count ?? 0, caches: caches?.count ?? 0,
    empty: images + (caches?.count ?? 0) + (containers?.count ?? 0) === 0,
  };
}

/** pruneRequest is the request for a selection. Replaced images are removed only when ticked. */
export function pruneRequest(sel: CleanupSelection): RuntimePruneRequest {
  return { replaced: sel.replaced, stale: sel.stale, caches: sel.caches };
}

export interface DiskSegment {
  id: 'images' | 'caches';
  label: string;
  bytes: number;
  /** Share of what the runtimes use, as a percentage with one decimal. */
  percent: number;
}

export interface DiskUse {
  total: number;
  segments: DiskSegment[];
  /** Words the meter for assistive technology. */
  summary: string;
}

/**
 * diskUse splits what the runtimes use into images and build caches. There is
 * no limit to measure it against: the API reports sizes, not a quota.
 */
export function diskUse(st: RuntimeStorage): DiskUse {
  const images = (st.runtimes ?? []).reduce((n, r) => n + size(r.size), 0);
  const caches = (st.caches ?? []).reduce((n, v) => n + size(v.size), 0);
  const total = images + caches;
  const pct = (n: number) => (total ? Math.round((n / total) * 1000) / 10 : 0);
  const segments: DiskSegment[] = [
    { id: 'images', label: 'Runtime images', bytes: images, percent: pct(images) },
    { id: 'caches', label: 'Build caches', bytes: caches, percent: pct(caches) },
  ];
  return { total, segments, summary: segments.map((s) => `${s.label} ${formatSize(s.bytes)}`).join(', ') };
}
