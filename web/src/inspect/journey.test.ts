import { describe, expect, it } from 'vitest';
import type { Change, Event, RecordLineage } from '../api/types';
import { confidenceLabel, findLineage, hopTree, pathHighlight, routeMatrix } from './journey';
import { groupChanges } from './grouped';

const ev = (seq: number, kind: string, extra: Partial<Event> = {}): Event =>
  ({ seq, kind, id: String(seq), run_id: 'r', source: 'processor_wrapper', time: '', ...extra }) as Event;

// The log-routing sample: an error log is enriched, then copied by the
// routing connector to the archive and to the pager pipeline, which redacts it.
const events = [
  ev(1, 'input.injected', { component: 'otlp' }),
  ev(2, 'stage.input', { component: 'resource/env', pipeline: 'logs/in', from_seq: 1 }),
  ev(3, 'stage.output', { component: 'resource/env', pipeline: 'logs/in', input_seq: 2 }),
  ev(4, 'stage.input', { component: 'routing', source: 'connector_wrapper', from_seq: 3 }),
  ev(5, 'stage.output', { component: 'routing', source: 'connector_wrapper', pipeline: 'logs/archive', input_seq: 4 }),
  ev(6, 'exporter.captured', { component: 'otlp/archive', from_seq: 5 }),
  ev(7, 'stage.output', { component: 'routing', source: 'connector_wrapper', pipeline: 'logs/pager', input_seq: 4 }),
  ev(8, 'stage.input', { component: 'transform/page', pipeline: 'logs/pager', from_seq: 7 }),
  ev(9, 'stage.output', { component: 'transform/page', pipeline: 'logs/pager', input_seq: 8 }),
  ev(10, 'exporter.captured', { component: 'otlp/pager', from_seq: 9 }),
];
const bySeq = new Map(events.map((e) => [e.seq, e]));
const ref = 'resource[0].scope[0].log_record[0]';
const change = (path: string, op: string): Change => ({ entity: ref, path, op });

const error: RecordLineage = {
  id: `1:${ref}`,
  name: 'card declined',
  receiver: 'otlp',
  signal: 'logs',
  hops: [
    { parent: -1, seq: 1, kind: 'input.injected', component: 'otlp', ref },
    { parent: 0, seq: 2, kind: 'stage.input', component: 'resource/env', pipeline: 'logs/in', ref },
    {
      parent: 1,
      seq: 3,
      kind: 'stage.output',
      component: 'resource/env',
      pipeline: 'logs/in',
      ref,
      confidence: 'exact',
      changes: [{ entity: 'resource[0]', path: 'attributes["deployment.environment"]', op: 'added', after: 'prod' }],
    },
    { parent: 2, seq: 4, kind: 'stage.input', component: 'routing', ref },
    { parent: 3, seq: 5, kind: 'stage.output', component: 'routing', pipeline: 'logs/archive', ref },
    { parent: 4, seq: 6, kind: 'exporter.captured', component: 'otlp/archive', ref, end: 'delivered' },
    { parent: 3, seq: 7, kind: 'stage.output', component: 'routing', pipeline: 'logs/pager', ref },
    { parent: 6, seq: 8, kind: 'stage.input', component: 'transform/page', pipeline: 'logs/pager', ref },
    {
      parent: 7,
      seq: 9,
      kind: 'stage.output',
      component: 'transform/page',
      pipeline: 'logs/pager',
      ref,
      confidence: 'high',
      changes: [change('attributes["page.priority"]', 'added'), change('attributes["user.email"]', 'removed')],
    },
    { parent: 8, seq: 10, kind: 'exporter.captured', component: 'otlp/pager', ref, end: 'delivered' },
  ],
  deliveries: [
    { exporter: 'otlp/archive', pipeline: 'logs/archive', hop: 5, changed: true },
    { exporter: 'otlp/pager', pipeline: 'logs/pager', hop: 9, changed: true },
  ],
};
const debug: RecordLineage = {
  id: '1:resource[0].scope[0].log_record[2]',
  name: 'cache miss',
  receiver: 'otlp',
  signal: 'logs',
  stops: [{ component: 'filter/debug', pipeline: 'logs/archive', hop: 6, end: 'dropped' }],
};

describe('journey', () => {
  it('places a journey on the graph', () => {
    const h = pathHighlight(error, bySeq);
    expect(Object.fromEntries(h.nodes)).toEqual({
      'receiver/otlp': 'passed',
      'logs/in|processor/resource/env': 'changed',
      'connector/routing': 'passed',
      'exporter/otlp/archive': 'delivered',
      'logs/pager|processor/transform/page': 'changed',
      'exporter/otlp/pager': 'delivered',
    });
    expect([...h.edges].sort()).toEqual([
      'connector/routing->exporter/otlp/archive',
      'connector/routing->logs/pager|processor/transform/page',
      'logs/in|processor/resource/env->connector/routing',
      'logs/pager|processor/transform/page->exporter/otlp/pager',
      'receiver/otlp->logs/in|processor/resource/env',
    ]);
    expect([...h.pipelines].sort()).toEqual(['logs/archive', 'logs/in', 'logs/pager']);
  });

  it('marks an aggregating connector as where a record was counted, not stopped', () => {
    const counted: RecordLineage = {
      id: `1:${ref}`,
      name: 'card declined',
      receiver: 'otlp',
      signal: 'logs',
      hops: [
        { parent: -1, seq: 1, kind: 'input.injected', component: 'otlp', ref },
        { parent: 0, seq: 4, kind: 'stage.input', component: 'routing', ref, end: 'aggregated', into: ['5:p'] },
      ],
    };
    expect(pathHighlight(counted, bySeq).nodes.get('connector/routing')).toBe('counted');
  });

  it('branches where a connector copies the record', () => {
    const [root] = hopTree(error);
    const connector = root.children[0].children[0].children[0];
    expect(connector.hop.component).toBe('routing');
    expect(connector.children.map((c) => c.hop.pipeline)).toEqual(['logs/archive', 'logs/pager']);
  });

  it('finds the journey of a record shown in a stage', () => {
    expect(findLineage([debug, error], 8, ref)?.name).toBe('card declined');
    expect(findLineage([debug, error], 8, 'resource[0].scope[0].log_record[1]')).toBeUndefined();
  });

  it('tabulates where each record went', () => {
    const m = routeMatrix([error, debug]);
    expect(m.exporters).toEqual(['otlp/archive', 'otlp/pager']);
    expect(m.rows.map((r) => [r.name, r.cells, r.stops])).toEqual([
      ['card declined', { 'otlp/archive': 'changed', 'otlp/pager': 'changed' }, []],
      ['cache miss', {}, [{ component: 'filter/debug', end: 'dropped' }]],
    ]);
  });
});

describe('grouped changes', () => {
  const before = {
    resourceLogs: [
      {
        resource: { attributes: [{ key: 'service.name', value: { stringValue: 'payments' } }] },
        scopeLogs: [
          {
            scope: { name: 'app' },
            logRecords: [
              {
                body: { stringValue: 'card declined' },
                attributes: [
                  { key: 'user.email', value: { stringValue: 'jane@example.com' } },
                  { key: 'order.id', value: { intValue: '7' } },
                ],
              },
              { body: { stringValue: 'cache miss' } },
            ],
          },
        ],
      },
    ],
  };

  it('groups changes under resource, scope and record with their untouched attributes', () => {
    const g = groupChanges(
      {
        identity: 'content',
        before: { resources: 1, scopes: 1, items: 2 },
        after: { resources: 1, scopes: 1, items: 1 },
        changes: [
          { entity: 'resource[0]', path: 'attributes["deployment.environment"]', op: 'added', after: 'prod' },
          change('attributes["user.email"]', 'removed'),
          change('attributes["page.priority"]', 'added'),
        ],
        removed: [{ ref: 'resource[0].scope[0].log_record[1]', name: 'cache miss' }],
      },
      before,
    );
    expect(g.summary).toMatchObject({ resources: 1, scopes: 0, records: 1, added: 2, removed: 1, modified: 0, dropped: 1, created: 0 });
    expect(g.resources).toHaveLength(1);
    const [r] = g.resources;
    expect(r.label).toBe('service.name = payments');
    expect(r.resource?.unchanged).toEqual([{ key: 'service.name', value: 'payments' }]);
    expect(r.scopes[0].records.map((x) => [x.label, x.dropped ?? false, x.unchanged])).toEqual([
      ['card declined', false, [{ key: 'order.id', value: '7' }]],
      ['cache miss', true, []],
    ]);
  });
});

describe('confidenceLabel', () => {
  it('words span IDs, record objects and content matches', () => {
    expect(confidenceLabel('id')).toBe('same span ID');
    expect(confidenceLabel('object')).toBe('same record');
    expect(confidenceLabel('exact')).toBe('exact match');
    expect(confidenceLabel('medium')).toBe('medium match');
  });
});
