import { describe, expect, it } from 'vitest';
import { decodeValue, filterSignalRows, formatDuration, formatTimestamp, signalRows, type SignalFilters } from './signals';

const attr = (key: string, value: unknown) => ({ key, value });
const logs = (records: unknown[]) => ({ resourceLogs: [{ resource: { attributes: [attr('service.name', { stringValue: 'checkout' }), attr('zone', { stringValue: 'west' })] }, scopeLogs: [{ scope: { name: 'checkout.log', version: '1.2', attributes: [attr('source', { stringValue: 'sdk' })] }, logRecords: records }] }] });
const metrics = (items: unknown[]) => ({ resourceMetrics: [{ scopeMetrics: [{ metrics: items }] }] });
const defaults: SignalFilters = { query: '', service: '', category: '', sort: 'recorded' };

describe('signal presentation', () => {
  it('decodes nested AnyValues and preserves false, zero and int64 precision', () => {
    expect(decodeValue({ kvlistValue: { values: [attr('tags', { arrayValue: { values: [{ boolValue: false }, { intValue: '9223372036854775807' }, { doubleValue: 0 }] } })] } })).toEqual({ tags: [false, '9223372036854775807', 0] });
  });

  it('preserves resource and scope context without mutating the snapshot', () => {
    const payload = logs([{ body: { stringValue: 'Payment declined' }, severityNumber: 17, severityText: 'custom-error', attributes: [attr('retry', { boolValue: false })], traceId: 'abc', timeUnixNano: '1760000000000000123' }]);
    const original = JSON.stringify(payload);
    const [r] = signalRows(payload);
    expect(r).toMatchObject({ entity: 'resource[0].scope[0].log_record[0]', title: 'Payment declined', service: 'checkout', category: 'ERROR', badge: 'custom-error', tone: 'bad', time: '2025-10-09T08:53:20.000000123Z' });
    expect(r.attributes).toEqual([['retry', 'false']]);
    expect(r.scope).toContainEqual(['source', 'sdk']);
    expect(r.resource).toContainEqual(['zone', 'west']);
    expect(JSON.stringify(payload)).toBe(original);
  });

  it('uses observed time only when log recorded time is unset', () => {
    const [r] = signalRows(logs([{ body: { stringValue: 'x' }, timeUnixNano: '0', observedTimeUnixNano: '1760000000000000001' }]));
    expect(r.time).toBe('2025-10-09T08:53:20.000000001Z');
    expect(r.category).toBe('UNSPECIFIED');
    expect(r.fields).toContainEqual(['Recorded time (UTC)', '—']);
  });

  it('formats nanoseconds and subtracts timestamps before converting durations', () => {
    expect(formatDuration('1760000000000000001', '1760000000000000002')).toBe('1 ns');
    expect(formatDuration('1760000000000000001', '1760000000001500001')).toBe('1.5 ms');
    expect(formatDuration('20', '10')).toBe('—');
    expect(formatDuration('0', '1000')).toBe('—');
    // A nanosecond count as a JS number has already lost precision.
    // eslint-disable-next-line no-loss-of-precision
    for (const v of [undefined, '', '0', '-1', 'x', '9'.repeat(40), 1760000000000000100]) expect(formatTimestamp(v)).toBe('—');
  });

  it('shows trace status, duration, correlation IDs and event attributes', () => {
    const [r] = signalRows({ resourceSpans: [{ scopeSpans: [{ spans: [{ traceId: 'trace', spanId: 'span', parentSpanId: 'parent', name: 'POST /pay', kind: 2, startTimeUnixNano: '1760000000000000000', endTimeUnixNano: '1760000000042000000', status: { code: 'STATUS_CODE_ERROR', message: 'Declined' }, events: [{ name: 'exception', attributes: [attr('exception.message', { stringValue: 'timeout' })] }] }] }] }] });
    expect(r).toMatchObject({ entity: 'span:trace:span', value: '42 ms', category: 'Error', tone: 'bad' });
    expect(r.fields).toContainEqual(['Kind', 'Server']);
    expect(r.fields).toContainEqual(['Parent span ID', 'parent']);
    expect(r.search).toContain('timeout');
  });

  it('makes a row per metric point, retaining point lineage and metric selection IDs', () => {
    const rows = signalRows(metrics([{ name: 'requests', unit: '1', sum: { aggregationTemporality: 2, isMonotonic: true, dataPoints: [{ asInt: '9223372036854775807', attributes: [attr('route', { stringValue: '/cart' })] }, { asInt: '0' }] } }]));
    expect(rows.map((r) => r.value)).toEqual(['9223372036854775807', '0']);
    expect(rows[0].entity).toBe('resource[0].scope[0].metric[0].data_point[0]');
    expect(rows[0].fields).toContainEqual(['Temporality', 'Cumulative']);
    expect(rows[0].fields).toContainEqual(['Monotonic', 'true']);
    expect(filterSignalRows(rows, { ...defaults, record: rows[0].recordEntity })).toHaveLength(2);
    expect(filterSignalRows(rows, { ...defaults, record: rows[0].entity })).toHaveLength(1);
  });

  it('represents all metric types and does not turn missing points into zero', () => {
    const rows = signalRows(metrics([
      { name: 'g', gauge: { dataPoints: [{ asDouble: 0 }, { flags: 1, asDouble: 99 }] } },
      { name: 'h', histogram: { dataPoints: [{ count: '3', sum: 6, explicitBounds: [1, 5], bucketCounts: ['1', '2', '0'] }] } },
      { name: 'e', exponentialHistogram: { dataPoints: [{ count: '2', scale: -1, zeroCount: '0', positive: { offset: -2, bucketCounts: ['2'] } }] } },
      { name: 's', summary: { dataPoints: [{ count: '2', quantileValues: [{ quantile: 0.99, value: 4 }] }] } },
      { name: 'empty', sum: { dataPoints: [] } },
    ]));
    expect(rows.map((r) => r.value)).toEqual(['0', 'No recorded value', '3 observations · sum 6', '2 observations', '2 observations', 'No data points']);
    expect(rows[2].fields).toContainEqual(['Bucket (1, 5]', '2']);
    expect(rows[2].fields).toContainEqual(['Bucket (5, +∞]', '0']);
    expect(rows[3].fields).toContainEqual(['positive bucket offset', '-2']);
    expect(rows[4].fields).toContainEqual(['Quantile 0.99', '4']);
  });

  it('handles partial or malformed previews and retains original index paths', () => {
    for (const v of [undefined, null, [], 'x', {}, { resourceLogs: [null, {}] }]) expect(signalRows(v)).toEqual([]);
    expect(signalRows(logs([null, { body: { boolValue: false } }]))[0]).toMatchObject({ entity: 'resource[0].scope[0].log_record[1]', title: 'false' });
  });
});

describe('signal search and filters', () => {
  const rows = signalRows(logs([
    { body: { stringValue: 'Payment declined' }, severityNumber: 17, timeUnixNano: '1760000000000000002' },
    { body: { stringValue: 'Payment accepted' }, severityText: 'INFO', timeUnixNano: '1760000000000000001' },
    { body: { stringValue: 'No timestamp' }, severityText: 'warning' },
  ]));

  it('combines case-insensitive plain text terms, inherited attributes and facets', () => {
    expect(filterSignalRows(rows, { ...defaults, query: 'PAYMENT west sdk', service: 'checkout', category: 'ERROR' }).map((r) => r.title)).toEqual(['Payment declined']);
    expect(filterSignalRows(rows, { ...defaults, query: 'payment missing' })).toEqual([]);
    expect(filterSignalRows(rows, { ...defaults, category: 'WARN' }).map((r) => r.title)).toEqual(['No timestamp']);
    expect(filterSignalRows(rows, { ...defaults, record: 'missing' })).toEqual([]);
  });

  it('sorts at nanosecond precision, keeps unset times last and preserves source order', () => {
    expect(filterSignalRows(rows, { ...defaults, sort: 'oldest' }).map((r) => r.title)).toEqual(['Payment accepted', 'Payment declined', 'No timestamp']);
    expect(filterSignalRows(rows, { ...defaults, sort: 'newest' }).map((r) => r.title)).toEqual(['Payment declined', 'Payment accepted', 'No timestamp']);
    expect(rows.map((r) => r.title)).toEqual(['Payment declined', 'Payment accepted', 'No timestamp']);
  });
});
