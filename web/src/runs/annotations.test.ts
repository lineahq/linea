import { describe, expect, it } from 'vitest';
import type { Event, Stage } from '../api/types';
import { edgeStats, nodeStats } from './annotations';

let seq = 0;
const ev = (kind: string, component: string, items: number, pipeline?: string): Event => ({
  id: `r:${++seq}`,
  run_id: 'r',
  seq,
  time: '2026-10-10T00:00:00Z',
  source: 'test',
  kind,
  component,
  pipeline,
  counts: { items, resources: 1, scopes: 1 },
});

// A shape taken from the multisignal fixture run: a filter drops one of two
// logs, the transform after it errors once.
const events: Event[] = [
  ev('input.injected', 'otlp', 2),
  ev('stage.input', 'filter/health', 2, 'logs'),
  ev('stage.output', 'filter/health', 1, 'logs'),
  ev('stage.input', 'transform/logs', 1, 'logs'),
  ev('stage.error', 'transform/logs', 0, 'logs'),
  ev('exporter.captured', 'otlp/backend', 1),
];

const stages: Stage[] = [
  {
    component: 'filter/health',
    pipeline: 'logs',
    kind: 'processor',
    type: 'filter',
    signal: 'logs',
    emission: 'synchronous',
    inputs: [
      {
        seq: 2,
        outcome: 'emitted',
        outputs: [3],
        diff: {
          identity: 'content',
          before: { items: 2, resources: 1, scopes: 1 },
          after: { items: 1, resources: 1, scopes: 1 },
          removed: [{ name: 'healthcheck ok' }],
        },
      },
    ],
  },
];

describe('nodeStats', () => {
  const s = nodeStats(events, stages, true);

  it('counts per node kind', () => {
    expect(s.get('receiver/otlp')).toEqual({ itemsIn: 2 });
    expect(s.get('logs|processor/filter/health')).toMatchObject({ itemsIn: 2, itemsOut: 1, dropped: 1 });
    expect(s.get('logs|processor/transform/logs')).toMatchObject({ itemsIn: 1, errors: 1 });
    expect(s.get('exporter/otlp/backend')).toMatchObject({ itemsIn: 1, active: true });
  });

  it('marks only the latest node of a live run active', () => {
    expect([...s.values()].filter((n) => n.active)).toHaveLength(1);
    expect([...nodeStats(events, stages, false).values()].some((n) => n.active)).toBe(false);
  });
});

describe('edgeStats', () => {
  it('labels edges from processors with their output and into processors with their input', () => {
    const s = nodeStats(events, stages);
    const edges = [
      { key: 'a', from: 'receiver/otlp', to: 'logs|processor/filter/health', pipeline: 'logs', signal: 'logs' },
      { key: 'b', from: 'logs|processor/filter/health', to: 'logs|processor/transform/logs', pipeline: 'logs', signal: 'logs' },
      { key: 'c', from: 'receiver/otlp', to: 'exporter/otlp/backend', pipeline: 'logs', signal: 'logs' },
    ];
    const e = edgeStats(edges, s);
    expect(e.get('a')?.items).toBe(2);
    expect(e.get('b')?.items).toBe(1);
    expect(e.has('c')).toBe(false);
  });

  it('labels edges out of a connector with what it delivered into each pipeline', () => {
    const routed: Event[] = [
      { ...ev('stage.input', 'routing', 3), source: 'connector_wrapper' },
      { ...ev('stage.output', 'routing', 2, 'logs/archive'), source: 'connector_wrapper' },
      { ...ev('stage.output', 'routing', 1, 'logs/pager'), source: 'connector_wrapper' },
    ];
    const s = nodeStats(routed);
    expect(s.get('connector/routing')).toMatchObject({ itemsIn: 3, itemsOut: 3, routes: { 'logs/archive': 2, 'logs/pager': 1 } });
    const e = edgeStats(
      [
        { key: 'a', from: 'connector/routing', to: 'exporter/otlp/archive', pipeline: 'logs/archive', signal: 'logs' },
        { key: 'p', from: 'connector/routing', to: 'logs/pager|processor/transform/page', pipeline: 'logs/pager', signal: 'logs' },
        { key: 'x', from: 'connector/routing', to: 'exporter/otlp/audit', pipeline: 'logs/audit', signal: 'logs' },
      ],
      s,
    );
    expect([e.get('a')?.items, e.get('p')?.items, e.get('x')?.items]).toEqual([2, 1, 0]);
  });
});

describe('what components did beside the pipelines', () => {
  const own = (kind: string, component: string, component_kind: string, extra: Partial<Event> = {}): Event =>
    ({ ...ev(kind, component, 0), counts: undefined, component_kind, ...extra });
  const s = nodeStats([
    own('component.started', 'livecapture', 'extension'),
    own('component.log', 'livecapture', 'extension', { log: { level: 'info', message: 'session opened' } }),
    { ...own('endpoint.received', 'livecapture', 'extension', { endpoint: 'delivery' }), counts: { items: 3, resources: 1, scopes: 1 } },
    own('component.log', 'memory_limiter', 'processor', { pipeline: 'logs', log: { level: 'info', message: 'started' } }),
    own('component.log', 'otlp', 'receiver', { log: { level: 'warn', message: 'slow' } }),
  ]);
  it('counts an extension’s logs and what its endpoints received on its node', () => {
    expect(s.get('extension/livecapture')).toEqual({ logs: 1, sent: 3 });
    expect(s.get('logs|processor/memory_limiter')).toEqual({ logs: 1 });
    expect(s.get('receiver/otlp')).toEqual({ logs: 1 });
  });
});
