import { describe, expect, it } from 'vitest';
import type { Event } from '../api/types';
import { counterpart, listRecords, signalOf } from './records';
import { changedPaths, flattenTree } from './tree';
import { unified } from './unified';
import { eventStep, nextSteps, prevStep } from './navigation';

const logs = (bodies: string[]) => ({
  resourceLogs: [{ scopeLogs: [{ logRecords: bodies.map((b) => ({ body: { stringValue: b } })) }] }],
});

describe('records', () => {
  it('names records like the stage analysis', () => {
    const traces = { resourceSpans: [{ scopeSpans: [{ spans: [{ traceId: 't', spanId: 's', name: 'GET /' }] }] }] };
    expect(signalOf(traces)).toBe('traces');
    expect(listRecords(traces).map((r) => [r.entity, r.label])).toEqual([['span:t:s', 'GET /']]);
    expect(listRecords(logs(['a', 'b'])).map((r) => [r.entity, r.label])).toEqual([
      ['resource[0].scope[0].log_record[0]', 'a'],
      ['resource[0].scope[0].log_record[1]', 'b'],
    ]);
  });

  it('matches positional records after removals', () => {
    const before = listRecords(logs(['a', 'drop', 'c']));
    const after = listRecords(logs(['a', 'c']));
    const removed = new Set(['resource[0].scope[0].log_record[1]']);
    expect(counterpart(before, after, 'resource[0].scope[0].log_record[2]', removed)?.label).toBe('c');
    expect(counterpart(before, after, 'resource[0].scope[0].log_record[1]', removed)).toBeUndefined();
  });
});

describe('tree', () => {
  it('flattens open containers and hides collapsed ones', () => {
    const v = { a: { b: 1 }, c: [true] };
    expect(flattenTree(v, new Set()).map((r) => r.path)).toEqual(['', 'a', 'a/b', 'c', 'c/0']);
    expect(flattenTree(v, new Set(['a'])).map((r) => r.path)).toEqual(['', 'a', 'c', 'c/0']);
  });

  it('marks changed paths and their ancestors', () => {
    const before = { a: { b: 1, x: 2 }, c: 3 };
    const after = { a: { b: 1 }, c: 4 };
    expect([...changedPaths(before, after)].sort()).toEqual(['', 'a', 'a/x', 'c']);
    expect([...changedPaths(after, before)].sort()).toEqual(['', 'a', 'c']);
  });
});

describe('unified', () => {
  it('numbers added, removed and context lines', () => {
    const lines = unified('a\nb\nc\n', 'a\nB\nc\n');
    expect(lines.map((l) => [l.kind, l.text, l.old, l.new])).toEqual([
      ['hunk', '@@ -1,3 +1,3 @@', undefined, undefined],
      ['ctx', 'a', 1, 1],
      ['del', 'b', 2, undefined],
      ['add', 'B', undefined, 2],
      ['ctx', 'c', 3, 3],
    ]);
  });
});

describe('navigation', () => {
  const ev = (seq: number, kind: string, extra: Partial<Event> = {}): Event =>
    ({ seq, kind, id: String(seq), run_id: 'r', source: 's', time: '', ...extra }) as Event;
  const events = [
    ev(1, 'input.injected', { component: 'otlp', snapshot: 'A' }),
    ev(2, 'stage.input', { component: 'p1', pipeline: 'logs', snapshot: 'A' }),
    ev(3, 'stage.output', { component: 'p1', pipeline: 'logs', snapshot: 'B', input_seq: 2 }),
    ev(4, 'stage.input', { component: 'p2', pipeline: 'logs', snapshot: 'B' }),
    ev(5, 'stage.output', { component: 'p2', pipeline: 'logs', snapshot: 'C', input_seq: 4 }),
    ev(6, 'exporter.captured', { component: 'debug', snapshot: 'C' }),
  ];

  it('follows a payload by snapshot ref', () => {
    expect(prevStep(events, 4)).toMatchObject({ node: 'logs|processor/p1', input: 2 });
    expect(prevStep(events, 2)).toMatchObject({ node: 'receiver/otlp', input: 1 });
    expect(nextSteps(events, 3, 'logs')[0]).toMatchObject({ node: 'logs|processor/p2', input: 4 });
    expect(nextSteps(events, 5, 'logs')[0]).toMatchObject({ node: 'exporter/debug', input: 6 });
    expect(prevStep(events, 6)).toMatchObject({ node: 'logs|processor/p2', input: 4 });
    expect(eventStep(events[2])).toMatchObject({ node: 'logs|processor/p1', input: 2 });
  });

  // The same unchanged payload enters two pipelines through a connector:
  // snapshots cannot tell the branches apart, from_seq does.
  const routed = [
    ev(1, 'input.injected', { component: 'otlp', snapshot: 'A' }),
    ev(2, 'stage.input', { component: 'routing', source: 'connector_wrapper', snapshot: 'A', from_seq: 1 }),
    ev(3, 'stage.output', { component: 'routing', source: 'connector_wrapper', pipeline: 'logs/b', snapshot: 'A', input_seq: 2 }),
    ev(4, 'stage.input', { component: 'p', pipeline: 'logs/b', snapshot: 'A', from_seq: 3 }),
    ev(5, 'stage.output', { component: 'routing', source: 'connector_wrapper', pipeline: 'logs/a', snapshot: 'A', input_seq: 2 }),
    ev(6, 'stage.input', { component: 'p', pipeline: 'logs/a', snapshot: 'A', from_seq: 5 }),
    ev(7, 'exporter.captured', { component: 'debug', snapshot: 'A', from_seq: 6 }),
  ];

  it('follows the recorded call through a connector', () => {
    expect(nextSteps(routed, 1, undefined)).toMatchObject([{ node: 'connector/routing', input: 2 }]);
    expect(nextSteps(routed, 5, 'logs/a')).toMatchObject([{ node: 'logs/a|processor/p', input: 6 }]);
    expect(prevStep(routed, 6)).toMatchObject({ node: 'connector/routing', input: 2 });
    expect(prevStep(routed, 4)).toMatchObject({ node: 'connector/routing', input: 2 });
    expect(prevStep(routed, 2)).toMatchObject({ node: 'receiver/otlp', input: 1 });
    expect(nextSteps(routed, 3, 'logs/b').map((s) => s.input)).toEqual([4]);
    // An exporter ends the path.
    expect(nextSteps(routed, 7, undefined)).toEqual([]);
  });
});
