import { describe, expect, it } from 'vitest';
import type { Change, Diff } from '../api/types';
import { changeOverview, distinctValues, fieldLabel, filterOverview } from './changeOverview';
import { groupChanges } from './grouped';

const entity = (i: number) => `resource[0].scope[0].log_record[${i}]`;
const logs = (messages: string[]) => ({ resourceLogs: [{ resource: { attributes: [{ key: 'service.name', value: { stringValue: 'checkout' } }] }, scopeLogs: [{ scope: { name: 'app' }, logRecords: messages.map((s) => ({ body: { stringValue: s } })) }] }] });
const before = logs(['Payment declined', 'Login retry', 'Health check']);
const diff = (changes: Change[], extra: Partial<Diff> = {}): Diff => ({ identity: 'content', before: { resources: 1, scopes: 1, items: 3 }, after: { resources: 1, scopes: 1, items: 3 }, changes, ...extra });
const edit = (i: number, value: string): Change => ({ entity: entity(i), path: 'attributes["user.id"]', op: 'modified', before: value, after: `hash-${value}`, confidence: 'high' });

describe('change overview', () => {
  it('groups repeated edits once and counts distinct entities, not field occurrences', () => {
    const d = diff([edit(0, 'jane'), edit(1, 'omar'), { entity: entity(0), path: 'attributes["password"]', op: 'removed', before: 'secret' }]);
    const overview = changeOverview(d, before, undefined);
    expect(overview.fields).toHaveLength(2);
    expect(overview.fields[0]).toMatchObject({ field: 'user.id', targetLabel: 'Log attribute', entities: 2 });
    expect(overview.editedRecords).toBe(2);
    expect(overview.counts).toMatchObject({ all: 3, modified: 2, removed: 1, dropped: 0 });
    expect(overview.fields[0].edits.map((e) => e.label)).toEqual(['Payment declined', 'Login retry']);
    expect(distinctValues(overview.fields[0].edits, 'before').map((v) => v.text)).toEqual(['jane', 'omar']);
  });

  it('keeps resource/scope edits separate from direct record edits and preserves span context', () => {
    const spans = { resourceSpans: [{ resource: { attributes: [{ key: 'service.name', value: { stringValue: 'api' } }] }, scopeSpans: [{ scope: { name: 'http' }, spans: [{ traceId: 't', spanId: '1', name: 'GET /' }, { traceId: 't', spanId: '2', name: 'POST /' }] }] }] };
    const d = diff([
      { entity: 'resource[0]', path: 'attributes["env"]', op: 'added', after: 'prod' },
      { entity: 'resource[0].scope[0]', path: 'version', op: 'modified', before: '1', after: '2' },
      { entity: 'span:t:1', path: 'attributes["env"]', op: 'added', after: 'local' },
    ]);
    const overview = changeOverview(d, spans, undefined, 'span:t:1');
    expect(overview.fields).toHaveLength(3);
    expect(overview.editedRecords).toBe(1);
    expect(overview.editedResources).toBe(1);
    expect(overview.editedScopes).toBe(1);
    expect(overview.fields.map((f) => [f.target, f.sharedRecords])).toEqual([['resource', 2], ['scope', 2], ['record', 0]]);
    expect(overview.fields[2].targetLabel).toBe('Span attribute');
  });

  it('filters selected metric groups to their individual point edits with readable labels', () => {
    const metricEntity = 'resource[0].scope[0].metric[0]';
    const point = `${metricEntity}.data_point[0]`;
    const payload = { resourceMetrics: [{ scopeMetrics: [{ metrics: [{ name: 'latency', gauge: { dataPoints: [{ asDouble: 250, attributes: [{ key: 'route', value: { stringValue: '/cart' } }] }, { asDouble: 40 }] } }] }] }] };
    const d = diff([{ entity: point, path: 'as_double', op: 'modified', before: 250, after: 0.25 }]);
    const overview = changeOverview(d, payload, undefined, metricEntity);
    expect(overview.fields[0].edits[0]).toMatchObject({ label: 'latency', context: 'Unknown service · 250' });
    const grouped = groupChanges(d, payload);
    expect(grouped.resources[0].scopes[0].records[0]).toMatchObject({ label: 'latency · 250', unchanged: [{ key: 'route', value: '/cart' }] });
    expect(overview.editedRecords).toBe(1);
  });

  it('retains ID-based span edits and drops when their containing resource is selected', () => {
    const payload = { resourceSpans: [
      { scopeSpans: [{ spans: [{ traceId: 't', spanId: '1', name: 'GET /' }, { traceId: 't', spanId: '2', name: 'GET /health' }] }] },
      { scopeSpans: [{ spans: [{ traceId: 'other', spanId: '3', name: 'Other service' }] }] },
    ] };
    const d = diff([
      { entity: 'span:t:1', path: 'name', op: 'modified', before: 'GET /', after: 'GET /home' },
      { entity: 'span:other:3', path: 'name', op: 'modified', before: 'Other service', after: 'Changed' },
    ], { removed: [{ trace_id: 't', span_id: '2', name: 'GET /health' }] });
    const overview = changeOverview(d, payload, undefined, 'resource[0]');
    expect(overview.fields[0].edits.map((e) => e.change.entity)).toEqual(['span:t:1']);
    expect(overview.records.map((r) => r.entity)).toEqual(['span:t:2']);
    expect(overview.editedRecords).toBe(1);
  });

  it('does not count an empty metric as a record sharing a resource', () => {
    const payload = { resourceMetrics: [{ scopeMetrics: [{ metrics: [{ name: 'empty', gauge: { dataPoints: [] } }] }] }] };
    const overview = changeOverview(diff([{ entity: 'resource[0]', path: 'attributes["env"]', op: 'added', after: 'prod' }]), payload, undefined);
    expect(overview.fields[0].sharedRecords).toBe(0);
  });

  it('uses output context for new records and never confuses colliding input/output positions', () => {
    const d = diff([], { removed: [{ ref: entity(0), name: 'old' }], added: [{ ref: entity(0), name: 'new' }] });
    const overview = changeOverview(d, before, logs(['Created event']));
    expect(overview.records.map((r) => [r.kind, r.label])).toEqual([['dropped', 'Payment declined'], ['created', 'Created event']]);
    const selected = changeOverview(d, before, logs(['Created event']), entity(0));
    expect(selected.records.map((r) => r.kind)).toEqual(['dropped']);
    expect(selected.counts.created).toBe(0);
  });

  it('distinguishes removal from an empty string and keeps int/string type changes separate', () => {
    const overview = changeOverview(diff([
      { entity: entity(0), path: 'attributes["flag"]', op: 'removed', before: false },
      { entity: entity(1), path: 'attributes["id"]', op: 'type_changed', before: 5, after: '5', before_type: 'Int', after_type: 'Str' },
    ]), before, undefined);
    expect(distinctValues(overview.fields[0].edits, 'before')).toEqual([{ text: 'false', type: undefined }]);
    expect(distinctValues(overview.fields[0].edits, 'after')[0].text).toBe('Not present');
    const typed = overview.fields[1].edits;
    expect(distinctValues(typed, 'before')).toEqual([{ text: '5', type: 'Int' }]);
    expect(distinctValues(typed, 'after')).toEqual([{ text: '5', type: 'Str' }]);
    expect(overview.counts).toMatchObject({ modified: 0, type_changed: 1 });
  });

  it('searches affected records and values, retaining only the matching edits and shared counts', () => {
    const overview = changeOverview(diff([edit(0, 'jane'), edit(1, 'omar')]), before, undefined);
    const result = filterOverview(overview, 'user.id jane', 'modified');
    expect(result.fields[0].entities).toBe(1);
    expect(result.fields[0].edits[0].label).toBe('Payment declined');
    expect(filterOverview(overview, 'missing', 'all').fields).toEqual([]);
    expect(filterOverview(overview, '', 'removed').fields).toEqual([]);
    expect(overview.fields[0].edits).toHaveLength(2);
  });

  it('formats quoted attribute keys without treating their dots as structural paths', () => {
    expect(fieldLabel('attributes["http.request.method"]')).toBe('http.request.method');
    expect(fieldLabel('attributes["nested"].child')).toBe('nested.child');
    expect(fieldLabel('severity_text')).toBe('severity text');
    expect(fieldLabel('metric.name')).toBe('metric · name');
    expect(fieldLabel('attributes["a\\"b"]')).toBe('a"b');
  });
});
