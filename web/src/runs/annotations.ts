import type { Event, Stage } from '../api/types';
import { nodeKey, type LayoutEdge } from '../graph/layout';

/** NodeStats annotate a graph node with what the run observed there. */
export interface NodeStats {
  /** Items received: injected for receivers, consumed for processors, captured for exporters. */
  itemsIn?: number;
  /** Items a processor emitted. */
  itemsOut?: number;
  /** Records the stage analysis found removed (known once the run is done). */
  dropped?: number;
  errors?: number;
  /** Most recent node of a live run. */
  active?: boolean;
  /** Items a connector delivered, per pipeline it delivered into. */
  routes?: Record<string, number>;
  /** Items the component's simulated endpoints received. */
  sent?: number;
  /** Entries the component logged. */
  logs?: number;
}

export interface EdgeStats {
  items: number;
  active?: boolean;
}

/** eventNodeKey maps an event to the graph node that observed it. */
export function eventNodeKey(e: Event): string | undefined {
  if (!e.component) return undefined;
  if (e.component_kind === 'extension') return nodeKey(`extension/${e.component}`, 'extension', '');
  switch (e.kind) {
    case 'component.log':
    case 'endpoint.received':
      // What a component logged or sent: the node of its kind.
      if (e.component_kind === 'processor') return e.pipeline ? nodeKey(`processor/${e.component}`, 'processor', e.pipeline) : undefined;
      return e.component_kind ? nodeKey(`${e.component_kind}/${e.component}`, e.component_kind, '') : undefined;
    case 'input.injected':
      return nodeKey(`receiver/${e.component}`, 'receiver', '');
    case 'exporter.captured':
      return nodeKey(`exporter/${e.component}`, 'exporter', '');
    case 'stage.input':
    case 'stage.output':
    case 'stage.error':
    case 'stage.decision':
    case 'component.started':
    case 'component.stopped':
      // A connector is one node shared by the pipelines it joins; the
      // pipeline of its stage.output is the one it delivered into.
      if (e.source === 'connector_wrapper') return nodeKey(`connector/${e.component}`, 'connector', '');
      return e.pipeline ? nodeKey(`processor/${e.component}`, 'processor', e.pipeline) : undefined;
  }
  return undefined;
}

/** stageNodeKey is the graph node of a stage: a processor in a pipeline, or a connector. */
export function stageNodeKey(s: Stage): string {
  if (s.kind === 'connector') return nodeKey(`connector/${s.component}`, 'connector', '');
  return nodeKey(`processor/${s.component}`, 'processor', s.pipeline);
}

/**
 * nodeStats aggregates events per graph node. It works on a live event
 * stream as well as on a finished report; stages, once available, add the
 * records each stage removed and errors that analysis attributed to inputs.
 */
export function nodeStats(events: Event[], stages?: Stage[], live = false): Map<string, NodeStats> {
  const out = new Map<string, NodeStats>();
  const at = (key: string) => {
    let s = out.get(key);
    if (!s) {
      s = {};
      out.set(key, s);
    }
    return s;
  };
  let lastKey: string | undefined;
  for (const e of events) {
    const key = eventNodeKey(e);
    if (!key) continue;
    const items = e.counts?.items ?? 0;
    const s = at(key);
    switch (e.kind) {
      case 'input.injected':
      case 'stage.input':
      case 'exporter.captured':
        s.itemsIn = (s.itemsIn ?? 0) + items;
        lastKey = key;
        break;
      case 'stage.output':
        s.itemsOut = (s.itemsOut ?? 0) + items;
        if (e.source === 'connector_wrapper' && e.pipeline) {
          s.routes = { ...s.routes, [e.pipeline]: (s.routes?.[e.pipeline] ?? 0) + items };
        }
        lastKey = key;
        break;
      case 'stage.error':
        s.errors = (s.errors ?? 0) + 1;
        lastKey = key;
        break;
      case 'endpoint.received':
        s.sent = (s.sent ?? 0) + items;
        break;
      case 'component.log':
        s.logs = (s.logs ?? 0) + 1;
        break;
    }
  }
  for (const st of stages ?? []) {
    const s = at(stageNodeKey(st));
    const removed = st.inputs?.reduce((n, i) => n + (i.diff?.removed?.length ?? 0), 0) ?? 0;
    if (removed) s.dropped = removed;
  }
  if (live && lastKey) at(lastKey).active = true;
  return out;
}

/**
 * edgeStats labels an edge with the items that crossed it: what a connector
 * delivered into the edge's pipeline, what the source processor emitted, or
 * else what the target processor consumed. Other edges between shared
 * components carry no per-pipeline count and stay unlabelled.
 */
export function edgeStats(edges: LayoutEdge[], nodes: Map<string, NodeStats>): Map<string, EdgeStats> {
  const out = new Map<string, EdgeStats>();
  for (const e of edges) {
    const from = nodes.get(e.from);
    const to = nodes.get(e.to);
    const items = from?.routes
      ? (from.routes[e.pipeline] ?? 0)
      : e.from.includes('|')
        ? from?.itemsOut
        : e.to.includes('|')
          ? to?.itemsIn
          : undefined;
    if (items !== undefined) out.set(e.key, { items, active: !!(from?.active || to?.active) });
  }
  return out;
}
