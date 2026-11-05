import { describe, expect, it } from 'vitest';
import { locateComponent, yamlSyntaxErrors } from './locate';

const config = `receivers:
  otlp:
    protocols: {grpc: {}}
processors:
  transform/logs:
    log_statements: []
exporters:
  debug: {}
service:
  pipelines:
    logs:
      receivers: [otlp]
      exporters: [debug]
`;

describe('locateComponent', () => {
  it.each([
    ['otlp', 2],
    ['transform/logs', 5],
    ['debug', 8],
    ['logs', 11],
  ])('finds %s on line %i', (id, line) => {
    expect(locateComponent(config, id)?.startLine).toBe(line);
  });

  it('returns undefined for unknown components and broken YAML', () => {
    expect(locateComponent(config, 'nope')).toBeUndefined();
    expect(locateComponent('a: [', 'a')).toBeUndefined();
  });
});

describe('yamlSyntaxErrors', () => {
  it('locates a syntax error', () => {
    const errs = yamlSyntaxErrors('receivers:\n  otlp: [\n');
    expect(errs.length).toBeGreaterThan(0);
    expect(errs[0].location.startLine).toBeGreaterThanOrEqual(2);
  });
  it('is empty for valid YAML', () => {
    expect(yamlSyntaxErrors(config)).toEqual([]);
  });
});
