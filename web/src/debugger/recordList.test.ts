import { describe, expect, it } from 'vitest';
import type { Event, RecordLineage } from '../api/types';
import { fateOf, fateText, filterCounts, filterRows, recordRows, stopNode } from './recordList';
import { journeyPath } from './followPath';

const base = { signal: 'logs', receiver: 'otlp' };
const dropped = { id: 'a', name: 'card authorization failed', ...base, stops: [{ end: 'dropped', component: 'filter/pager', hop: 1 }] } as RecordLineage;
const delivered = { id: 'b', name: 'checkout completed', ...base, deliveries: [{ exporter: 'otlp/archive', hop: 2, changed: false }] } as RecordLineage;
const lost = { id: 'c', name: 'retrying', ...base, stops: [{ end: 'unobserved', component: 'batch', hop: 1 }] } as RecordLineage;
const bare = { id: 'd', name: 'x', ...base } as RecordLineage;
const counted = { id: 'e', name: 'count', ...base, stops: [{ end: 'aggregated', component: 'count/errors', hop: 1 }] } as RecordLineage;
const both = { id: 'f', name: 'both', ...base, deliveries: [{ exporter: 'x', hop: 2, changed: true }], stops: [{ end: 'dropped', component: 'filter/y', hop: 1 }] } as RecordLineage;

describe('record fates', () => {
  it('classifies a journey by where it ended', () => {
    expect([dropped, delivered, lost, bare, counted, both].map(fateOf)).toEqual(['dropped', 'delivered', 'nooutput', 'nooutput', 'counted', 'delivered']);
  });
  it('words the fate; no observed output is never called a drop', () => {
    expect(fateText(dropped)).toBe('dropped at filter/pager');
    expect(fateText(delivered)).toBe('delivered to otlp/archive');
    expect(fateText(lost)).toBe('no observed output at batch');
    expect(fateText(bare)).toBe('no observed output');
    expect(fateText(both)).toBe('delivered to x; dropped at filter/y');
  });
  it('counts and filters rows', () => {
    const rows = recordRows([dropped, delivered, delivered, lost]);
    expect(filterCounts(rows)).toEqual([
      { filter: 'all', count: 4 }, { filter: 'dropped', count: 1 }, { filter: 'delivered', count: 2 }, { filter: 'nooutput', count: 1 },
    ]);
    expect(filterRows(rows, 'dropped', '').map((r) => r.id)).toEqual(['a']);
    expect(filterRows(rows, 'all', 'ARCHIVE checkout').map((r) => r.id)).toEqual(['b', 'b']);
    expect(filterRows(rows, 'delivered', 'authorization')).toEqual([]);
    expect(filterCounts(recordRows([counted])).map((c) => c.filter)).toContain('counted');
  });
});

const ev = (seq: number, kind: string, component: string, pipeline?: string, source = 'collector') =>
  ({ seq, kind, component, pipeline, source, id: String(seq), run_id: 'r', time: '' }) as unknown as Event;
const events = [ev(1, 'input.injected', 'otlp'), ev(2, 'stage.input', 'transform/normalize', 'logs/ingest'), ev(3, 'stage.output', 'transform/normalize', 'logs/ingest'), ev(4, 'stage.input', 'filter/pager', 'logs/pager')];
const journey = {
  id: 'a', name: 'n', ...base,
  hops: [
    { kind: 'input.injected', component: 'otlp', parent: -1, ref: 'r0', seq: 1 },
    { kind: 'stage.input', component: 'transform/normalize', pipeline: 'logs/ingest', parent: 0, ref: 'r0', seq: 2 },
    { kind: 'stage.output', component: 'transform/normalize', pipeline: 'logs/ingest', parent: 1, ref: 'r0', seq: 3, changes: [{ entity: 'e', op: 'changed', path: 'p' }] },
    { kind: 'stage.input', component: 'filter/pager', pipeline: 'logs/pager', parent: 2, ref: 'r0', seq: 4, end: 'dropped' },
  ],
  stops: [{ end: 'dropped', component: 'filter/pager', hop: 3, pipeline: 'logs/pager' }],
} as unknown as RecordLineage;

describe('followed journey', () => {
  it('lists each observed node once, in hop order, with where it stopped', () => {
    expect(journeyPath(journey, events)).toEqual([
      { key: 'receiver/otlp', label: 'otlp', status: 'passed' },
      { key: 'logs/ingest|processor/transform/normalize', label: 'transform/normalize', status: 'changed' },
      { key: 'logs/pager|processor/filter/pager', label: 'filter/pager', status: 'stopped' },
    ]);
  });
  it('finds the node where a journey stopped', () => {
    expect(stopNode(journey, events)).toBe('logs/pager|processor/filter/pager');
    expect(stopNode(delivered, events)).toBeUndefined();
  });
});
