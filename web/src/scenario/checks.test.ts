import { describe, expect, it } from 'vitest';
import { parse } from 'yaml';
import type { Report } from '../api/types';
import {
  appendAssertion,
  ASSERTION_TEMPLATES,
  attributeKeys,
  checkProblem,
  conflicts,
  describeAssertion,
  readChecks,
  setChecks,
  suggestFromRun,
  templateTarget,
  type Check,
} from './checks';
import { editScenario, readScenario, type TargetChoice } from './scenarioDoc';

const edit = (text: string, e: Parameters<typeof editScenario>[1]) => {
  const r = editScenario(text, e);
  if ('error' in r) throw new Error(r.error);
  return r.yaml;
};

const scenario = `version: 1
# my scenario
assertions:
  - name: cleaned
    target: {exporter: otlp/backend, signal: logs}
    count: {equals: 2}
    attributes:
      absent: [user.email]
      equals: {http.status_code: 200, ratio: 2.0, ok: true, code: "200"}
      types: {size: int}
    resource_attributes:
      present: [service.name]
`;

const conditions = (text: string) => readScenario(text) as { model: { assertions: { conditions: string }[] } };

describe('readChecks', () => {
  it('reads every condition as a row, keeping value types', () => {
    const r = readChecks(conditions(scenario).model.assertions[0].conditions);
    expect(r).toEqual({
      checks: [
        { kind: 'count', bound: { op: 'equals', n: 2 } },
        { kind: 'attribute', scope: 'attributes', rule: 'absent', key: 'user.email' },
        { kind: 'attribute', scope: 'attributes', rule: 'equals', key: 'http.status_code', value: '200', valueType: 'int' },
        { kind: 'attribute', scope: 'attributes', rule: 'equals', key: 'ratio', value: '2', valueType: 'double' },
        { kind: 'attribute', scope: 'attributes', rule: 'equals', key: 'ok', value: 'true', valueType: 'bool' },
        { kind: 'attribute', scope: 'attributes', rule: 'equals', key: 'code', value: '200', valueType: 'string' },
        { kind: 'attribute', scope: 'attributes', rule: 'types', key: 'size', value: 'int' },
        { kind: 'attribute', scope: 'resource_attributes', rule: 'present', key: 'service.name' },
      ],
    });
  });

  it('reads ranges', () => {
    expect(readChecks('count: {min: 1, max: 3}\ndropped: {max: 0}\n')).toEqual({
      checks: [
        { kind: 'count', bound: { op: 'between', n: 1, m: 3 } },
        { kind: 'dropped', bound: { op: 'max', n: 0 } },
      ],
    });
  });

  it('leaves shapes rows would lose to the YAML', () => {
    for (const text of ['count: {equals: 1} # why', 'count: {equals: 1, min: 0}', 'count: 3', 'attributes: {present: user}', 'attributes: {equals: {a: [1]}}'])
      expect(readChecks(text), text).toHaveProperty('unsupported');
  });
});

describe('setChecks', () => {
  it('writes rows back without touching the rest of the scenario', () => {
    const r = readChecks(conditions(scenario).model.assertions[0].conditions) as { checks: Check[] };
    const again = edit(scenario, setChecks(0, r.checks));
    expect(again).toContain('# my scenario');
    expect(again).toContain('    count: { equals: 2 }\n');
    expect(parse(again)).toEqual(parse(scenario));
  });

  it('writes typed values the Go decoder reads the same way', () => {
    const y = edit(
      scenario,
      setChecks(0, [
        { kind: 'attribute', scope: 'attributes', rule: 'equals', key: 'a', value: '200', valueType: 'string' },
        { kind: 'attribute', scope: 'attributes', rule: 'equals', key: 'b', value: '3', valueType: 'double' },
        { kind: 'attribute', scope: 'attributes', rule: 'equals', key: 'c', value: 'false', valueType: 'bool' },
        { kind: 'attribute', scope: 'attributes', rule: 'equals', key: 'd', value: '-4', valueType: 'int' },
        { kind: 'attribute', scope: 'attributes', rule: 'equals', key: 'e', value: '', valueType: 'string' },
      ]),
    );
    expect(y).toContain('a: "200"');
    expect(y).toContain('b: 3.0');
    expect(parse(y).assertions[0].attributes.equals).toEqual({ a: '200', b: 3, c: false, d: -4, e: '' });
    expect(readChecks(conditions(y).model.assertions[0].conditions)).toMatchObject({ checks: [{ valueType: 'string' }, { valueType: 'double' }, {}, {}, {}] });
  });

  it('removes every condition when no rows are left', () => {
    expect(parse(edit(scenario, setChecks(0, []))).assertions[0]).toEqual({ name: 'cleaned', target: { exporter: 'otlp/backend', signal: 'logs' } });
  });
});

const choices: TargetChoice[] = [
  { label: 'exporter otlp/backend (logs)', target: { exporter: 'otlp/backend', signal: 'logs' } },
  { label: 'processor transform in logs', target: { processor: 'transform', pipeline: 'logs', signal: 'logs' } },
  { label: 'processor filter/noise in logs', target: { processor: 'filter/noise', pipeline: 'logs', signal: 'logs' } },
];

describe('templates', () => {
  it('start from a fitting target', () => {
    const t = (id: string) =>
      templateTarget(
        ASSERTION_TEMPLATES.find((x) => x.id === id)!,
        choices,
      );
    expect(t('count')).toEqual(choices[0].target);
    expect(t('drops')).toEqual(choices[2].target);
    expect(templateTarget(ASSERTION_TEMPLATES[0], [])).toBeUndefined();
  });

  it('append an assertion the scenario reads', () => {
    const y = edit('', appendAssertion(choices[2].target, ASSERTION_TEMPLATES.find((x) => x.id === 'drops')!.checks));
    expect(parse(y)).toEqual({
      version: 1,
      assertions: [{ target: { processor: 'filter/noise', pipeline: 'logs', signal: 'logs' }, dropped: { min: 1 } }],
    });
  });

  it('are described in words', () => {
    expect(describeAssertion(choices[0].target, [{ kind: 'count', bound: { op: 'equals', n: 1 } }])).toBe('1 log record reaches exporter otlp/backend');
    expect(describeAssertion(choices[0].target, [{ kind: 'attribute', scope: 'attributes', rule: 'absent', key: 'user.email' }])).toBe(
      'user.email is removed at otlp/backend',
    );
    expect(describeAssertion(choices[2].target, [{ kind: 'dropped', bound: { op: 'min', n: 1 } }])).toBe('processor filter/noise in logs drops at least 1');
  });
});

describe('row problems', () => {
  it('explain what is missing', () => {
    const e = choices[0].target;
    expect(checkProblem({ kind: 'dropped', bound: { op: 'equals', n: 0 } }, e)).toMatch(/processor/);
    expect(checkProblem({ kind: 'count', bound: { op: 'between', n: 3, m: 1 } }, e)).toMatch(/more than/);
    expect(checkProblem({ kind: 'count', bound: { op: 'equals', n: -1 } }, e)).toBeDefined();
    expect(checkProblem({ kind: 'attribute', scope: 'attributes', rule: 'present', key: ' ' }, e)).toMatch(/key/);
    expect(checkProblem({ kind: 'attribute', scope: 'attributes', rule: 'equals', key: 'a', value: '1.5', valueType: 'int' }, e)).toMatch(/whole/);
    expect(checkProblem({ kind: 'attribute', scope: 'attributes', rule: 'equals', key: 'a', value: '1.5', valueType: 'double' }, e)).toBeUndefined();
  });

  it('find contradictions', () => {
    const c = conflicts([
      { kind: 'attribute', scope: 'attributes', rule: 'present', key: 'a' },
      { kind: 'attribute', scope: 'resource_attributes', rule: 'absent', key: 'a' },
      { kind: 'attribute', scope: 'attributes', rule: 'absent', key: 'a' },
      { kind: 'count', bound: { op: 'min', n: 1 } },
      { kind: 'count', bound: { op: 'max', n: 1 } },
    ]);
    expect([...c.keys()]).toEqual([2, 4]);
  });
});

describe('suggestFromRun', () => {
  const report = {
    result: {
      mode: 'quick',
      status: 'completed',
      outputs: [
        { exporter: 'otlp/backend', signal: 'logs', batches: 1, counts: { items: 3, resources: 1, scopes: 1 } },
        { exporter: 'gone', signal: 'logs', batches: 1, counts: { items: 1, resources: 1, scopes: 1 } },
      ],
      events: [
        { kind: 'stage.input', component: 'filter/noise', pipeline: 'logs', signal: 'logs', counts: { items: 5 } },
        { kind: 'stage.output', component: 'filter/noise', pipeline: 'logs', signal: 'logs', counts: { items: 3 } },
        { kind: 'stage.input', component: 'transform', pipeline: 'logs', signal: 'logs', counts: { items: 3 } },
        { kind: 'stage.output', component: 'transform', pipeline: 'logs', signal: 'logs', counts: { items: 3 } },
      ],
    },
  } as unknown as Report;

  it('proposes exporter counts and processor drops for targets that still exist', () => {
    const s = suggestFromRun(report, choices, [{ target: choices[0].target, conditions: 'count: {min: 1}\n' }]);
    expect(s.map((x) => [x.key, x.checks, x.present])).toEqual([
      ['count|e|otlp/backend|logs', [{ kind: 'count', bound: { op: 'equals', n: 3 } }], true],
      ['dropped|p|filter/noise|logs|logs', [{ kind: 'dropped', bound: { op: 'equals', n: 2 } }], false],
    ]);
  });

  it('claims no drops from an incomplete run', () => {
    const partial = { result: { ...report.result!, status: 'partial' } } as Report;
    expect(suggestFromRun(partial, choices, []).map((x) => x.key)).toEqual(['count|e|otlp/backend|logs']);
    expect(suggestFromRun(undefined, choices, [])).toEqual([]);
  });

  it('proposes what a simulated endpoint received and words its target', () => {
    const target = { endpoint: { component: 'extension/livecapture', path: 'delivery' }, signal: 'logs' };
    const sent = {
      result: { status: 'completed', endpoint_outputs: [{ component: 'extension/livecapture', endpoint: 'delivery', signal: 'logs', batches: 1, counts: { items: 4, resources: 1, scopes: 1 } }] },
    } as unknown as Report;
    const s = suggestFromRun(sent, [...choices, { label: 'endpoint', target }], []);
    expect(s.map((x) => [x.key, x.checks])).toEqual([['count|n|extension/livecapture|delivery|logs', [{ kind: 'count', bound: { op: 'equals', n: 4 } }]]]);
    expect(describeAssertion(target, s[0].checks)).toBe('4 log records reach endpoint delivery of extension/livecapture');
  });
});

describe('attributeKeys', () => {
  it('lists record and resource keys of the inputs', () => {
    const p = {
      resourceLogs: [
        {
          resource: { attributes: [{ key: 'service.name', value: {} }] },
          scopeLogs: [{ scope: { attributes: [{ key: 'scope.k' }] }, logRecords: [{ attributes: [{ key: 'b' }, { key: 'a' }] }] }],
        },
      ],
    };
    expect(attributeKeys([p, 'x'])).toEqual({ attributes: ['a', 'b'], resource_attributes: ['service.name'] });
  });
});
