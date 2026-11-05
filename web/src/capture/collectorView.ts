import type { IconName } from '../ui/Icon';
import type { Collector } from '../api/types';
import type { PhaseTone } from './phase';

/**
 * Pure view logic for the Collectors list, the Groups members table and the
 * "not set up" state. Components stay thin around these.
 */

export type SetupState = 'loading' | 'not-configured' | 'waiting' | 'ready';

/**
 * setupState decides whether Live capture is usable. Capture is not set up
 * when the debugger has no fleet connection; it is waiting when connected but
 * no Collector reports through fleet's livecapture plugin.
 */
export function setupState(page: { enabled: boolean; items?: readonly unknown[]; next_cursor?: string } | undefined): SetupState {
  if (!page) return 'loading';
  if (!page.enabled) return 'not-configured';
  if (!(page.items?.length ?? 0) && !page.next_cursor) return 'waiting';
  return 'ready';
}

/**
 * Fleet lists only connected Collectors, so a listed Collector is either
 * ready or has capture switched off in its extension.
 */
export type CollectorStatus = 'connected' | 'capture-disabled';

export function collectorStatus(c: Pick<Collector, 'enabled'>): CollectorStatus {
  return c.enabled ? 'connected' : 'capture-disabled';
}

export const statusLabels: Record<CollectorStatus, string> = {
  connected: 'Connected',
  'capture-disabled': 'Capture disabled',
};

interface Chip {
  label: string;
  tone: PhaseTone;
  icon: IconName;
}

/** connectionChip is the Status column's chip. */
export function connectionChip(c: Pick<Collector, 'enabled'>): Chip {
  return collectorStatus(c) === 'connected' ? { label: 'Connected', tone: 'pass', icon: 'circle-check' } : { label: 'Capture disabled', tone: 'plain', icon: 'circle-minus' };
}

/** availabilityChip says whether a Collector can be captured right now. */
export function availabilityChip(c: Pick<Collector, 'enabled'>): Chip {
  return collectorStatus(c) === 'connected' ? { label: 'Available', tone: 'pass', icon: 'circle-check' } : { label: 'Capture not allowed', tone: 'warn', icon: 'triangle-alert' };
}

/** isCapturable is true for a connected Collector whose extension allows capture. */
export function isCapturable(c: Pick<Collector, 'enabled'>): boolean {
  return collectorStatus(c) === 'connected';
}

export interface CollectorFilter {
  search: string;
  cluster: string;
  group: string;
  status: string;
}

export const noFilter: CollectorFilter = { search: '', cluster: '', group: '', status: '' };

/** filterCollectors narrows a page of Collectors by search text and the three facets. */
export function filterCollectors(items: Collector[], f: CollectorFilter): Collector[] {
  const q = f.search.trim().toLowerCase();
  return items.filter((c) => {
    if (f.cluster && c.cluster !== f.cluster) return false;
    if (f.group && c.collector_group !== f.group) return false;
    if (f.status && collectorStatus(c) !== f.status) return false;
    if (!q) return true;
    return [c.pod, c.cluster, c.namespace, c.collector_group, c.collector_version, c.distribution_version, c.agent_id].some((v) => (v ?? '').toLowerCase().includes(q));
  });
}

/** facetValues lists the distinct non-empty values of a field, sorted. */
export function facetValues(items: Collector[], field: 'cluster' | 'collector_group'): string[] {
  return [...new Set(items.map((c) => c[field] ?? '').filter(Boolean))].sort((a, b) => a.localeCompare(b));
}

/** countBadge is the sub-nav count: exact when the whole list is loaded, otherwise "N+". */
export function countBadge(shown: number | undefined, more: boolean): string | undefined {
  if (shown === undefined) return undefined;
  return more ? `${shown}+` : String(shown);
}

/** locationText joins cluster and namespace for compact lines. */
export function locationText(c: Pick<Collector, 'cluster' | 'namespace'>): string {
  return [c.cluster, c.namespace].filter(Boolean).join(' / ') || 'Local';
}

/** versionText is the Collector version, with the distribution when it adds information. */
export function versionText(c: Pick<Collector, 'collector_version' | 'distribution_version'>): string {
  const version = c.collector_version || 'Unknown';
  return c.distribution_version && c.distribution_version !== c.collector_version ? `${version} · ${c.distribution_version}` : version;
}

/** The collectors the Configure capture action carries to New capture. */
export function selectionTarget(ids: Iterable<string>): { collectors: string[] } | undefined {
  const list = [...new Set(ids)].slice(0, 16);
  return list.length ? { collectors: list } : undefined;
}

/** The ceiling on Collectors per capture, with the message for going over it. */
export const SELECTION_LIMIT = 16;
