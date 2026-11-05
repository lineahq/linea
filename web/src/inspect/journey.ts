// Record journeys: the report's lineage follows each injected record through
// the run (internal/debugger/analysis/lineage.go), and each data point an aggregating
// connector derived from them. These helpers place a journey on
// the pipeline graph and summarize where every record went.
import type { Event, RecordLineage } from '../api/types';
import { eventNodeKey } from '../runs/annotations';

export type Hop = NonNullable<RecordLineage['hops']>[number];

/** How a node took part in a journey, most significant last. */
export type PathStatus = 'passed' | 'changed' | 'counted' | 'delivered' | 'stopped';

const RANK: Record<PathStatus, number> = { passed: 0, changed: 1, counted: 2, delivered: 3, stopped: 4 };

export interface PathHighlight {
  nodes: Map<string, PathStatus>;
  /** "from->to" node key pairs the record crossed. */
  edges: Set<string>;
  /** Pipelines the record travelled in, to tell apart edges of shared nodes. */
  pipelines: Set<string>;
}

/** hopNodeKey is the graph node that observed a hop. */
export function hopNodeKey(hop: Hop, bySeq: Map<number, Event>): string | undefined {
  const e = bySeq.get(hop.seq);
  return e ? eventNodeKey(e) : undefined;
}

/** pathHighlight places a journey on the graph. */
export function pathHighlight(l: RecordLineage, bySeq: Map<number, Event>): PathHighlight {
  const nodes = new Map<string, PathStatus>();
  const edges = new Set<string>();
  const pipelines = new Set<string>();
  const hops = l.hops ?? [];
  const keys = hops.map((h) => hopNodeKey(h, bySeq));
  const mark = (key: string, s: PathStatus) => {
    const cur = nodes.get(key);
    if (!cur || RANK[s] > RANK[cur]) nodes.set(key, s);
  };
  hops.forEach((h, i) => {
    const key = keys[i];
    if (!key) return;
    if (h.pipeline) pipelines.add(h.pipeline);
    let s: PathStatus = 'passed';
    if (h.end === 'delivered') s = 'delivered';
    else if (h.end === 'aggregated') s = 'counted';
    else if (h.end) s = 'stopped';
    else if (h.changes?.length) s = 'changed';
    mark(key, s);
    const from = h.parent >= 0 ? keys[h.parent] : undefined;
    if (from && from !== key) edges.add(`${from}->${key}`);
  });
  return { nodes, edges, pipelines };
}

/** findLineage finds the journey of the record entity in the payload of event seq. */
export function findLineage(lineage: RecordLineage[] | undefined, seq: number, entity: string): RecordLineage | undefined {
  const span = entity.startsWith('span:');
  return lineage?.find((l) => (span ? l.span === entity : true) &&
    (l.hops?.some((h) => h.seq === seq && (span || h.ref === entity)) || l.sent?.some((s) => s.seq === seq && (span || s.ref === entity))));
}

/** hopEntity names the record of a hop the way the stage views name it. */
export function hopEntity(l: RecordLineage, hop: Hop): string {
  return l.span ?? hop.ref;
}

export type Cell = 'delivered' | 'changed';

export interface RouteRow {
  id: string;
  name: string;
  /** Where the record came from: its receiver, or the connector that emitted it. */
  receiver: string;
  /** "aggregated" for a data point an aggregating connector emitted. */
  origin?: string;
  signal: string;
  cells: Record<string, Cell>;
  /** Where paths ended without reaching an exporter. */
  stops: { component: string; end: string }[];
}

/** routeMatrix lists every record with the exporters it reached and where it stopped. */
export function routeMatrix(lineage: RecordLineage[] | undefined): { exporters: string[]; rows: RouteRow[] } {
  const exporters = new Set<string>();
  const rows = (lineage ?? []).map((l) => {
    const cells: Record<string, Cell> = {};
    for (const d of l.deliveries ?? []) {
      exporters.add(d.exporter);
      if (cells[d.exporter] !== 'changed') cells[d.exporter] = d.changed ? 'changed' : 'delivered';
    }
    return {
      id: l.id,
      name: l.name,
      receiver: l.receiver ?? l.connector ?? '',
      origin: l.origin,
      signal: l.signal,
      cells,
      stops: (l.stops ?? []).map((s) => ({ component: s.component ?? '', end: s.end })),
    };
  });
  return { exporters: [...exporters].sort(), rows };
}

/** HopNode is a hop with the hops that continued from it, for display as a tree. */
export interface HopNode {
  index: number;
  hop: Hop;
  children: HopNode[];
}

/** hopTree arranges hops by parent. A processor's input and output are one step. */
export function hopTree(l: RecordLineage): HopNode[] {
  const hops = l.hops ?? [];
  const nodes = hops.map((hop, index): HopNode => ({ index, hop, children: [] }));
  const roots: HopNode[] = [];
  nodes.forEach((n) => {
    if (n.hop.parent >= 0 && nodes[n.hop.parent]) nodes[n.hop.parent].children.push(n);
    else roots.push(n);
  });
  return roots;
}

/**
 * confidenceLabel words how a stage output was matched to the record it
 * received: by span ID, as the same record object (exact, never called out),
 * or by content with its confidence.
 */
export function confidenceLabel(confidence: string): string {
  if (confidence === 'id') return 'same span ID';
  if (confidence === 'object') return 'same record';
  return `${confidence} match`;
}

/** endMark marks a path's end: counted into data points is not a stop like the others. */
export const endMark = (end: string) => (end === 'aggregated' ? 'Σ' : '✕');

export const END_LABELS: Record<string, string> = {
  delivered: 'delivered',
  dropped: 'dropped',
  not_routed: 'not routed',
  unresolved: 'lost track',
  unobserved: 'unobserved',
  aggregated: 'counted',
  uncounted: 'not counted',
};
