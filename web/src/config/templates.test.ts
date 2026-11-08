import { describe, expect, it } from 'vitest';
import { parse } from 'yaml';
import { addSignalPipeline, COMPONENT_SNIPPETS, CONFIG_TEMPLATES, starterConfig, defaultPipelines, insertComponent, lineOf, pipelinesOf, type ComponentSnippet } from './templates';

const snippet = (kind: string, type: string) => COMPONENT_SNIPPETS.find((s) => s.kind === kind && s.type === type) as ComponentSnippet;

const config = `# Clean application logs.
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

processors:
  resource:
    attributes:
      - key: deployment.environment
        value: demo # a comment
        action: upsert
  batch:

exporters:
  otlp/backend:
    endpoint: backend.example.invalid:4317

service:
  pipelines:
    logs:
      receivers: [otlp]
      processors: [resource, batch]
      exporters: [otlp/backend]
    traces:
      receivers: [otlp]
      exporters: [otlp/backend]
`;

function inserted(text: string, s: ComponentSnippet, into: string[]) {
  const r = insertComponent(text, s, into);
  if ('error' in r) throw new Error(r.error);
  return r;
}

describe('starting configurations', () => {
  it('are pipelines from OTLP to debug', () => {
    for (const t of CONFIG_TEMPLATES) {
      const v = parse(t.yaml);
      expect(Object.keys(v.receivers), t.id).toEqual(['otlp']);
      for (const p of Object.values(v.service.pipelines) as {
        exporters: string[];
      }[])
        expect(p.exporters).toEqual(['debug']);
    }
    expect(pipelinesOf(CONFIG_TEMPLATES.find((t) => t.id === 'all')!.yaml).map((p) => p.signal)).toEqual(['logs', 'traces', 'metrics']);
  });
});

describe('insertComponent', () => {
  it('adds a processor before batch and keeps everything else as written', () => {
    const r = inserted(config, snippet('processor', 'transform'), ['logs']);
    expect(r.id).toBe('transform');
    expect(r.yaml).toContain('      processors: [resource, transform, batch]\n');
    for (const line of config.split('\n').filter((l) => !l.includes('processors: ['))) expect(r.yaml).toContain(line);
    expect(r.yaml.split('\n')[r.line - 1]).toBe('  transform:');
    const v = parse(r.yaml);
    expect(v.processors.transform.log_statements[0].context).toBe('log');
    expect(v.processors.transform.trace_statements).toBeUndefined();
  });

  it('writes the body for the signals of the chosen pipelines', () => {
    const v = parse(inserted(config, snippet('processor', 'filter'), ['logs', 'traces']).yaml);
    expect(Object.keys(v.processors.filter)).toEqual(['error_mode', 'logs', 'traces']);
  });

  it('names a second instance and puts memory_limiter first', () => {
    const twice = inserted(inserted(config, snippet('processor', 'memory_limiter'), ['logs']).yaml, snippet('processor', 'memory_limiter'), ['traces']);
    expect(twice.id).toBe('memory_limiter/2');
    const v = parse(twice.yaml);
    expect(v.service.pipelines.logs.processors).toEqual(['memory_limiter', 'resource', 'batch']);
    expect(v.service.pipelines.traces.processors).toEqual(['memory_limiter/2']);
  });

  it('adds receivers and exporters to the chosen pipelines only', () => {
    const v = parse(inserted(config, snippet('exporter', 'debug'), ['traces']).yaml);
    expect(v.exporters.debug).toEqual({ verbosity: 'detailed' });
    expect(v.service.pipelines.traces.exporters).toEqual(['otlp/backend', 'debug']);
    expect(v.service.pipelines.logs.exporters).toEqual(['otlp/backend']);
  });

  it('wires a counting connector into a new pipeline and keeps the export', () => {
    const r = inserted(config, snippet('connector', 'count'), ['logs']);
    expect(r.created).toEqual(['metrics/counts']);
    const v = parse(r.yaml);
    expect(Object.keys(v)).toEqual(['receivers', 'processors', 'exporters', 'connectors', 'service']);
    expect(v.connectors.count.logs).toBeDefined();
    expect(v.service.pipelines.logs.exporters).toEqual(['otlp/backend', 'count']);
    expect(v.service.pipelines['metrics/counts']).toEqual({
      receivers: ['count'],
      exporters: ['otlp/backend'],
    });
  });

  it('lets a router take over the exports', () => {
    const v = parse(inserted(config, snippet('connector', 'routing'), ['logs']).yaml);
    expect(v.service.pipelines.logs.exporters).toEqual(['routing']);
    expect(v.connectors.routing.default_pipelines).toEqual(['logs/default']);
    expect(v.connectors.routing.table[0].pipelines).toEqual(['logs/routed']);
    expect(v.service.pipelines['logs/routed'].exporters).toEqual(['otlp/backend']);
  });

  it('starts from empty or comment-only text', () => {
    const r = inserted('# mine\n', snippet('receiver', 'otlp'), []);
    expect(r.yaml.startsWith('# mine\n')).toBe(true);
    expect(parse(r.yaml).receivers.otlp.protocols).toBeDefined();
    expect(parse(inserted('', snippet('exporter', 'nop'), []).yaml)).toEqual({
      exporters: { nop: {} },
    });
  });

  it('refuses text it cannot read', () => {
    expect(insertComponent('a: [', snippet('exporter', 'nop'), [])).toHaveProperty('error');
    expect(insertComponent('- a\n', snippet('exporter', 'nop'), [])).toHaveProperty('error');
  });
});

describe('helpers', () => {
  it('chooses the pipelines whose signal a component accepts', () => {
    expect(defaultPipelines(snippet('processor', 'tail_sampling'), pipelinesOf(config))).toEqual(['traces']);
    expect(defaultPipelines(snippet('processor', 'batch'), pipelinesOf(config))).toEqual(['logs', 'traces']);
  });

  it('finds a key in a section', () => {
    expect(lineOf(config, 'exporters', 'otlp/backend')).toBe(17);
    expect(lineOf(config, 'exporters', 'missing')).toBe(1);
  });
});

describe('section spacing', () => {
  it('puts one blank line around a new section', () => {
    const text = 'exporters:\n  debug:\n\nservice:\n  pipelines:\n    logs:\n      receivers: [otlp]\n      exporters: [debug]\n';
    const r = inserted(text, snippet('connector', 'forward'), []);
    expect(r.yaml).toBe('exporters:\n  debug:\n\nconnectors:\n  forward: {}\n\nservice:\n  pipelines:\n    logs:\n      receivers: [otlp]\n      exporters: [debug]\n');
    const v = inserted(config, snippet('connector', 'forward'), []).yaml;
    expect(v).toContain('backend.example.invalid:4317\n\nconnectors:\n  forward: {}\n\nservice:');
  });
});

describe('addSignalPipeline', () => {
  it('reuses the OTLP receiver and debug exporter and keeps comments', () => {
    const text = `# mine\n${starterConfig(['logs'])}`;
    const r = addSignalPipeline(text, 'metrics');
    if ('error' in r) throw new Error(r.error);
    expect(r).toMatchObject({ pipeline: 'metrics', receiver: 'otlp' });
    expect(r.yaml).toContain('# mine');
    const c = parse(r.yaml);
    expect(c.service.pipelines.metrics).toEqual({ receivers: ['otlp'], exporters: ['debug'] });
    expect(c.service.pipelines.logs.processors).toEqual(['batch']);
  });
  it('adds a receiver and an exporter when the configuration has none', () => {
    const r = addSignalPipeline('receivers:\n  filelog: {include: [a]}\nexporters:\n  otlp/out: {endpoint: x}\nservice:\n  pipelines:\n    logs:\n      receivers: [filelog]\n      exporters: [otlp/out]\n', 'traces');
    if ('error' in r) throw new Error(r.error);
    const c = parse(r.yaml);
    expect(Object.keys(c.receivers)).toEqual(['filelog', 'otlp']);
    expect(c.exporters.debug).toEqual({ verbosity: 'detailed' });
    expect(c.service.pipelines.traces).toEqual({ receivers: ['otlp'], exporters: ['debug'] });
  });
  it('names a second pipeline of a signal apart and refuses unreadable text', () => {
    const r = addSignalPipeline(starterConfig(['logs']), 'logs');
    expect(r).toMatchObject({ pipeline: 'logs/2' });
    expect(addSignalPipeline('a: [', 'logs')).toHaveProperty('error');
    const blank = addSignalPipeline('', 'logs');
    if ('error' in blank) throw new Error(blank.error);
    expect(parse(blank.yaml).service.pipelines.logs.receivers).toEqual(['otlp']);
  });
});
