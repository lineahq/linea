// Aggregations: a connector such as count turns the records it consumes
// into data points of another signal (internal/debugger/aggregation). These helpers
// relate the report's aggregations to the record journeys on both sides.
import type { Aggregation, RecordLineage } from '../api/types';

/** contains mirrors analysis.Contains: a span covers its events, a metric its data points. */
export function contains(aggregated: string, record: string): boolean {
  return aggregated === record || aggregated.startsWith(record + '.') || record.startsWith(aggregated + '.');
}

/** A record of the connector's input, with its journey when there is one. */
export interface CountedRecord {
  ref: string;
  name: string;
  /** Lineage ID of the record's journey. */
  lineage?: string;
}

export interface PointRow {
  ref: string;
  metric: string;
  attributes: [string, string][];
  value: number;
  records: CountedRecord[];
  /** Lineage ID of the data point's own journey (verified aggregations only). */
  lineage?: string;
}

/** aggregatedID mirrors the lineage ID of a derived data point. */
export const aggregatedID = (a: Aggregation, ref: string) => `${a.output}:${ref}`;

/**
 * aggregationRows lists an aggregation's data points with the records each
 * counted, named by their journeys, and the records nothing counted.
 */
export function aggregationRows(a: Aggregation, lineage?: RecordLineage[]): { points: PointRow[]; uncounted: CountedRecord[] } {
  const ids = new Set((lineage ?? []).map((l) => l.id));
  // Journeys that pass the connector's input, with their record's position there.
  const at = (lineage ?? []).flatMap((l) => (l.hops ?? []).filter((h) => h.seq === a.input).map((h) => ({ l, ref: h.ref })));
  const record = (ref: string): CountedRecord => {
    const m = at.find((x) => contains(ref, x.ref));
    if (!m) return { ref, name: ref };
    // A count of span events or data points names the part it counted.
    const part = ref.length > m.ref.length ? ` · ${ref.slice(m.ref.length + 1)}` : '';
    return { ref, name: (m.l.name || m.l.id) + part, lineage: m.l.id };
  };
  const points = (a.points ?? []).map((p) => {
    const id = aggregatedID(a, p.ref);
    return {
      ref: p.ref,
      metric: p.metric,
      attributes: Object.entries(p.attributes ?? {}).sort(([x], [y]) => (x < y ? -1 : x > y ? 1 : 0)),
      value: p.value,
      records: (p.records ?? []).map(record),
      lineage: ids.has(id) ? id : undefined,
    };
  });
  return { points, uncounted: (a.uncounted ?? []).map(record) };
}

export const VERIFICATION: Record<string, [string, string]> = {
  replay_verified: [
    'verified',
    'Replaying the configuration on the recorded input produced exactly the emitted data points, so each point lists the records it counted.',
  ],
  replay_differs: ['differs', 'The replay does not reproduce the emitted data points, so no records are attributed to them.'],
  unsupported: ['not modelled', 'The configuration uses something the replay does not model, so no records are attributed.'],
};
