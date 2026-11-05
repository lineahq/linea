import { describe, expect, it } from 'vitest';
import type { Aggregation, RecordLineage } from '../api/types';
import { aggregationRows, contains } from './aggregation';

const rec = (i: number) => `resource[0].scope[0].log_record[${i}]`;
const journey = (i: number, name: string): RecordLineage => ({
  id: `1:${rec(i)}`,
  name,
  signal: 'logs',
  receiver: 'otlp',
  hops: [
    { parent: -1, seq: 1, kind: 'input.injected', component: 'otlp', ref: rec(i) },
    { parent: 0, seq: 4, kind: 'stage.input', component: 'count', ref: rec(i), end: 'aggregated', into: ['5:p0'] },
  ],
});
const point = 'resource[0].scope[0].metric[0].data_point[0]';
const derived: RecordLineage = { id: `5:${point}`, name: 'app.log.errors', signal: 'metrics', origin: 'aggregated', connector: 'count' };

describe('aggregationRows', () => {
  const agg: Aggregation = {
    connector: 'count',
    input: 4,
    output: 5,
    verification: 'replay_verified',
    points: [{ ref: point, metric: 'app.log.errors', attributes: { 'service.name': 'payments', 'http.route': '/pay' }, value: 1, records: [rec(0)] }],
    uncounted: [rec(1)],
  };

  it('names counted and uncounted records by their journeys and links the point', () => {
    const { points, uncounted } = aggregationRows(agg, [journey(0, 'card declined'), journey(1, 'cache miss'), derived]);
    expect(points[0].attributes).toEqual([
      ['http.route', '/pay'],
      ['service.name', 'payments'],
    ]);
    expect(points[0].records).toEqual([{ ref: rec(0), name: 'card declined', lineage: `1:${rec(0)}` }]);
    expect(points[0].lineage).toBe(`5:${point}`);
    expect(uncounted).toEqual([{ ref: rec(1), name: 'cache miss', lineage: `1:${rec(1)}` }]);
  });

  it('names a counted part of a record and keeps unknown records by position', () => {
    const spanAgg: Aggregation = {
      connector: 'count',
      input: 4,
      output: 6,
      verification: 'replay_verified',
      points: [{ ref: point, metric: 'trace.span.event.count', value: 2, records: [`${rec(0)}.event[1]`, rec(7)] }],
    };
    const { points } = aggregationRows(spanAgg, [journey(0, 'GET /')]);
    expect(points[0].records.map((r) => r.name)).toEqual(['GET / · event[1]', rec(7)]);
    expect(points[0].lineage).toBeUndefined();
  });
});

describe('contains', () => {
  it('relates records to their parts both ways', () => {
    expect(contains('a.span[0].event[1]', 'a.span[0]')).toBe(true);
    expect(contains('a.metric[2]', 'a.metric[2].data_point[0]')).toBe(true);
    expect(contains('a.span[1]', 'a.span[10]')).toBe(false);
  });
});
