// The Records tab: one row per record journey of the run, with the sentence
// that says where it ended. "No observed output" is a fate of its own and is
// never worded as a drop: only a stage that recorded removing the record
// says "dropped".
import type { Event, RecordLineage } from '../api/types';
import type { IconName } from '../ui/Icon';
import { END_LABELS, hopNodeKey } from '../inspect/journey';

export type Fate = 'dropped' | 'delivered' | 'counted' | 'nooutput';

export interface RecordRow {
  id: string;
  name: string;
  signal: string;
  /** A data point an aggregating connector emitted (a Σ row). */
  aggregated: boolean;
  fate: Fate;
  /** The row's sentence: "dropped at filter/pager", "delivered to otlp/archive". */
  text: string;
  /** Exporters the record reached. */
  deliveredTo: string[];
  /** Text searched by the filter box. */
  haystack: string;
}

export const FATE_ICONS: Record<Fate, IconName> = { dropped: 'ban', delivered: 'circle-check', counted: 'layers', nooutput: 'circle-dashed' };

const ENDS: Record<string, string> = { ...END_LABELS, unobserved: 'no observed output' };
const where = (s: { end: string; component?: string }) => `${ENDS[s.end] ?? s.end}${s.component ? ` at ${s.component}` : ''}`;

/** fateOf classifies a journey: reaching an exporter wins, then a recorded drop, then being counted. */
export function fateOf(l: RecordLineage): Fate {
  if (l.deliveries?.length) return 'delivered';
  const stops = l.stops ?? [];
  if (stops.some((s) => s.end === 'dropped')) return 'dropped';
  if (stops.some((s) => s.end === 'aggregated')) return 'counted';
  return 'nooutput';
}

/** fateText words where a record ended, without claiming more than the stops record. */
export function fateText(l: RecordLineage): string {
  const delivered = [...new Set((l.deliveries ?? []).map((d) => d.exporter))];
  const stops = (l.stops ?? []).filter((s) => s.end !== 'delivered');
  const parts: string[] = [];
  if (delivered.length) parts.push(`delivered to ${delivered.join(', ')}`);
  if (stops.length) parts.push(stops.map(where).join(', '));
  return parts.join('; ') || 'no observed output';
}

export function recordRows(lineage: RecordLineage[] | undefined): RecordRow[] {
  return (lineage ?? []).map((l) => {
    const fate = fateOf(l);
    const text = fateText(l);
    return {
      id: l.id,
      name: l.name || l.id,
      signal: l.signal,
      aggregated: l.origin === 'aggregated',
      fate,
      text,
      deliveredTo: [...new Set((l.deliveries ?? []).map((d) => d.exporter))],
      haystack: `${l.name} ${l.id} ${l.signal} ${l.receiver ?? ''} ${l.connector ?? ''} ${text}`.toLowerCase(),
    };
  });
}

export type RecordFilter = 'all' | Fate;

export const FILTER_LABELS: Record<RecordFilter, string> = {
  all: 'All',
  dropped: 'Dropped',
  delivered: 'Delivered',
  counted: 'Counted',
  nooutput: 'No observed output',
};

/** filterCounts counts rows per filter; a filter with no rows is dropped, except All and the three core fates. */
export function filterCounts(rows: RecordRow[]): { filter: RecordFilter; count: number }[] {
  const n = (f: Fate) => rows.filter((r) => r.fate === f).length;
  const out: { filter: RecordFilter; count: number }[] = [
    { filter: 'all', count: rows.length },
    { filter: 'dropped', count: n('dropped') },
    { filter: 'delivered', count: n('delivered') },
  ];
  if (n('counted')) out.push({ filter: 'counted', count: n('counted') });
  out.push({ filter: 'nooutput', count: n('nooutput') });
  return out;
}

/** filterRows applies a fate filter and a free-text query (every word must appear). */
export function filterRows(rows: RecordRow[], filter: RecordFilter, query: string): RecordRow[] {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  return rows.filter((r) => (filter === 'all' || r.fate === filter) && words.every((w) => r.haystack.includes(w)));
}

/**
 * stopNode is the graph node where a journey ended: the stage that dropped
 * it, else another recorded stop. Undefined when the journey did not stop
 * (it was delivered) or the stop's event is not in the run.
 */
export function stopNode(l: RecordLineage, events: Event[]): string | undefined {
  const stops = l.stops ?? [];
  const stop = stops.find((s) => s.end === 'dropped') ?? stops[0];
  const hop = stop && l.hops?.[stop.hop];
  if (!hop) return undefined;
  return hopNodeKey(hop, new Map(events.map((e) => [e.seq, e])));
}
