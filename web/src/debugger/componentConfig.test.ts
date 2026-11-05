import { describe, expect, it } from 'vitest';
import type { Graph, TopologyComponent } from '../api/types';
import { asWritten, howItRuns, statementCount } from './componentConfig';

const yaml = `receivers:
  otlp:
    protocols:
      grpc: # default port
        endpoint: 0.0.0.0:4317
processors:
  batch:
  transform/normalize:
    log_statements:
      - set(severity_text, "ERROR")
exporters:
  debug: { verbosity: detailed }
`;

describe('configuration as written', () => {
  it('returns a component block with its comments, without the common indent', () => {
    expect(asWritten(yaml, 'receiver', 'otlp')).toBe('protocols:\n  grpc: # default port\n    endpoint: 0.0.0.0:4317');
    expect(asWritten(yaml, 'processor', 'transform/normalize')).toBe('log_statements:\n  - set(severity_text, "ERROR")');
    expect(asWritten(yaml, 'exporter', 'debug')).toBe('{ verbosity: detailed }');
  });
  it('is empty for a component without settings and undefined when it cannot be found', () => {
    expect(asWritten(yaml, 'processor', 'batch')).toBe('');
    expect(asWritten(yaml, 'processor', 'nope')).toBeUndefined();
    expect(asWritten(yaml, 'connector', 'x')).toBeUndefined();
    expect(asWritten('a: [', 'receiver', 'otlp')).toBeUndefined();
  });
});

const c = (over: Partial<TopologyComponent>): TopologyComponent => ({ id: 'k8sattributes', kind: 'processor', type: 'k8sattributes', ref: 'processor/k8sattributes', mode: 'real', pipelines: ['logs/ingest'], ...over });
const graph = { pipelines: [{ id: 'logs/ingest', signal: 'logs', processors: ['processor/k8sattributes', 'processor/batch'] }] } as unknown as Graph;

describe('how a component will run', () => {
  it('explains each execution mode', () => {
    expect(howItRuns(c({}), graph).title).toBe('Real component code');
    const mocked = howItRuns(c({ mode: 'dependency_mocked', note: 'pods come from a mock' }), graph);
    expect(mocked.title).toBe('Real component code, mocked environment');
    expect(mocked.body).toContain('pods come from a mock.');
    expect(howItRuns(c({ kind: 'exporter', id: 'otlp/pager', mode: 'captured' })).title).toBe('Output captured, not sent');
    expect(howItRuns(c({ mode: 'simulated' })).title).toBe('Simulated');
  });
  it('warns that a stand-in does not do the real work', () => {
    const h = howItRuns(c({ id: 'acme/enrich', mode: 'stand_in' }));
    expect(h.warn).toBe(true);
    expect(h.available.ok).toBe(false);
    expect(h.body).toContain('would not show what production does');
  });
  it('places a processor in its pipelines and lists stand-in values', () => {
    const h = howItRuns(c({ stand_in_values: ['env:TOKEN'] }), graph);
    expect(h.pipelines).toEqual([{ id: 'logs/ingest', position: 'position 1 of 2 processors' }]);
    expect(h.facts).toEqual([['Stand-in values', '${env:TOKEN}']]);
    expect(h.available.ok).toBe(true);
  });
});

describe('statement count', () => {
  it('sums the replayed statements of a processor', () => {
    const ottl = [{ processor: 'transform/a', input: 1, signal: 'logs', verification: 'replay_verified', groups: [{ index: 0, context: 'log', error_mode: 'ignore', status: 'ok', statements: [{}, {}] }, { index: 1, context: 'log', error_mode: 'ignore', status: 'ok', statements: [{}] }] }];
    expect(statementCount(ottl as never, 'transform/a')).toBe(3);
    expect(statementCount(ottl as never, 'transform/b')).toBeUndefined();
    expect(statementCount(undefined, 'x')).toBeUndefined();
  });
});
