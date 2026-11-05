import type { Graph, TopologyComponent } from '../api/types';
import type { EdgeStats, NodeStats } from '../runs/annotations';
import type { PathHighlight } from '../inspect/journey';
import { nodeKey } from './layout';

// Lane-based pipeline graph model. One lane per pipeline, read left to right:
// receivers, processors, then exporters. The node keys are exactly the keys
// of layout.ts (`nodeKey`), and a link carries the layout edge keys it stands
// for, so `?node=`, nodeStats/edgeStats and the record journey keep working.

export type SignalFilter = 'all' | 'logs' | 'traces' | 'metrics';
export const SIGNAL_FILTERS: SignalFilter[] = ['all', 'logs', 'traces', 'metrics'];

/** ModeInfo describes how the debugger executes a component, as the legend explains it. */
export interface ModeInfo {
  /** The API's mode string. */
  id: string;
  /** Suffix of the `od-mode--*` class. */
  cls: 'real' | 'mocked' | 'captured' | 'simulated' | 'standin' | 'unsupported';
  label: string;
  meaning: string;
  icon: 'mode-real' | 'mode-mocked' | 'mode-captured' | 'mode-simulated' | 'mode-standin' | 'ban';
}

export const MODES: ModeInfo[] = [
  { id: 'real', cls: 'real', label: 'Real', icon: 'mode-real', meaning: 'Runs as the Collector would run it.' },
  {
    id: 'dependency_mocked',
    cls: 'mocked',
    label: 'Dependency mocked',
    icon: 'mode-mocked',
    meaning: 'Runs for real; the service it depends on is answered by a mock.',
  },
  {
    id: 'captured',
    cls: 'captured',
    label: 'Captured',
    icon: 'mode-captured',
    meaning: 'Replaced by local capture: what reaches it is recorded, never sent.',
  },
  {
    id: 'simulated',
    cls: 'simulated',
    label: 'Simulated',
    icon: 'mode-simulated',
    meaning: 'Not executed as such: fixtures are injected, or its endpoints are simulated.',
  },
  {
    id: 'stand_in',
    cls: 'standin',
    label: 'Stand-in',
    icon: 'mode-standin',
    meaning: 'Its type is not in this distribution; a stand-in component takes its place.',
  },
  {
    id: 'unsupported',
    cls: 'unsupported',
    label: 'Unsupported',
    icon: 'ban',
    meaning: 'The debugger does not execute this kind of component yet.',
  },
];

/** modeInfo resolves an API mode string; an unknown one is shown as unsupported under its own name. */
export function modeInfo(mode: string): ModeInfo {
  const known = MODES.find((m) => m.id === mode);
  if (known) return known;
  const unsupported = MODES[MODES.length - 1]!;
  return { ...unsupported, id: mode, label: mode.replace(/_/g, ' ') || unsupported.label };
}

export interface LaneItem {
  /** Same key as layout.ts: processors per pipeline, the rest by ref. */
  key: string;
  ref: string;
  id: string;
  type: string;
  kind: string;
  mode: ModeInfo;
  note?: string;
  /** The run's meta line; undefined before a run or without data. */
  meta?: string;
  errors: number;
  /** True for a connector shown at the start of a lane that consumes it. */
  handoff: boolean;
  /** Matches the search; always true while not searching. */
  match: boolean;
}

export interface LaneEdge {
  /** layout.ts edge key, `pipeline|from->to`. */
  key: string;
  from: string;
  to: string;
}

/** LaneLink joins two neighbouring columns of a lane. */
export interface LaneLink {
  pipeline: string;
  edges: LaneEdge[];
  /** Items that crossed it, when the run observed them. */
  count?: number;
  /** Nothing crossed although something entered the previous column. */
  dropped: boolean;
  active: boolean;
  /** Follows a column of connector hand-offs only. */
  short: boolean;
}

export interface Lane {
  id: string;
  signal: string;
  /** Columns left to right; items in a column are stacked. */
  columns: LaneItem[][];
  /** links[i] joins columns[i] and columns[i + 1]. */
  links: LaneLink[];
  match: boolean;
}

export interface LaneModel {
  lanes: Lane[];
  extensions: ExtensionItem[];
  /** Modes in use, in legend order. */
  modes: ModeInfo[];
  searching: boolean;
  /** Distinct node keys matching the search. */
  matches: number;
}

export interface ExtensionItem extends LaneItem {
  /** What the extension did in the run, e.g. "3 received". */
  summary?: string;
}

export interface LaneOptions {
  signal?: SignalFilter;
  query?: string;
  nodeStats?: Map<string, NodeStats>;
  edgeStats?: Map<string, EdgeStats>;
  /** "all" (default) shows every extension; "running" only those that start in a run (as layout.ts). */
  extensions?: 'all' | 'running';
}

/** routeLabel shortens a pipeline id to what follows its signal: "logs/pager" is "pager". */
export function routeLabel(pipeline: string): string {
  const i = pipeline.indexOf('/');
  return i > 0 && i < pipeline.length - 1 ? pipeline.slice(i + 1) : pipeline;
}

const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;

/**
 * nodeMeta builds the meta line of a node from what the run observed:
 * "in 5 · out 5" (receiver), "5 → 5 · dropped 2" (processor), "received 5"
 * (exporter), "2 → pager · 5 → archive" (connector). `consumers` are the
 * pipelines that take a connector's output, so a route that delivered
 * nothing still reads "0 → pager".
 */
export function nodeMeta(kind: string, s: NodeStats | undefined, consumers: string[] = []): string | undefined {
  if (!s) return undefined;
  const parts: string[] = [];
  const flow = (): string | undefined => {
    if (s.itemsOut !== undefined) return `${s.itemsIn ?? '–'} → ${s.itemsOut}`;
    if (s.itemsIn !== undefined) return `in ${s.itemsIn}`;
    return undefined;
  };
  switch (kind) {
    case 'receiver':
      if (s.itemsIn !== undefined) parts.push(s.itemsOut !== undefined ? `in ${s.itemsIn} · out ${s.itemsOut}` : `in ${s.itemsIn}`);
      break;
    case 'exporter':
      if (s.itemsIn !== undefined) parts.push(`received ${s.itemsIn}`);
      break;
    case 'connector': {
      const routes = s.routes;
      if (routes || (s.itemsIn !== undefined && consumers.length)) {
        const ids = consumers.length ? consumers : Object.keys(routes ?? {});
        const labels = ids.map((p) => `${routes?.[p] ?? 0} → ${routeLabel(p)}`);
        if (labels.length) {
          parts.push(labels.join(' · '));
          break;
        }
      }
      const f = flow();
      if (f) parts.push(f);
      break;
    }
    case 'extension': {
      if (s.sent !== undefined) parts.push(`${s.sent} received`);
      if (s.logs) parts.push(`${s.logs} logged`);
      break;
    }
    default: {
      const f = flow();
      if (f) parts.push(f);
      if (s.dropped) parts.push(`dropped ${s.dropped}`);
    }
  }
  if (s.errors) parts.push(plural(s.errors, 'error', 'errors'));
  return parts.length ? parts.join(' · ') : undefined;
}

/** matchesQuery is true when every word of the query occurs in one of the fields (case-insensitive). */
export function matchesQuery(query: string, fields: Array<string | undefined>): boolean {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (!words.length) return true;
  const hay = fields.filter(Boolean).join('\n').toLowerCase();
  return words.every((w) => hay.includes(w));
}

/** extensionRuns says whether an extension starts in a run (same rule as layout.ts). */
function extensionRuns(c: TopologyComponent): boolean {
  return c.mode === 'real' || c.mode === 'dependency_mocked';
}

/**
 * laneModel arranges a topology in lanes. Pure: stats, search and the signal
 * filter are applied here; selection and the followed record journey are
 * applied by the view (`linkOnPath`).
 */
export function laneModel(g: Graph, opts: LaneOptions = {}): LaneModel {
  const { signal = 'all', query = '', nodeStats, edgeStats } = opts;
  const searching = query.trim() !== '';
  const byRef = new Map((g.components ?? []).map((c) => [c.ref, c]));
  const pipelines = g.pipelines ?? [];
  const consumersOf = (ref: string) => pipelines.filter((p) => p.receivers?.includes(ref)).map((p) => p.id);

  // Edge keys as layout.ts builds them.
  const edgeKeys = new Set<string>();
  for (const e of g.edges ?? []) {
    const from = byRef.get(e.from);
    const to = byRef.get(e.to);
    if (from && to) edgeKeys.add(`${e.pipeline}|${nodeKey(e.from, from.kind, e.pipeline)}->${nodeKey(e.to, to.kind, e.pipeline)}`);
  }

  const item = (c: TopologyComponent, pipeline: string, handoff: boolean, laneMatch: boolean): LaneItem => {
    const key = nodeKey(c.ref, c.kind, pipeline);
    const stats = nodeStats?.get(key);
    const mode = modeInfo(c.mode);
    return {
      key,
      ref: c.ref,
      id: c.id,
      type: c.type,
      kind: c.kind,
      mode,
      note: c.note,
      meta: handoff ? undefined : nodeMeta(c.kind, stats, c.kind === 'connector' ? consumersOf(c.ref) : []),
      errors: stats?.errors ?? 0,
      handoff,
      match: !searching || laneMatch || matchesQuery(query, [c.id, c.type, c.ref, c.kind, mode.label]),
    };
  };

  const lanes: Lane[] = [];
  const matched = new Set<string>();
  for (const p of pipelines) {
    if (signal !== 'all' && p.signal !== signal) continue;
    const laneMatch = searching && matchesQuery(query, [p.id]);
    const columns: LaneItem[][] = [];
    const first: LaneItem[] = [];
    for (const ref of p.receivers ?? []) {
      const c = byRef.get(ref);
      if (c) first.push(item(c, p.id, c.kind === 'connector', laneMatch));
    }
    if (first.length) columns.push(first);
    for (const ref of p.processors ?? []) {
      const c = byRef.get(ref);
      if (c) columns.push([item(c, p.id, false, laneMatch)]);
    }
    const last: LaneItem[] = [];
    for (const ref of p.exporters ?? []) {
      const c = byRef.get(ref);
      if (c) last.push(item(c, p.id, false, laneMatch));
    }
    if (last.length) columns.push(last);

    const links: LaneLink[] = [];
    for (let i = 0; i + 1 < columns.length; i++) {
      const from = columns[i]!;
      const to = columns[i + 1]!;
      const edges: LaneEdge[] = [];
      for (const a of from)
        for (const b of to) {
          const key = `${p.id}|${a.key}->${b.key}`;
          if (edgeKeys.has(key)) edges.push({ key, from: a.key, to: b.key });
        }
      // Edges into one column share the stage's count, so the largest stands for the link.
      const stats = edges.map((e) => edgeStats?.get(e.key)).filter((s): s is EdgeStats => !!s);
      const count = stats.length ? Math.max(...stats.map((s) => s.items)) : undefined;
      const entered = from.some((a) => (nodeStats?.get(a.key)?.itemsIn ?? 0) > 0);
      links.push({
        pipeline: p.id,
        edges,
        count,
        dropped: count === 0 && entered,
        active: stats.some((s) => s.active),
        short: from.every((a) => a.handoff),
      });
    }
    for (const col of columns) for (const it of col) if (searching && it.match) matched.add(it.key);
    lanes.push({ id: p.id, signal: p.signal, columns, links, match: laneMatch });
  }

  const extensions: ExtensionItem[] = [];
  for (const c of g.components ?? []) {
    if (c.kind !== 'extension') continue;
    if (opts.extensions === 'running' && !extensionRuns(c)) continue;
    const base = item(c, '', false, false);
    extensions.push({ ...base, summary: base.meta });
    if (searching && base.match) matched.add(base.key);
  }

  const used = new Set<string>();
  for (const l of lanes) for (const col of l.columns) for (const it of col) used.add(it.mode.id);
  for (const e of extensions) used.add(e.mode.id);
  return {
    lanes,
    extensions,
    modes: [...MODES.slice(0, 5), ...[...used].filter((id) => !MODES.slice(0, 5).some((m) => m.id === id)).map(modeInfo)],
    searching,
    matches: matched.size,
  };
}

/** linkOnPath says whether the followed record crossed the link. */
export function linkOnPath(link: LaneLink, path: PathHighlight | undefined): boolean {
  return !!path && path.pipelines.has(link.pipeline) && link.edges.some((e) => path.edges.has(`${e.from}->${e.to}`));
}

export const ZOOM_MIN = 0.5;
export const ZOOM_MAX = 1.5;
export const ZOOM_STEP = 0.1;

/** clampZoom keeps a scale within 50-150 % (two decimals). */
export function clampZoom(z: number): number {
  if (!Number.isFinite(z)) return 1;
  return Math.round(Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, z)) * 100) / 100;
}

/** stepZoom zooms in (+1) or out (-1) by 10 percentage points. */
export function stepZoom(z: number, dir: 1 | -1): number {
  return clampZoom(Math.round((z + dir * ZOOM_STEP) * 100) / 100);
}

/** fitZoom is the largest scale at which the content fits the viewport, within the zoom range. */
export function fitZoom(content: { width: number; height: number }, viewport: { width: number; height: number }): number {
  if (content.width <= 0 || content.height <= 0 || viewport.width <= 0 || viewport.height <= 0) return 1;
  // Round down so the content never overflows by a rounding error.
  const raw = Math.min(viewport.width / content.width, viewport.height / content.height);
  return clampZoom(Math.floor(raw * 100) / 100);
}

/** zoomLabel formats a scale as a percentage. */
export function zoomLabel(z: number): string {
  return `${Math.round(z * 100)}%`;
}

export type ArrowDirection = 'left' | 'right' | 'up' | 'down';

/**
 * nextFocus picks the box an arrow key moves focus to: the nearest box in
 * that direction, preferring the one most in line with the current box.
 */
export function nextFocus(
  boxes: Array<{ x: number; y: number; width: number; height: number }>,
  from: number,
  dir: ArrowDirection,
): number | undefined {
  const cur = boxes[from];
  if (!cur) return undefined;
  const cx = cur.x + cur.width / 2;
  const cy = cur.y + cur.height / 2;
  let best: number | undefined;
  let bestScore = Infinity;
  boxes.forEach((b, i) => {
    if (i === from) return;
    const dx = b.x + b.width / 2 - cx;
    const dy = b.y + b.height / 2 - cy;
    const along = dir === 'left' ? -dx : dir === 'right' ? dx : dir === 'up' ? -dy : dy;
    const across = dir === 'left' || dir === 'right' ? Math.abs(dy) : Math.abs(dx);
    if (along <= 1) return;
    const score = across * 3 + along;
    if (score < bestScore) {
      bestScore = score;
      best = i;
    }
  });
  return best;
}
