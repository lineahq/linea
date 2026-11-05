import { api } from '../api/client';
import type { Collector, CapturePointPage, CaptureSelection } from '../api/types';
import { selectionForPoint } from './selection';

export type CollectorGrouping = 'cluster' | 'namespace' | 'collector_group' | 'none';
export const groupingLabels = { cluster: 'Cluster', namespace: 'Namespace', collector_group: 'Collector group', none: 'No grouping' } as const;
type Point = NonNullable<CapturePointPage['items']>[number];
export type GroupCaptureTargets = { label: string; collectors: Collector[]; points: Point[] };

/** One capture reaches at most this many Collectors; fleet refuses more. */
const MAX_TARGETS = 16;
/** A fleet group is reviewed only up to this many connected Collectors. */
const MAX_GROUP_REVIEW = 64;

// Resolve every inventory page before freezing targets. Search and the visible
// page never narrow a whole-group capture. Errors refuse a partial roster.
export async function resolveGroupCapture(field: Exclude<CollectorGrouping, 'none'>, value: string): Promise<GroupCaptureTargets> {
  const members = new Map<string, Collector>(); const seen = new Set<string>();
  let cursor = ''; let count = 0;
  do {
    if (seen.has(cursor)) throw new Error('Inventory paging repeated. Select the group again.');
    seen.add(cursor);
    const page = await api.collectors(cursor);
    if (!page.enabled) throw new Error('Live capture has no fleet connection.');
    count += page.items?.length ?? 0;
    if (count > 1024 || seen.size > 1024) throw new Error('Inventory exceeds the review limit. Select a narrower group.');
    for (const item of page.items ?? []) if ((item[field] || '') === value) members.set(item.id, item);
    if (members.size > MAX_TARGETS) throw new Error('This group has more than 16 collectors. Choose a narrower metadata group; one capture supports up to 16.');
    cursor = page.next_cursor || '';
  } while (cursor);
  const collectors = [...members.values()].sort((a, b) => a.pod.localeCompare(b.pod) || a.id.localeCompare(b.id));
  if (!collectors.length) throw new Error('No collectors currently match this group. Select the group again.');
  return reviewGroupCollectors(`${groupingLabels[field]}: ${value || 'Not reported'}`, collectors);
}

export async function reviewGroupCollectors(label: string, collectors: Collector[]): Promise<GroupCaptureTargets> {
  if (!collectors.length) throw new Error('This group has no connected collectors.');
  if (collectors.length > MAX_TARGETS) throw new Error('One capture supports up to 16 collectors. Choose a narrower group.');
  const catalogs = await loadCollectorCatalogs(collectors);
  return { label, collectors, points: sharedPoints(catalogs) };
}

/** Every capture point each Collector reports, in the order of `collectors`. */
export async function loadCollectorCatalogs(collectors: Collector[]): Promise<Point[][]> {
  const catalogs: Point[][] = [];
  // At most 64 targets and 256 points per target; keep network work sequential.
  for (const collector of collectors) {
    const points: Point[] = []; const pages = new Set<string>(); let pointCursor = '';
    do {
      if (pages.has(pointCursor)) throw new Error('Capture-point paging repeated. Select the group again.');
      pages.add(pointCursor);
      const page = await api.capturePoints(collector.id, pointCursor);
      points.push(...page.items ?? []);
      if (points.length > 256 || pages.size > 256) throw new Error('A collector catalog exceeds the review limit.');
      pointCursor = page.next_cursor || '';
    } while (pointCursor);
    catalogs.push(points);
  }
  return catalogs;
}

function sharedPoints(catalogs: Point[][]): Point[] {
  const key = (point: Point) => JSON.stringify([point.pipeline, point.point, point.signal]);
  const common = new Map(catalogs[0].map(point => [key(point), { ...point }]));
  for (const catalog of catalogs.slice(1)) {
    const available = new Map(catalog.map(point => [key(point), point]));
    for (const [id, point] of common) {
      const other = available.get(id);
      if (!other) common.delete(id);
      else if (other.sensitivity === 'raw') point.sensitivity = 'raw';
    }
  }
  return [...common.values()];
}

export function selectionForGroup(group: GroupCaptureTargets): CaptureSelection {
  const defaults = selectionForPoint(group.collectors[0].id, { pipeline: 'logs', point: 'ingress', signal: 'logs', sensitivity: 'redacted' });
  return { ...defaults, collector_ids: group.collectors.map(item => item.id), capture_points: [], signals: [] };
}

export async function loadCollectorInventory(): Promise<Collector[]> {
  const items = new Map<string, Collector>(); const seen = new Set<string>(); let cursor = ''; let count = 0;
  do {
    if (seen.has(cursor)) throw new Error('Inventory paging repeated. Reload collectors.'); seen.add(cursor);
    const page = await api.collectors(cursor); count += page.items?.length ?? 0;
    if (count > 1024 || seen.size > 1024) throw new Error('Collector inventory exceeds the review limit.');
    for (const item of page.items ?? []) items.set(item.id, item);
    cursor = page.next_cursor || '';
  } while (cursor);
  return [...items.values()].sort((a, b) => a.pod.localeCompare(b.pod) || a.id.localeCompare(b.id));
}

/** A fleet group as a capture would see it now. */
export interface FleetGroupMembers {
  name: string;
  /** Connected Collectors of connected member agents. */
  collectors: Collector[];
  /** Member agents fleet lists as not connected. */
  offline: number;
}

/**
 * loadFleetGroupMembers reads a fleet group's members and the Collectors they
 * run. Fleet resolves the group again when the capture starts, so this is a
 * preview: members can change in between.
 */
export async function loadFleetGroupMembers(id: string, inventory?: Collector[]): Promise<FleetGroupMembers> {
  const group = await api.collectorGroup(id);
  const ids = new Set<string>(); const seen = new Set<string>(); let cursor = ''; let offline = 0;
  do {
    if (seen.has(cursor) || seen.size >= 1024) throw new Error('Group paging repeated. Review the group again.'); seen.add(cursor);
    const page = await api.collectorGroupMembers(id, cursor);
    for (const member of page.items ?? []) {
      if (!member.connected) offline++;
      else for (const collector of member.collector_ids ?? []) ids.add(collector);
    }
    if (ids.size > MAX_GROUP_REVIEW) throw new Error(`This group has more than ${MAX_GROUP_REVIEW} connected Collectors. One capture supports up to 16; choose a narrower group.`);
    cursor = page.next_cursor || '';
  } while (cursor);
  const known = inventory ?? (ids.size ? await loadCollectorInventory() : []);
  return { name: group.name, collectors: known.filter(c => ids.has(c.id)), offline };
}
