import { describe, expect, it } from 'vitest';
import type { Graph } from '../api/types';
import type { PathHighlight } from '../inspect/journey';
import { edgeStats, type NodeStats } from '../runs/annotations';
import { layoutGraph } from './layout';
import {
  clampZoom,
  fitZoom,
  laneModel,
  linkOnPath,
  matchesQuery,
  modeInfo,
  nextFocus,
  nodeMeta,
  routeLabel,
  stepZoom,
  zoomLabel,
} from './lanes';

const comp = (kind: string, id: string, pipelines: string[], mode = 'real') => ({
  ref: `${kind}/${id}`,
  kind,
  id,
  type: id.split('/')[0]!,
  mode,
  pipelines,
});
const edge = (pipeline: string, from: string, to: string) => ({ pipeline, signal: pipeline.split('/')[0]!, from, to });

// The designed example: ingest routes by severity into pager and archive.
const graph: Graph = {
  pipelines: [
    {
      id: 'logs/ingest',
      signal: 'logs',
      receivers: ['receiver/otlp'],
      processors: ['processor/k8sattributes', 'processor/transform/normalize'],
      exporters: ['connector/routing/severity'],
    },
    { id: 'logs/pager', signal: 'logs', receivers: ['connector/routing/severity'], processors: ['processor/filter/pager'], exporters: ['exporter/otlp/pager'] },
    { id: 'logs/archive', signal: 'logs', receivers: ['connector/routing/severity'], processors: ['processor/batch'], exporters: ['exporter/otlp/archive'] },
    { id: 'traces', signal: 'traces', receivers: ['receiver/otlp'], processors: [], exporters: ['exporter/otlp/archive', 'exporter/debug'] },
    { id: 'metrics', signal: 'metrics', receivers: ['receiver/otlp'], processors: [], exporters: ['exporter/debug'] },
  ],
  components: [
    comp('receiver', 'otlp', ['logs/ingest', 'traces', 'metrics']),
    comp('processor', 'k8sattributes', ['logs/ingest'], 'dependency_mocked'),
    comp('processor', 'transform/normalize', ['logs/ingest']),
    comp('connector', 'routing/severity', ['logs/ingest', 'logs/pager', 'logs/archive']),
    comp('processor', 'filter/pager', ['logs/pager']),
    comp('processor', 'batch', ['logs/archive']),
    comp('exporter', 'otlp/pager', ['logs/pager'], 'captured'),
    comp('exporter', 'otlp/archive', ['logs/archive', 'traces'], 'captured'),
    comp('exporter', 'debug', ['traces', 'metrics']),
    comp('extension', 'health_check', [], 'simulated'),
    comp('extension', 'custom_ext', [], 'real'),
  ],
  edges: [
    edge('logs/ingest', 'receiver/otlp', 'processor/k8sattributes'),
    edge('logs/ingest', 'processor/k8sattributes', 'processor/transform/normalize'),
    edge('logs/ingest', 'processor/transform/normalize', 'connector/routing/severity'),
    edge('logs/pager', 'connector/routing/severity', 'processor/filter/pager'),
    edge('logs/pager', 'processor/filter/pager', 'exporter/otlp/pager'),
    edge('logs/archive', 'connector/routing/severity', 'processor/batch'),
    edge('logs/archive', 'processor/batch', 'exporter/otlp/archive'),
    edge('traces', 'receiver/otlp', 'exporter/otlp/archive'),
    edge('traces', 'receiver/otlp', 'exporter/debug'),
    edge('metrics', 'receiver/otlp', 'exporter/debug'),
  ],
};

const stats = new Map<string, NodeStats>([
  ['receiver/otlp', { itemsIn: 5 }],
  ['logs/ingest|processor/k8sattributes', { itemsIn: 5, itemsOut: 5 }],
  ['logs/ingest|processor/transform/normalize', { itemsIn: 5, itemsOut: 5 }],
  ['connector/routing/severity', { itemsIn: 5, itemsOut: 7, routes: { 'logs/pager': 2, 'logs/archive': 5 } }],
  ['logs/pager|processor/filter/pager', { itemsIn: 2, itemsOut: 0, dropped: 2 }],
  ['logs/archive|processor/batch', { itemsIn: 5, itemsOut: 5 }],
  ['exporter/otlp/pager', { itemsIn: 0 }],
  ['exporter/otlp/archive', { itemsIn: 5 }],
]);
const edges = edgeStats(layoutGraph(graph).edges, stats);

describe('laneModel', () => {
  const m = laneModel(graph, { nodeStats: stats, edgeStats: edges });
  const lane = (id: string) => m.lanes.find((l) => l.id === id)!;
  const keys = (id: string) => lane(id).columns.map((c) => c.map((i) => i.key));

  it('makes one lane per pipeline with receivers, processors, then exporters', () => {
    expect(m.lanes.map((l) => l.id)).toEqual(['logs/ingest', 'logs/pager', 'logs/archive', 'traces', 'metrics']);
    expect(keys('logs/ingest')).toEqual([
      ['receiver/otlp'],
      ['logs/ingest|processor/k8sattributes'],
      ['logs/ingest|processor/transform/normalize'],
      ['connector/routing/severity'],
    ]);
  });

  it('uses the layout.ts node keys', () => {
    const layoutKeys = new Set(layoutGraph(graph).nodes.map((n) => n.key));
    const laneKeys = new Set(m.lanes.flatMap((l) => l.columns.flat().map((i) => i.key)));
    for (const k of laneKeys) expect(layoutKeys.has(k), k).toBe(true);
    for (const k of layoutKeys) if (!k.startsWith('extension/')) expect(laneKeys.has(k), k).toBe(true);
  });

  it('shows a consumed connector as a hand-off at the start and the producer as a node at the end', () => {
    const pager = lane('logs/pager').columns[0]![0]!;
    expect(pager).toMatchObject({ key: 'connector/routing/severity', handoff: true, kind: 'connector' });
    const producer = lane('logs/ingest').columns.at(-1)![0]!;
    expect(producer).toMatchObject({ key: 'connector/routing/severity', handoff: false });
    expect(lane('logs/pager').links[0]!.short).toBe(true);
    expect(lane('logs/pager').links[1]!.short).toBe(false);
  });

  it('stacks the exporters of a lane in one column and keeps a shared one under one key', () => {
    expect(keys('traces')).toEqual([['receiver/otlp'], ['exporter/otlp/archive', 'exporter/debug']]);
    expect(lane('logs/archive').columns.at(-1)![0]!.key).toBe('exporter/otlp/archive');
  });

  it('maps modes to the design classes', () => {
    const node = (key: string) => m.lanes.flatMap((l) => l.columns.flat()).find((i) => i.key === key)!;
    expect(node('logs/ingest|processor/k8sattributes').mode).toMatchObject({ cls: 'mocked', label: 'Dependency mocked' });
    expect(node('exporter/otlp/pager').mode.cls).toBe('captured');
    expect(modeInfo('stand_in').cls).toBe('standin');
    expect(modeInfo('something_new')).toMatchObject({ cls: 'unsupported', label: 'something new' });
  });

  it('lists the five modes, and unsupported ones only when present', () => {
    expect(m.modes.map((x) => x.cls)).toEqual(['real', 'mocked', 'captured', 'simulated', 'standin']);
    const g2: Graph = { ...graph, components: [...graph.components!, comp('extension', 'weird', [], 'unsupported')] };
    expect(laneModel(g2).modes.at(-1)!.cls).toBe('unsupported');
  });

  it('builds meta lines from the run', () => {
    const meta = (key: string) => m.lanes.flatMap((l) => l.columns.flat()).find((i) => i.key === key && !i.handoff)!.meta;
    expect(meta('receiver/otlp')).toBe('in 5');
    expect(meta('logs/ingest|processor/k8sattributes')).toBe('5 → 5');
    expect(meta('logs/pager|processor/filter/pager')).toBe('2 → 0 · dropped 2');
    expect(meta('exporter/otlp/pager')).toBe('received 0');
    expect(meta('connector/routing/severity')).toBe('2 → pager · 5 → archive');
    expect(laneModel(graph).lanes[0]!.columns[0]![0]!.meta).toBeUndefined();
  });

  it('counts links from the edge stats and flags a drop after non-zero input', () => {
    expect(lane('logs/ingest').links.map((l) => l.count)).toEqual([5, 5, 5]);
    expect(lane('logs/pager').links.map((l) => [l.count, l.dropped])).toEqual([
      [2, false],
      [0, true],
    ]);
    expect(lane('logs/archive').links.map((l) => l.count)).toEqual([5, 5]);
    // Shared nodes carry no per-pipeline count.
    expect(lane('traces').links[0]!.count).toBeUndefined();
    expect(lane('traces').links[0]!.edges.map((e) => e.key)).toEqual([
      'traces|receiver/otlp->exporter/otlp/archive',
      'traces|receiver/otlp->exporter/debug',
    ]);
  });

  it('does not call a link dropped when nothing entered', () => {
    const none = new Map<string, NodeStats>([['logs/pager|processor/filter/pager', { itemsIn: 0, itemsOut: 0 }]]);
    const e = edgeStats(layoutGraph(graph).edges, none);
    const lm = laneModel(graph, { nodeStats: none, edgeStats: e });
    expect(lm.lanes.find((l) => l.id === 'logs/pager')!.links[1]).toMatchObject({ count: 0, dropped: false });
  });

  it('marks a live link active', () => {
    const live = new Map(stats);
    live.set('logs/archive|processor/batch', { itemsIn: 5, itemsOut: 5, active: true });
    const lm = laneModel(graph, { nodeStats: live, edgeStats: edgeStats(layoutGraph(graph).edges, live) });
    expect(lm.lanes.find((l) => l.id === 'logs/archive')!.links.map((l) => l.active)).toEqual([true, true]);
  });

  it('puts every extension in the row, or only those that run', () => {
    expect(m.extensions.map((x) => x.key)).toEqual(['extension/health_check', 'extension/custom_ext']);
    expect(laneModel(graph, { extensions: 'running' }).extensions.map((x) => x.id)).toEqual(['custom_ext']);
  });

  it('summarizes an extension from its stats', () => {
    const s = new Map<string, NodeStats>([['extension/health_check', { sent: 3, logs: 1 }]]);
    expect(laneModel(graph, { nodeStats: s }).extensions[0]!.summary).toBe('3 received · 1 logged');
  });

  it('filters lanes by signal and keeps the extension row', () => {
    expect(laneModel(graph, { signal: 'traces' }).lanes.map((l) => l.id)).toEqual(['traces']);
    expect(laneModel(graph, { signal: 'metrics' }).lanes.map((l) => l.id)).toEqual(['metrics']);
    expect(laneModel(graph, { signal: 'logs' }).lanes).toHaveLength(3);
    expect(laneModel(graph, { signal: 'traces' }).extensions).toHaveLength(2);
  });

  it('searches components and pipelines', () => {
    const q = laneModel(graph, { query: 'pager' });
    expect(q.searching).toBe(true);
    const match = q.lanes.flatMap((l) => l.columns.flat()).filter((i) => i.match).map((i) => i.key);
    // The whole logs/pager lane matches by its id; nothing in other lanes does.
    expect(new Set(match)).toEqual(new Set(['connector/routing/severity', 'logs/pager|processor/filter/pager', 'exporter/otlp/pager']));
    expect(laneModel(graph, { query: 'k8s' }).matches).toBe(1);
    expect(laneModel(graph, { query: 'nothing here' }).matches).toBe(0);
    expect(laneModel(graph, { query: '  ' }).searching).toBe(false);
    expect(laneModel(graph).lanes.every((l) => l.columns.flat().every((i) => i.match))).toBe(true);
  });

  it('searches by execution mode', () => {
    // otlp/pager and otlp/archive are captured.
    expect(laneModel(graph, { query: 'captured' }).matches).toBe(2);
  });

  it('handles an empty topology', () => {
    expect(laneModel({})).toMatchObject({ lanes: [], extensions: [], matches: 0 });
  });

  it('skips refs that are not components', () => {
    const g: Graph = { pipelines: [{ id: 'logs', signal: 'logs', receivers: ['receiver/x'], processors: [], exporters: [] }], components: [] };
    expect(laneModel(g).lanes[0]!.columns).toEqual([]);
  });
});

describe('nodeMeta', () => {
  it.each([
    ['receiver', { itemsIn: 5, itemsOut: 5 }, [], 'in 5 · out 5'],
    ['processor', { itemsIn: 3 }, [], 'in 3'],
    ['processor', { itemsIn: 4, itemsOut: 2, dropped: 1, errors: 2 }, [], '4 → 2 · dropped 1 · 2 errors'],
    ['processor', { errors: 1 }, [], '1 error'],
    ['exporter', { itemsIn: 5 }, [], 'received 5'],
    ['connector', { itemsIn: 5, routes: { 'logs/a': 3 } }, ['logs/a', 'logs/b'], '3 → a · 0 → b'],
    ['connector', { itemsIn: 5, itemsOut: 5 }, [], '5 → 5'],
    ['connector', { routes: { x: 1 } }, [], '1 → x'],
    ['processor', {}, [], undefined],
  ] as const)('%s %j', (kind, s, consumers, want) => {
    expect(nodeMeta(kind, s as NodeStats, [...consumers])).toBe(want);
  });
  it('is empty without stats', () => expect(nodeMeta('processor', undefined)).toBeUndefined());
});

describe('routeLabel', () => {
  it.each([
    ['logs/pager', 'pager'],
    ['logs', 'logs'],
    ['metrics/a/b', 'a/b'],
    ['x/', 'x/'],
  ])('%s', (id, want) => expect(routeLabel(id)).toBe(want));
});

describe('matchesQuery', () => {
  it('needs every word in some field, ignoring case', () => {
    expect(matchesQuery('OTLP rec', ['otlp', 'receiver'])).toBe(true);
    expect(matchesQuery('otlp batch', ['otlp', 'receiver'])).toBe(false);
    expect(matchesQuery('', [])).toBe(true);
  });
});

describe('linkOnPath', () => {
  const path: PathHighlight = {
    nodes: new Map(),
    edges: new Set(['connector/routing/severity->logs/pager|processor/filter/pager']),
    pipelines: new Set(['logs/pager']),
  };
  const m = laneModel(graph);
  const link = (lane: string, i: number) => m.lanes.find((l) => l.id === lane)!.links[i]!;
  it('follows the journey only in the pipelines it travelled', () => {
    expect(linkOnPath(link('logs/pager', 0), path)).toBe(true);
    expect(linkOnPath(link('logs/pager', 1), path)).toBe(false);
    expect(linkOnPath(link('logs/archive', 0), path)).toBe(false);
    expect(linkOnPath(link('logs/pager', 0), undefined)).toBe(false);
  });
});

describe('zoom', () => {
  it.each([
    [1, 1],
    [0.1, 0.5],
    [3, 1.5],
    [0.5, 0.5],
    [NaN, 1],
  ])('clamps %s', (z, want) => expect(clampZoom(z)).toBe(want));
  it('steps by ten points within range', () => {
    expect(stepZoom(1, 1)).toBe(1.1);
    expect(stepZoom(1.1, 1)).toBe(1.2);
    expect(stepZoom(0.7, -1)).toBe(0.6);
    expect(stepZoom(0.5, -1)).toBe(0.5);
    expect(stepZoom(1.5, 1)).toBe(1.5);
  });
  it('fits the largest scale that shows everything', () => {
    expect(fitZoom({ width: 1000, height: 400 }, { width: 800, height: 600 })).toBe(0.8);
    expect(fitZoom({ width: 1000, height: 1000 }, { width: 800, height: 500 })).toBe(0.5);
    expect(fitZoom({ width: 400, height: 200 }, { width: 800, height: 600 })).toBe(1.5);
    expect(fitZoom({ width: 5000, height: 4000 }, { width: 800, height: 600 })).toBe(0.5);
    expect(fitZoom({ width: 0, height: 0 }, { width: 800, height: 600 })).toBe(1);
    expect(fitZoom({ width: 900, height: 100 }, { width: 1000, height: 600 })).toBe(1.11);
  });
  it('labels as percent', () => expect(zoomLabel(1.1)).toBe('110%'));
});

describe('nextFocus', () => {
  const box = (x: number, y: number) => ({ x, y, width: 100, height: 50 });
  const boxes = [box(0, 0), box(200, 0), box(400, 0), box(0, 100), box(200, 100)];
  it.each([
    [0, 'right', 1],
    [1, 'left', 0],
    [2, 'left', 1],
    [0, 'down', 3],
    [4, 'up', 1],
    [2, 'down', 4],
    [0, 'left', undefined],
    [0, 'up', undefined],
  ] as const)('from %i %s', (from, dir, want) => expect(nextFocus(boxes, from, dir)).toBe(want));
  it('has nowhere to go from nothing', () => expect(nextFocus([], 0, 'left')).toBeUndefined());
});
