import { describe, expect, it } from 'vitest';
import type { CaptureStream } from '../api/types';
import { anyValueText, asSelection, clip, filterActive, filterRecords, groupSelection, recordKinds, sampleRecords, scanRecord, selectionBarText, selectionProblem, selectionWarning, severityName, streamKey, streamLabel, summarizeRecord } from './recordView';

const log = (body: unknown, severity: Record<string, unknown> = {}) => ({ resourceLogs: [{ scopeLogs: [{ logRecords: [{ body, ...severity }] }] }] });

describe('summarizeRecord', () => {
  it('reads a log body and severity text', () => {
    expect(summarizeRecord(log({ stringValue: 'card authorization failed' }, { severityText: 'Error', severityNumber: 17 }), 1)).toEqual({ signal: 'logs', kind: 'ERROR', tone: 'error', text: 'card authorization failed' });
  });
  it('names the severity from its number when there is no text', () => {
    expect(summarizeRecord(log({ stringValue: 'x' }, { severityNumber: 21 }), 1)).toMatchObject({ kind: 'FATAL', tone: 'error' });
    expect(summarizeRecord(log({ stringValue: 'x' }, { severityNumber: 13 }), 1)).toMatchObject({ kind: 'WARN', tone: 'warn' });
    expect(summarizeRecord(log({ stringValue: 'x' }, { severityNumber: 9 }), 1)).toMatchObject({ kind: 'INFO', tone: undefined });
    expect(summarizeRecord(log({ stringValue: 'x' }), 1)).toMatchObject({ kind: 'UNSET' });
  });
  it('falls back for a structured or missing body', () => {
    expect(summarizeRecord(log({ kvlistValue: { values: [] } }), 3).text).toBe('{"kvlistValue":{"values":[]}}');
    expect(summarizeRecord(log(undefined), 3).text).toBe('Log record 3');
  });
  it('summarizes spans and metrics by name', () => {
    expect(summarizeRecord({ resourceSpans: [{ scopeSpans: [{ spans: [{ name: 'GET /pay' }] }] }] }, 1)).toEqual({ signal: 'traces', kind: 'SPAN', tone: undefined, text: 'GET /pay' });
    expect(summarizeRecord({ resourceMetrics: [{ scopeMetrics: [{ metrics: [{ name: 'http.requests' }] }] }] }, 1)).toMatchObject({ signal: 'metrics', kind: 'METRIC', text: 'http.requests' });
  });
  it('is safe on payloads it does not understand', () => {
    for (const bad of [undefined, null, 'text', 5, [], {}, { resourceLogs: [] }, { resourceLogs: [{}] }]) {
      expect(summarizeRecord(bad, 9)).toMatchObject({ signal: 'unknown', text: 'Record 9' });
    }
  });
});

describe('text helpers', () => {
  it('writes AnyValues', () => {
    expect(anyValueText({ intValue: '4' })).toBe('4');
    expect(anyValueText({ boolValue: false })).toBe('false');
    expect(anyValueText('plain')).toBe('plain');
    expect(anyValueText(undefined)).toBe('');
  });
  it('names severity ranges', () => {
    expect([0, 1, 5, 9, 13, 17, 24].map(severityName)).toEqual(['UNSET', 'TRACE', 'DEBUG', 'INFO', 'WARN', 'ERROR', 'FATAL']);
  });
  it('clips long single-line text', () => {
    expect(clip('a\n  b')).toBe('a b');
    expect(clip('x'.repeat(200), 10)).toBe('xxxxxxxxx…');
  });
});

describe('record selection', () => {
  const a: CaptureStream = { collector_id: 'c1', pipeline: 'logs', point: 'egress', signal: 'logs' };
  const b: CaptureStream = { collector_id: 'c2', pipeline: 'logs', point: 'egress', signal: 'logs' };

  it('groups selected sequences by source stream, in order', () => {
    const groups = groupSelection({ 9: a, 3: b, 4: a }, 12, (s) => (s === a ? 'otlp' : ''));
    // Streams appear in the order of their first (lowest) sequence.
    expect(groups).toEqual([
      { stream: b, sequences: [3], watermark: 12, receiver: '' },
      { stream: a, sequences: [4, 9], watermark: 12, receiver: 'otlp' },
    ]);
    expect(streamKey(a)).not.toBe(streamKey(b));
  });

  it('describes the selection', () => {
    expect(selectionBarText(groupSelection({ 1: a, 2: a }, 5))).toBe('1 stream: logs from logs · egress');
    expect(selectionBarText(groupSelection({ 1: a, 2: b }, 5))).toBe('2 streams');
    expect(streamLabel({ pipeline: 'p' })).toBe('records from p · ?');
  });

  it('refuses selections the platform would refuse', () => {
    expect(selectionProblem([])).toMatch(/Select received records/);
    expect(selectionProblem(groupSelection({ 1: a }, 1))).toBeUndefined();
    const many = Object.fromEntries(Array.from({ length: 1001 }, (_, i) => [i, a]));
    expect(selectionProblem(groupSelection(many, 1))).toMatch(/1,000 records/);
    const streams = Object.fromEntries(Array.from({ length: 17 }, (_, i) => [i, { ...a, collector_id: `c${i}` }]));
    expect(selectionProblem(groupSelection(streams, 1))).toMatch(/at most 16/);
  });
});

describe('whole-capture filters and samples', () => {
  const stream: CaptureStream = { collector_id: 'c', pipeline: 'logs', point: 'output', signal: 'logs' };
  const rec = (sequence: number, body: string, severityNumber = 9, attrs: Record<string, string> = {}) =>
    scanRecord({ sequence, stream, payload: { resourceLogs: [{ scopeLogs: [{ logRecords: [{ body: { stringValue: body }, severityNumber, attributes: Object.entries(attrs).map(([key, v]) => ({ key, value: { stringValue: v } })) }] }] }] } });
  const records = [rec(1, 'Payment failed', 17, { 'http.route': '/pay' }), rec(2, 'Cache hit'), rec(3, 'Payment ok', 9, { 'http.route': '/pay' }), rec(4, 'Queue backlog', 13)];

  it('matches every word anywhere in the record, case-insensitively, and the kind', () => {
    expect(filterRecords(records, { text: 'payment', kind: '' }).map((r) => r.sequence)).toEqual([1, 3]);
    expect(filterRecords(records, { text: '/PAY failed', kind: '' }).map((r) => r.sequence)).toEqual([1]);
    expect(filterRecords(records, { text: '', kind: 'WARN' }).map((r) => r.sequence)).toEqual([4]);
    expect(filterRecords(records, { text: 'payment', kind: 'INFO' }).map((r) => r.sequence)).toEqual([3]);
    expect(filterActive({ text: '  ', kind: '' })).toBe(false);
    expect(filterActive({ text: '', kind: 'INFO' })).toBe(true);
  });

  it('a record whose payload was omitted is found by its summary only', () => {
    const omitted = scanRecord({ sequence: 9, stream, oversize: true });
    expect(omitted.bytes).toBe(0);
    expect(filterRecords([omitted], { text: 'record 9', kind: '' })).toHaveLength(1);
  });

  it('counts kinds, most frequent first', () => {
    expect(recordKinds(records)).toEqual([{ kind: 'INFO', count: 2 }, { kind: 'ERROR', count: 1 }, { kind: 'WARN', count: 1 }]);
  });

  it('samples the first 100, 100 spread evenly, or up to 10 of each kind', () => {
    const many = Array.from({ length: 1000 }, (_, i) => rec(i + 1, `line ${i}`, i % 4 === 0 ? 17 : 9));
    expect(sampleRecords(many, 'first').map((r) => r.sequence)).toEqual(Array.from({ length: 100 }, (_, i) => i + 1));
    const spread = sampleRecords(many, 'spread');
    expect(spread).toHaveLength(100);
    expect(spread[0].sequence).toBe(1);
    expect(spread[99].sequence).toBe(991);
    expect(sampleRecords(records, 'spread')).toEqual(records);
    const kinds = sampleRecords(many, 'kinds');
    expect(kinds).toHaveLength(20);
    expect(kinds.filter((r) => r.summary.kind === 'ERROR')).toHaveLength(10);
    expect(asSelection(kinds.slice(0, 2))).toEqual({ 1: stream, 2: stream });
  });

  it('warns about a selection above 200 records or 1 MiB, never below', () => {
    expect(selectionWarning(200)).toBeUndefined();
    expect(selectionWarning(200, 1024 * 1024)).toBeUndefined();
    expect(selectionWarning(201)).toBe('201 records make a large scenario: every run replays all of them. A sample of 100 usually shows the same behavior.');
    expect(selectionWarning(1500, 300 * 1024)).toContain('1,500 records (300 KiB)');
    expect(selectionWarning(50, 3 * 1024 * 1024)).toContain('50 records (3.0 MiB)');
  });
});
