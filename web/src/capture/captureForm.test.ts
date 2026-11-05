import { describe, expect, it } from 'vitest';
import {
  buildSelection,
  defaultForm,
  formFromSelection,
  formatDuration,
  formatSize,
  isFormDirty,
  parseCount,
  parseDuration,
  parseSize,
  pointKey,
  pointRows,
  reachableTargets,
  filterLimitsFor,
  V1_FILTER_LIMITS,
  reconcilePoints,
  samplingOptions,
  samplingText,
  type CaptureForm,
  type CatalogPoint,
} from './captureForm';

const logsEgress: CatalogPoint = { pipeline: 'logs', point: 'egress', signal: 'logs', sensitivity: 'redacted' };
const logsIngress: CatalogPoint = { pipeline: 'logs', point: 'ingress', signal: 'logs', sensitivity: 'redacted' };
const tracesIngress: CatalogPoint = { pipeline: 'traces', point: 'ingress', signal: 'traces', sensitivity: 'raw' };
const form = (patch: Partial<CaptureForm> = {}): CaptureForm => ({ ...defaultForm, points: [pointKey(logsEgress)], ...patch });

describe('parseDuration', () => {
  it.each([
    ['2m', 120],
    ['90s', 90],
    ['1m30s', 90],
    ['2 min', 120],
    ['1 minute 5 seconds', 65],
    ['120', 120],
    ['1h', 3600],
    [' 45 SEC ', 45],
  ])('%s -> %s', (text, seconds) => expect(parseDuration(text)).toBe(seconds));
  it.each(['', 'soon', '2x', 'm', '1m banana'])('rejects %j', (text) => expect(parseDuration(text)).toBeUndefined());
});

describe('formatDuration', () => {
  it.each([[60, '1m'], [90, '1m30s'], [45, '45s'], [280, '4m40s']])('%s -> %s', (n, text) => expect(formatDuration(n)).toBe(text));
});

describe('parseSize and formatSize', () => {
  it.each([
    ['50 MB', 50 * 1024 ** 2],
    ['64MiB', 64 * 1024 ** 2],
    ['512k', 512 * 1024],
    ['1.5 GB', 1.5 * 1024 ** 3],
    ['1000', 1000],
  ])('%s', (text, bytes) => expect(parseSize(text)).toBe(bytes));
  it.each(['', 'big', '5 TB', '-1 MB'])('rejects %j', (text) => expect(parseSize(text)).toBeUndefined());
  it('writes exact units', () => {
    expect(formatSize(64 * 1024 ** 2)).toBe('64 MiB');
    expect(formatSize(2048)).toBe('2 KiB');
    expect(formatSize(1500)).toBe('1500 B');
  });
});

describe('parseCount', () => {
  it.each([['5000', 5000], ['5,000', 5000], [' 12 ', 12]])('%s', (text, n) => expect(parseCount(text)).toBe(n));
  it.each(['', '1.5', 'many', '-3'])('rejects %j', (text) => expect(parseCount(text)).toBeUndefined());
});

describe('sampling', () => {
  it('offers the presets, plus an unusual current rate', () => {
    expect(samplingOptions(0.1).map((o) => o.label)).toEqual(['All records', '1 in 2 records', '1 in 10 records', '1 in 100 records']);
    expect(samplingOptions(0.25).at(-1)).toEqual({ rate: 0.25, label: '25% of records' });
  });
  it.each([[1, 'all records'], [0.1, '1 in 10'], [0.25, '1 in 4'], [0.3, '30% of records']])('%s -> %s', (rate, text) => expect(samplingText(rate)).toBe(text));
});

describe('buildSelection', () => {
  const catalog = [logsEgress, logsIngress, tracesIngress];
  it('builds the platform request from the form', () => {
    const result = buildSelection(['c1', 'c2'], catalog, form({ duration: '2m', maxRecords: '5000', maxSize: '12 MiB', samplingRate: 0.1, logs: 'severity_number >= 17\n\n' }));
    expect(result.problems).toEqual([]);
    expect(result.selection).toEqual({
      collector_ids: ['c1', 'c2'],
      capture_points: [{ pipeline: 'logs', point: 'egress' }],
      signals: ['logs'],
      duration_seconds: 120,
      filter: { logs: { log: ['severity_number >= 17'] } },
      sampling: { mode: 'hash', rate: 0.1 },
      limits: { bytes_per_second: 1048576, records_per_second: 5000, total_bytes: 12 * 1024 ** 2, total_records: 5000, record_bytes: 65536 },
    });
  });

  it('names a fleet group instead of Collectors', () => {
    const result = buildSelection({ groupId: 'edge', reachable: 2 }, catalog, form());
    expect(result.selection?.group_id).toBe('edge');
    expect(result.selection?.collector_ids).toBeUndefined();
    expect(buildSelection({ groupId: 'edge', reachable: 0 }, catalog, form()).problems.map((p) => p.message)).toEqual(['No connected Collector in this group reports every chosen point.']);
    expect(buildSelection({ groupId: '', reachable: 0 }, catalog, form()).problems.map((p) => p.message)).toEqual(['Choose a group.']);
    expect(buildSelection({ groupId: 'edge', reachable: 17 }, catalog, form()).problems.map((p) => p.field)).toEqual(['targets']);
  });

  it('derives the signals from the chosen points, in a stable order', () => {
    const result = buildSelection(['c1'], catalog, form({ points: [pointKey(tracesIngress), pointKey(logsEgress)] }));
    expect(result.selection?.signals).toEqual(['traces', 'logs']);
  });

  it('only sends filters for signals the capture covers, and resource filters always', () => {
    const result = buildSelection(['c1'], catalog, form({ logs: 'a', traces: 'b', metrics: 'c', datapoint: 'd', resource: 'r' }));
    expect(result.selection?.filter).toEqual({ resource: ['r'], logs: { log: ['a'] } });
  });

  it('sends metric and datapoint filters for metrics', () => {
    const metrics: CatalogPoint = { pipeline: 'metrics', point: 'ingress', signal: 'metrics', sensitivity: 'redacted' };
    const result = buildSelection(['c1'], [metrics], form({ points: [pointKey(metrics)], metrics: 'm', datapoint: 'd' }));
    expect(result.selection?.filter).toEqual({ metrics: { metric: ['m'], datapoint: ['d'] } });
  });

  const problem = (patch: Partial<CaptureForm>, ids = ['c1']) => buildSelection(ids, catalog, form(patch)).problems.map((p) => p.field);
  it.each([
    ['no targets', {}, [], ['targets']],
    ['too many targets', {}, Array.from({ length: 17 }, (_, i) => `c${i}`), ['targets']],
    ['no points', { points: [] }, ['c1'], ['points']],
    ['unknown point key', { points: ['["x","y","logs"]'] }, ['c1'], ['points']],
    ['unreadable duration', { duration: 'soon' }, ['c1'], ['duration']],
    ['duration too short', { duration: '2s' }, ['c1'], ['duration']],
    ['duration too long', { duration: '281s' }, ['c1'], ['duration']],
    ['record limit above the ceiling', { maxRecords: '10001' }, ['c1'], ['maxRecords']],
    ['record limit not a number', { maxRecords: 'lots' }, ['c1'], ['maxRecords']],
    ['size above the ceiling', { maxSize: '17 MiB' }, ['c1'], ['maxSize']],
    ['sampling of zero', { samplingRate: 0 }, ['c1'], ['sampling']],
    ['too many conditions', { logs: Array.from({ length: 17 }, (_, i) => `c${i}`).join('\n') }, ['c1'], ['filter']],
    ['over-long condition', { logs: 'x'.repeat(257) }, ['c1'], ['filter']],
  ] as [string, Partial<CaptureForm>, string[], string[]][])('refuses %s', (_name, patch, ids, fields) => {
    expect(problem(patch, ids)).toEqual(fields);
  });

  it("counts condition characters and follows the targets' filter limits", () => {
    expect(problem({ logs: 'é'.repeat(256) })).toEqual([]);
    const seventeen = form({ logs: Array.from({ length: 17 }, (_, i) => `c${i}`).join('\n') });
    const large = { max_conditions: 64, max_condition_chars: 1024, max_regex_chars: 256 };
    expect(buildSelection(['c1'], catalog, seventeen, large).problems).toEqual([]);
    expect(buildSelection(['c1'], catalog, form({ logs: 'x'.repeat(1025) }), large).problems.map((p) => p.message)).toEqual(['Each condition may be at most 1024 characters.']);
  });

  it('builds nothing while a problem remains', () => {
    expect(buildSelection(['c1'], catalog, form({ duration: 'soon' })).selection).toBeUndefined();
  });
});

describe('formFromSelection', () => {
  it('seeds the form from an earlier request and drops points the catalog lacks', () => {
    const seeded = formFromSelection(
      {
        collector_ids: ['c1'],
        capture_points: [{ pipeline: 'logs', point: 'egress' }, { pipeline: 'gone', point: 'egress' }],
        signals: ['logs'],
        duration_seconds: 120,
        filter: { resource: ['r'], logs: { log: ['a', 'b'] } },
        sampling: { mode: 'hash', rate: 0.1 },
        limits: { bytes_per_second: 1, records_per_second: 1, total_bytes: 50 * 1024 ** 2, total_records: 5000, record_bytes: 1 },
      },
      [logsEgress, logsIngress],
    );
    expect(seeded).toEqual({ ...defaultForm, points: [pointKey(logsEgress)], resource: 'r', logs: 'a\nb', samplingRate: 0.1, duration: '2m', maxRecords: '5000', maxSize: '50 MiB' });
  });
  it('round-trips through buildSelection', () => {
    const seeded = formFromSelection(buildSelection(['c1'], [logsEgress], form({ duration: '90s', logs: 'x' })).selection!, [logsEgress]);
    expect(seeded.duration).toBe('1m30s');
    expect(seeded.logs).toBe('x');
  });
});

describe('isFormDirty and reconcilePoints', () => {
  it('is clean only at the defaults', () => {
    expect(isFormDirty(defaultForm)).toBe(false);
    expect(isFormDirty({ ...defaultForm, duration: '2m' })).toBe(true);
    expect(isFormDirty(form())).toBe(true);
  });
  it('keeps only the points the new catalog offers', () => {
    const kept = reconcilePoints({ ...defaultForm, points: [pointKey(logsEgress), pointKey(tracesIngress)] }, [logsEgress]);
    expect(kept.points).toEqual([pointKey(logsEgress)]);
  });
});

describe('pointRows', () => {
  const targets = [{ pod: 'gw-0' }, { pod: 'gw-1' }, { pod: 'gw-2' }];
  const catalogs = [[logsEgress, logsIngress, tracesIngress], [logsEgress, logsIngress, tracesIngress], [logsEgress]];
  const rows = pointRows(targets, catalogs);
  it('says how many targets report each point and which do not', () => {
    const ingress = rows.find((r) => r.point.pipeline === 'logs' && r.point.point === 'ingress')!;
    expect(ingress).toMatchObject({ availableOn: 2, missingOn: ['gw-2'], selectable: false, reason: 'not on gw-2' });
    const egress = rows.find((r) => r.point.point === 'egress')!;
    expect(egress).toMatchObject({ availableOn: 3, missingOn: [], selectable: true });
  });
  it('lets a group capture choose any member point and names who is left out', () => {
    const ingress = pointRows(targets, catalogs, true).find((r) => r.point.pipeline === 'logs' && r.point.point === 'ingress')!;
    expect(ingress).toMatchObject({ selectable: true, reason: 'gw-2 left out' });
  });
  it('offers raw points; fleet checks the raw permission at start', () => {
    expect(pointRows([targets[0]], [[tracesIngress]])[0]).toMatchObject({ selectable: true, point: { sensitivity: 'raw' } });
  });
  it('orders logs first and ingress before egress', () => {
    expect(rows.map((r) => `${r.point.signal}:${r.point.point}`)).toEqual(['logs:ingress', 'logs:egress', 'traces:ingress']);
  });
  it('treats a raw report from any target as raw', () => {
    const mixed = pointRows([targets[0], targets[1]], [[{ ...logsEgress, sensitivity: 'raw' }], [logsEgress]]);
    expect(mixed[0].point.sensitivity).toBe('raw');
  });
});

describe('filterLimitsFor', () => {
  const small = { filter_limits: V1_FILTER_LIMITS };
  const large = { filter_limits: { max_conditions: 64, max_condition_chars: 1024, max_regex_chars: 512 } };
  it('takes the smallest limits of chosen Collectors and the largest of a group', () => {
    expect(filterLimitsFor([small, large], false)).toEqual(V1_FILTER_LIMITS);
    expect(filterLimitsFor([small, large], true)).toEqual(large.filter_limits);
  });
  it('uses the version 1 limits without targets or reported limits', () => {
    expect(filterLimitsFor([], true)).toEqual(V1_FILTER_LIMITS);
    expect(filterLimitsFor([{} as typeof small, large], false)).toEqual(V1_FILTER_LIMITS);
  });
});

describe('reachableTargets', () => {
  it('keeps the members that report every chosen point', () => {
    const catalogs = [[logsEgress, logsIngress], [logsEgress], []];
    expect(reachableTargets(['a', 'b', 'c'], catalogs, [pointKey(logsEgress)])).toEqual(['a', 'b']);
    expect(reachableTargets(['a', 'b', 'c'], catalogs, [pointKey(logsEgress), pointKey(logsIngress)])).toEqual(['a']);
    expect(reachableTargets(['a'], catalogs, [])).toEqual([]);
  });
});
