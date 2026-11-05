import type { Graph, TopologyComponent } from '../api/types';

/**
 * A node of the displayed graph. Receivers, exporters and connectors are
 * shared between pipelines; processors are drawn once per pipeline because
 * the Collector builds one processor instance per pipeline (and the debugger
 * observes them separately).
 */
export interface LayoutNode {
  key: string;
  component: TopologyComponent;
  /** Set for processor instances. */
  pipeline?: string;
  pipelines: string[];
  layer: number;
  x: number;
  y: number;
}

export interface LayoutEdge {
  key: string;
  from: string;
  to: string;
  pipeline: string;
  signal: string;
}

export interface Layout {
  nodes: LayoutNode[];
  edges: LayoutEdge[];
  width: number;
  height: number;
}

export const COLUMN = 230;
export const ROW = 110;

/** nodeKey identifies a displayed node: processors per pipeline. */
export function nodeKey(ref: string, kind: string, pipeline: string): string {
  return kind === 'processor' ? `${pipeline}|${ref}` : ref;
}

/**
 * layoutGraph places nodes in columns by longest path from the receivers and
 * in rows by the pipelines they belong to, so each pipeline reads left to
 * right on its own lane and connectors sit between the pipelines they join.
 */
export function layoutGraph(g: Graph): Layout {
  const byRef = new Map((g.components ?? []).map((c) => [c.ref, c]));
  const pipelines = (g.pipelines ?? []).map((p) => p.id);
  const lane = new Map(pipelines.map((p, i) => [p, i]));
  const nodes = new Map<string, LayoutNode>();
  const add = (ref: string, pipeline: string): string | undefined => {
    const c = byRef.get(ref);
    if (!c) return undefined;
    const key = nodeKey(ref, c.kind, pipeline);
    let n = nodes.get(key);
    if (!n) {
      n = { key, component: c, pipeline: c.kind === 'processor' ? pipeline : undefined, pipelines: [], layer: 0, x: 0, y: 0 };
      nodes.set(key, n);
    }
    if (!n.pipelines.includes(pipeline)) n.pipelines.push(pipeline);
    return key;
  };

  const edges: LayoutEdge[] = [];
  for (const e of g.edges ?? []) {
    const from = add(e.from, e.pipeline);
    const to = add(e.to, e.pipeline);
    if (from && to) edges.push({ key: `${e.pipeline}|${from}->${to}`, from, to, pipeline: e.pipeline, signal: e.signal });
  }
  // Components of a pipeline without edges (none expected) and unused ones.
  for (const c of g.components ?? []) {
    if (c.kind === 'extension') continue;
    for (const p of c.pipelines ?? []) add(c.ref, p);
  }
  // Extensions that run (custom ones) sit in a row of their own
  // below the pipelines; the inert ones do nothing worth a node.
  const extensions = (g.components ?? []).filter((c) => c.kind === 'extension' && runs(c));

  // Longest-path layering; the graph is a DAG (the Collector rejects cycles).
  const incoming = new Map<string, string[]>();
  for (const e of edges) incoming.set(e.to, [...(incoming.get(e.to) ?? []), e.from]);
  const memo = new Map<string, number>();
  const layerOf = (key: string, seen: Set<string>): number => {
    const known = memo.get(key);
    if (known !== undefined) return known;
    if (seen.has(key)) return 0;
    seen.add(key);
    const l = Math.max(-1, ...(incoming.get(key) ?? []).map((k) => layerOf(k, seen))) + 1;
    memo.set(key, l);
    return l;
  };
  for (const n of nodes.values()) n.layer = layerOf(n.key, new Set());

  // Rows: the mean lane of the node's pipelines, then pushed apart within a
  // column so nodes never overlap.
  const columns = new Map<number, LayoutNode[]>();
  for (const n of nodes.values()) columns.set(n.layer, [...(columns.get(n.layer) ?? []), n]);
  let maxRow = 0;
  for (const [layer, col] of columns) {
    const desired = (n: LayoutNode) => n.pipelines.reduce((s, p) => s + (lane.get(p) ?? 0), 0) / Math.max(1, n.pipelines.length);
    col.sort((a, b) => desired(a) - desired(b) || a.key.localeCompare(b.key));
    let next = 0;
    for (const n of col) {
      const row = Math.max(desired(n), next);
      n.x = layer * COLUMN;
      n.y = row * ROW;
      next = row + 1;
      maxRow = Math.max(maxRow, row);
    }
  }
  const maxLayer = Math.max(0, ...columns.keys(), extensions.length - 1);
  if (extensions.length) {
    const row = nodes.size ? maxRow + 1 : 0;
    extensions.forEach((c, i) => nodes.set(c.ref, { key: c.ref, component: c, pipelines: [], layer: i, x: i * COLUMN, y: row * ROW }));
    maxRow = row;
  }
  return {
    nodes: [...nodes.values()].sort((a, b) => a.layer - b.layer || a.y - b.y),
    edges,
    width: (maxLayer + 1) * COLUMN,
    height: (maxRow + 1) * ROW,
  };
}

/** runs says whether an extension starts in a run: a custom one, alone or against simulated endpoints. */
function runs(c: TopologyComponent): boolean {
  return c.mode === 'real' || c.mode === 'dependency_mocked';
}
