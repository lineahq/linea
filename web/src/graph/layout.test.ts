import { describe, expect, it } from 'vitest';
import type { Graph } from '../api/types';
import { layoutGraph, ROW } from './layout';

const comp = (kind: string, id: string, pipelines: string[]) => ({
  ref: `${kind}/${id}`,
  kind,
  id,
  type: id.split('/')[0],
  mode: 'real',
  pipelines,
});

// Two pipelines share a receiver and an exporter and use the same processor;
// a connector joins traces to metrics.
const graph: Graph = {
  pipelines: [
    { id: 'traces', signal: 'traces', receivers: ['otlp'], processors: ['batch'], exporters: ['debug', 'spanmetrics'] },
    { id: 'logs', signal: 'logs', receivers: ['otlp'], processors: ['batch'], exporters: ['debug'] },
    { id: 'metrics', signal: 'metrics', receivers: ['spanmetrics'], processors: [], exporters: ['debug'] },
  ],
  components: [
    comp('receiver', 'otlp', ['traces', 'logs']),
    comp('processor', 'batch', ['traces', 'logs']),
    comp('exporter', 'debug', ['traces', 'logs', 'metrics']),
    comp('connector', 'spanmetrics', ['traces', 'metrics']),
  ],
  edges: [
    { pipeline: 'traces', signal: 'traces', from: 'receiver/otlp', to: 'processor/batch' },
    { pipeline: 'traces', signal: 'traces', from: 'processor/batch', to: 'exporter/debug' },
    { pipeline: 'traces', signal: 'traces', from: 'processor/batch', to: 'connector/spanmetrics' },
    { pipeline: 'logs', signal: 'logs', from: 'receiver/otlp', to: 'processor/batch' },
    { pipeline: 'logs', signal: 'logs', from: 'processor/batch', to: 'exporter/debug' },
    { pipeline: 'metrics', signal: 'metrics', from: 'connector/spanmetrics', to: 'exporter/debug' },
  ],
};

describe('layoutGraph', () => {
  const l = layoutGraph(graph);
  const node = (key: string) => l.nodes.find((n) => n.key === key)!;

  it('draws processors once per pipeline and shares the rest', () => {
    expect(l.nodes.map((n) => n.key).sort()).toEqual(
      ['connector/spanmetrics', 'exporter/debug', 'logs|processor/batch', 'receiver/otlp', 'traces|processor/batch'].sort(),
    );
    expect(node('exporter/debug').pipelines.sort()).toEqual(['logs', 'metrics', 'traces']);
  });

  it('layers by longest path, placing the connector between pipelines', () => {
    expect(node('receiver/otlp').layer).toBe(0);
    expect(node('traces|processor/batch').layer).toBe(1);
    expect(node('connector/spanmetrics').layer).toBe(2);
    expect(node('exporter/debug').layer).toBe(3);
  });

  it('never overlaps nodes in a column', () => {
    const seen = new Set(l.nodes.map((n) => `${n.x},${n.y}`));
    expect(seen.size).toBe(l.nodes.length);
  });
});

describe('layoutGraph with extensions', () => {
  const ext = (id: string, mode: string) => ({ ...comp('extension', id, []), mode });
  const l = layoutGraph({ ...graph, components: [...graph.components!, ext('livecapture', 'dependency_mocked'), ext('health_check', 'simulated'), ext('custom', 'real')] });
  it('places only the extensions that run, in a row of their own below the pipelines', () => {
    const exts = l.nodes.filter((n) => n.component.kind === 'extension');
    expect(exts.map((n) => n.key)).toEqual(['extension/livecapture', 'extension/custom']);
    const below = Math.max(...l.nodes.filter((n) => n.component.kind !== 'extension').map((n) => n.y)) + ROW;
    expect(exts.every((n) => n.y === below && n.pipelines.length === 0)).toBe(true);
    expect(new Set(exts.map((n) => n.x)).size).toBe(2);
    expect(l.height).toBe(below + ROW);
  });
  it('lays out a configuration of extensions alone', () => {
    const only = layoutGraph({ pipelines: [], edges: [], components: [ext('custom', 'real')] });
    expect(only.nodes.map((n) => [n.key, n.y])).toEqual([['extension/custom', 0]]);
  });
});
