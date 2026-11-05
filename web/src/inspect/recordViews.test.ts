import { describe, expect, it } from 'vitest';
import type { Diff, OTTLReport } from '../api/types';
import { ottlPath, recordColumns, recordViews, statementLabels } from './recordViews';

const log = (message: string, extra: object[] = []) => ({ body: { stringValue: message }, severityText: 'INFO', attributes: [{ key: 'user.id', value: { stringValue: 'jane' } }, ...extra] });
const before = {
  resourceLogs: [{
    resource: { attributes: [{ key: 'service.name', value: { stringValue: 'accounts' } }] },
    scopeLogs: [{ scope: { name: 'app', version: '1.0' }, logRecords: [log('Login'), log('Health check')] }],
  }],
};
const counts = { items: 2, resources: 1, scopes: 1 };
const first = 'resource[0].scope[0].log_record[0]';
const diff = (d: Partial<Diff>): Diff => ({ identity: 'content', before: counts, after: counts, ...d });
const field = (v: ReturnType<typeof recordViews>['records'][number], path: string) => v.fields.find((f) => f.path === path)!;

describe('recordViews', () => {
  it('names the badge column after what the badge shows for each signal', () => {
    expect(recordColumns('logs')).toEqual({ badge: 'Severity', record: 'Message' });
    expect(recordColumns('traces')).toEqual({ badge: 'Status', record: 'Operation' });
    expect(recordColumns('metrics')).toEqual({ badge: 'Type', record: 'Metric' });
    expect(recordColumns(undefined).badge).toBe('Kind');
  });

  it('lists every input record, editing only the ones with changes', () => {
    const { records } = recordViews(diff({ changes: [{ entity: first, path: 'attributes["user.id"]', op: 'modified', before: 'jane', after: 'hashed' }] }), before, before);
    expect(records.map((r) => [r.title, r.state, r.changed])).toEqual([['Login', 'edited', 1], ['Health check', 'unchanged', 0]]);
    expect(field(records[0], 'log.attributes["user.id"]')).toMatchObject({ op: 'modified', before: '"jane"', after: '"hashed"', level: 'record' });
  });

  it('puts shared resource and scope edits on every record they apply to, in their own level', () => {
    const { records } = recordViews(diff({ changes: [
      { entity: 'resource[0]', path: 'attributes["env"]', op: 'added', after: 'prod', after_type: 'Str' },
      { entity: 'resource[0].scope[0]', path: 'version', op: 'modified', before: '1.0', after: '2.0' },
    ] }), before, before);
    for (const r of records) {
      expect(r.state).toBe('edited');
      expect(r.levels).toEqual(['resource', 'scope']);
      expect(field(r, 'resource.attributes["env"]')).toMatchObject({ level: 'resource', op: 'added', after: '"prod"', type: 'str' });
      expect(field(r, 'instrumentation_scope.version')).toMatchObject({ level: 'scope', before: '"1.0"', after: '"2.0"' });
    }
  });

  it('carries unchanged context with OTLP types so the whole record can be shown', () => {
    const typed = { resourceLogs: [{ resource: { attributes: [{ key: 'pid', value: { intValue: '42' } }] }, scopeLogs: [{ scope: { name: 'app' }, logRecords: [log('x', [{ key: 'n', value: { intValue: '5' } }])] }] }] };
    const { records } = recordViews(diff({}), typed, typed);
    expect(field(records[0], 'resource.attributes["pid"]')).toMatchObject({ op: 'same', before: '42', type: 'int' });
    expect(field(records[0], 'log.attributes["n"]')).toMatchObject({ op: 'same', before: '5', type: 'int' });
    expect(field(records[0], 'instrumentation_scope.name')).toMatchObject({ before: '"app"' });
    expect(field(records[0], 'log.body')).toMatchObject({ before: '"x"', type: 'str' });
  });

  it('keeps empty strings, removals and type changes distinct', () => {
    const { records } = recordViews(diff({ changes: [
      { entity: first, path: 'attributes["user.id"]', op: 'removed', before: '', before_type: 'Str' },
      { entity: first, path: 'attributes["count"]', op: 'type_changed', before: 5, after: '5', before_type: 'Int', after_type: 'Str' },
    ] }), before, before);
    expect(field(records[0], 'log.attributes["user.id"]')).toMatchObject({ op: 'removed', before: '""', after: undefined });
    expect(field(records[0], 'log.attributes["count"]')).toMatchObject({ before: '5', after: '"5"', type: 'int → str' });
  });

  it('shows dropped records from the input and created ones from the output, never pairing them', () => {
    const output = { resourceLogs: [{ scopeLogs: [{ logRecords: [log('Created log')] }] }] };
    const { records } = recordViews(diff({ removed: [{ ref: first, name: 'Login' }], added: [{ ref: first, name: 'Created log' }] }), before, output);
    expect(records.map((r) => [r.title, r.state, r.side])).toEqual([['Login', 'dropped', 'before'], ['Health check', 'unchanged', 'before'], ['Created log', 'new', 'after']]);
    expect(records[0].fields.every((f) => f.op === 'removed' && f.after === undefined)).toBe(true);
    expect(records[2].fields.every((f) => f.op === 'added' && f.before === undefined)).toBe(true);
    expect(records[2].fields.some((f) => f.after === '"Created log"')).toBe(true);
  });

  it('flags only medium-confidence matches and reports changes it cannot place', () => {
    const { records, unplaced } = recordViews(diff({ changes: [
      { entity: first, path: 'body', op: 'modified', before: 'Login', after: 'LOGIN', confidence: 'medium' },
      { entity: first, path: 'severity_text', op: 'modified', before: 'INFO', after: 'info', confidence: 'high' },
      { entity: 'resource[9]', path: 'attributes["x"]', op: 'added', after: 'y' },
    ] }), before, before);
    expect(records[0].confidence).toBe('medium');
    expect(unplaced.map((c) => c.entity)).toEqual(['resource[9]']);
  });

  it('maps diff paths to the OTTL path of each signal', () => {
    expect(ottlPath('logs', 'record', 'body')).toBe('log.body');
    expect(ottlPath('traces', 'record', 'status.code')).toBe('span.status.code');
    expect(ottlPath('traces', 'record', 'attributes["http.route"]')).toBe('span.attributes["http.route"]');
    expect(ottlPath('metrics', 'record', 'metric.name')).toBe('metric.name');
    expect(ottlPath('metrics', 'record', 'attributes["a"]', undefined, true)).toBe('datapoint.attributes["a"]');
    expect(ottlPath('metrics', 'record', 'value', 'Int', true)).toBe('datapoint.value_int');
    expect(ottlPath('metrics', 'record', 'value', 'Double', true)).toBe('datapoint.value_double');
    expect(ottlPath('logs', 'resource', 'attributes["k"]')).toBe('resource.attributes["k"]');
    expect(ottlPath('logs', 'scope', 'name')).toBe('instrumentation_scope.name');
  });

  it('applies metric-level edits to each of the metric\'s points', () => {
    const metrics = { resourceMetrics: [{ scopeMetrics: [{ metrics: [{ name: 'requests', unit: '1', sum: { dataPoints: [{ asInt: '3' }, { asInt: '7' }] } }] }] }] };
    const metric = 'resource[0].scope[0].metric[0]';
    const { records } = recordViews(diff({ changes: [
      { entity: metric, path: 'metric.name', op: 'modified', before: 'requests', after: 'reqs' },
      { entity: `${metric}.data_point[1]`, path: 'attributes["env"]', op: 'added', after: 'prod' },
    ] }), metrics, metrics);
    expect(records).toHaveLength(2);
    expect(records.map((r) => r.state)).toEqual(['edited', 'edited']);
    expect(records.map((r) => field(r, 'datapoint.attributes["env"]')?.op)).toEqual([undefined, 'added']);
    expect(records.every((r) => field(r, 'metric.name').after === '"reqs"')).toBe(true);
  });

  it('names the statements that made each change from an OTTL replay', () => {
    const report = { groups: [{ index: 0, statements: [
      { index: 0, changes: [{ entity: first, path: 'attributes["env"]', op: 'added' }] },
      { index: 1, changes: [{ entity: first, path: 'attributes["user.id"]', op: 'modified' }, { entity: first, path: 'attributes["env"]', op: 'modified' }] },
    ] }] } as unknown as OTTLReport;
    expect(statementLabels(report).get(`${first}\u0000attributes["env"]`)).toBe('S0, S1');
    const { records } = recordViews(diff({ changes: [{ entity: first, path: 'attributes["user.id"]', op: 'modified', before: 'jane', after: 'x' }] }), before, before, report);
    expect(field(records[0], 'log.attributes["user.id"]').by).toBe('S1');
  });

  it('gives a dropped input record and a created output record at the same position distinct ids', () => {
    const output = { resourceLogs: [{ scopeLogs: [{ logRecords: [log('Created log')] }] }] };
    const { records } = recordViews(diff({ removed: [{ ref: first, name: 'Login' }], added: [{ ref: first, name: 'Created log' }] }), before, output);
    expect(new Set(records.map((r) => r.id)).size).toBe(records.length);
  });

  it('lists the records of both snapshots as recorded when there is no diff', () => {
    const { records } = recordViews(undefined, before, before);
    expect(records.map((r) => [r.side, r.state, r.changed])).toEqual([['before', 'recorded', 0], ['before', 'recorded', 0], ['after', 'recorded', 0], ['after', 'recorded', 0]]);
  });
});
