import { describe, expect, it } from 'vitest';
import type { Event, Report } from '../api/types';
import { activity, endpointText, metricValue, metricsOf, simulatedEndpoints } from './extensions';

const ev = (seq: number, kind: string, extra: Partial<Event> = {}): Event =>
  ({ id: `r:${seq}`, run_id: 'r', seq, time: '2026-10-15T00:00:00Z', source: 'status_watcher', kind, component: 'livecapture', component_kind: 'extension', ...extra });

const report = {
  result: {
    status: 'completed',
    simulated_endpoints: [
      { component: 'extension/livecapture', path: 'delivery', protocol: 'grpc', endpoint: 'collector.example:4317' },
      { component: 'extension/livecapture', path: 'control', protocol: 'grpc', endpoint: 'control.example:443', refused: [{ call: '/live.v1.Control/Register', count: 2 }] },
      { component: 'processor/tap', path: 'upload', protocol: 'http', endpoint: 'https://upload.example' },
    ],
    endpoint_outputs: [{ component: 'extension/livecapture', endpoint: 'delivery', signal: 'logs', batches: 2, counts: { items: 5, resources: 1, scopes: 1 } }],
    component_metrics: [
      { component_kind: 'extension', component: 'livecapture', name: 'sessions', type: 'sum', int_value: 1 },
      { component_kind: 'extension', component: 'livecapture', name: 'latency', type: 'histogram', count: 3, sum: 1.5 },
      { component_kind: 'processor', component: 'livecapture', name: 'other', type: 'gauge', value: 2 },
    ],
  },
} as unknown as Report;

describe('extension activity', () => {
  it('lists lifecycle and log entries of that extension only, in event order', () => {
    const lines = activity([
      ev(1, 'component.started'),
      ev(2, 'component.log', { source: 'component_telemetry', log: { level: 'warn', message: 'no session', fields: { z: '1', a: 'x' } } }),
      ev(3, 'component.log', { component: 'other', log: { level: 'info', message: 'not mine' } }),
      ev(4, 'stage.input', { component_kind: undefined }),
      ev(5, 'component.stopped', { error: 'flush failed' }),
    ], 'livecapture');
    expect(lines).toEqual([
      { seq: 1, what: 'started', level: undefined, text: 'started' },
      { seq: 2, what: 'log', level: 'warn', text: 'no session', fields: [['a', 'x'], ['z', '1']] },
      { seq: 5, what: 'stopped', level: 'error', text: 'stopped: flush failed' },
    ]);
  });
});

describe('simulated endpoints', () => {
  it('joins each endpoint with what it received and what it refused', () => {
    expect(simulatedEndpoints(report)).toHaveLength(3);
    const mine = simulatedEndpoints(report, 'extension/livecapture');
    expect(mine.map((l) => l.path)).toEqual(['delivery', 'control']);
    expect(endpointText(mine[0])).toBe('received 5 logs item(s) in 2 batch(es).');
    expect(endpointText(mine[1])).toBe('received nothing; refused /live.v1.Control/Register ×2 (not an OTLP export).');
    expect(endpointText({ ...mine[1], refused: [], provider: 'livecapture' }))
      .toBe('served by the provider livecapture; received nothing.');
    expect(simulatedEndpoints(undefined)).toEqual([]);
  });
});

describe('component metrics', () => {
  it('lists the extension’s own points by name and words their values', () => {
    const m = metricsOf(report, 'extension', 'livecapture');
    expect(m.map((p) => p.name)).toEqual(['latency', 'sessions']);
    expect(m.map(metricValue)).toEqual(['count 3, sum 1.5', '1']);
  });
});
