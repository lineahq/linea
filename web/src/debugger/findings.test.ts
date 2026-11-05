import { describe, expect, it } from 'vitest';
import type { Report } from '../api/types';
import { changeGroups, keyFindings, levelOf, nothingReached, stageDrops } from './findings';

const comp = (kind: string, id: string, type = id.split('/')[0]) => ({ kind, id, type, ref: `${kind}/${id}`, mode: 'real' });
const counts = (items: number) => ({ items, resources: 1, scopes: 1 });

/** Two logs reach filter/pager, which drops both; the archive pipeline delivers three. */
function report(over: Partial<Report> = {}): Report {
  return {
    report_schema: '0.3',
    topology: {
      components: [comp('receiver', 'otlp'), comp('processor', 'filter/pager'), comp('processor', 'transform/normalize'), comp('exporter', 'otlp/pager'), comp('exporter', 'otlp/archive')],
      pipelines: [
        { id: 'logs/pager', signal: 'logs', receivers: ['receiver/otlp'], processors: ['processor/filter/pager'], exporters: ['exporter/otlp/pager'] },
        { id: 'logs/archive', signal: 'logs', receivers: ['receiver/otlp'], processors: ['processor/transform/normalize'], exporters: ['exporter/otlp/archive'] },
      ],
    },
    result: {
      mode: 'scenario',
      status: 'completed',
      inputs: [{ receiver: 'otlp', signal: 'logs', injected: true, counts: counts(5) }],
      outputs: [{ exporter: 'otlp/archive', signal: 'logs', batches: 1, counts: counts(3) }],
    },
    stages: [
      {
        component: 'filter/pager', kind: 'processor', pipeline: 'logs/pager', type: 'filter', signal: 'logs', emission: 'x',
        inputs: [{ seq: 4, outcome: 'dropped', diff: { identity: 'x', before: counts(2), after: counts(0), removed: [{ name: 'a' }, { name: 'b' }] } }],
      },
      {
        component: 'transform/normalize', kind: 'processor', pipeline: 'logs/archive', type: 'transform', signal: 'logs', emission: 'x',
        inputs: [{
          seq: 6, outcome: 'passed',
          diff: {
            identity: 'x', before: counts(3), after: counts(3),
            changes: [
              { entity: 'resource[0].scope[0].log_record[0]', op: 'changed', path: 'severity_text', before: 'error', after: 'ERROR' },
              { entity: 'resource[0].scope[0].log_record[1]', op: 'changed', path: 'severity_text', before: 'error', after: 'ERROR' },
              { entity: 'resource[0].scope[0].log_record[0]', op: 'added', path: 'attributes["tier"]', after: 'p' },
            ],
          },
        }],
      },
    ],
    lineage: [
      { id: 'l1', name: 'card failed', signal: 'logs', receiver: 'otlp', stops: [{ component: 'filter/pager', end: 'dropped', hop: 1, pipeline: 'logs/pager' }] },
    ],
    ...over,
  } as unknown as Report;
}

const text = (segments?: (string | { code: string })[]) => segments?.map((s) => (typeof s === 'string' ? s : s.code)).join('') ?? '';

describe('key findings', () => {
  it('names an exporter that received nothing, and where records were dropped before it', () => {
    const f = keyFindings(report());
    expect(f[0].id).toBe('zero:exporter/otlp/pager');
    expect(text(f[0].title)).toBe('otlp/pager received 0 records');
    expect(text(f[0].detail)).toBe('2 records were dropped at filter/pager.');
    expect(f[0].actions).toEqual([
      { kind: 'follow', label: 'Follow a dropped record', trace: 'l1' },
      { kind: 'open', label: 'Open filter/pager', node: 'logs/pager|processor/filter/pager' },
    ]);
  });

  it('does not list the same drop twice, and reports what stages changed', () => {
    const f = keyFindings(report());
    expect(f.map((x) => x.id)).toEqual(['zero:exporter/otlp/pager', 'change:logs/archive|transform/normalize']);
    expect(text(f[1].title)).toBe('transform/normalize changed severity_text on 2 records');
    expect(text(f[1].detail)).toBe('"error" → "ERROR".');
  });

  it('says so when no stage recorded a drop ahead of an empty exporter', () => {
    const r = report();
    r.stages![0].inputs![0].diff!.removed = [];
    const f = keyFindings(r);
    expect(text(f[0].detail)).toBe('No stage ahead of it recorded dropping records.');
    expect(f[0].actions).toEqual([]);
  });

  it('reports a drop in a pipeline whose exporter still received records', () => {
    const r = report();
    r.result!.outputs!.push({ exporter: 'otlp/pager', signal: 'logs', batches: 1, counts: counts(1) });
    const f = keyFindings(r);
    expect(f[0].id).toBe('drop:logs/pager|filter/pager');
    expect(text(f[0].title)).toBe('filter/pager dropped 2 records');
    expect(text(f[0].detail)).toBe('Of 2 it received in logs/pager.');
  });

  it('claims nothing about missing output after an early end or for an unrelated signal', () => {
    expect(keyFindings(report({ result: { ...report().result!, status: 'partial' } })).some((x) => x.id.startsWith('zero:'))).toBe(false);
    const traces = report();
    traces.result!.inputs![0].signal = 'traces';
    expect(keyFindings(traces).some((x) => x.id.startsWith('zero:'))).toBe(false);
  });

  it('is empty without a report', () => {
    expect(keyFindings(undefined)).toEqual([]);
  });
});

describe('finding helpers', () => {
  it('counts drops from the stage input diffs', () => {
    expect(stageDrops(report().stages![0])).toEqual({ removed: 2, received: 2 });
  });
  it('lists drops ahead of an exporter', () => {
    const r = report();
    const n = nothingReached(r, r.topology!.components![3]);
    expect(n.dropped.map((d) => d.component)).toEqual(['filter/pager']);
    expect(n.trace).toBe('l1');
  });
  it('tells resource, scope and record changes apart', () => {
    expect(levelOf('resource[0]')).toBe('resource');
    expect(levelOf('resource[0].scope[1]')).toBe('scope');
    expect(levelOf('resource[0].scope[0].log_record[2]')).toBe('record');
    expect(levelOf('span:t:s')).toBe('record');
  });
  it('groups changes by operation and path, largest first', () => {
    const g = changeGroups([
      { entity: 'a', op: 'added', path: 'p', after: 1 },
      { entity: 'b', op: 'changed', path: 'q', before: 1, after: 2 },
      { entity: 'c', op: 'changed', path: 'q', before: 1, after: 3 },
    ]);
    expect(g.map((x) => [x.op, x.path, x.entities.size])).toEqual([['changed', 'q', 2], ['added', 'p', 1]]);
    expect(g[0].after.size).toBe(2);
  });
});
