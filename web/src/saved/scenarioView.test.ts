import { describe, expect, it } from 'vitest';
import { assertionCount, scenarioSummary } from './scenarioView';

const SCENARIO = `version: 1
name: checkout
inputs:
  - receiver: otlp
    file: input.json
    after: 1s
mocks:
  files:
    - path: /var/log/app.log
      content: |
        hello
  kubernetes:
    objects:
      - kind: Pod
        metadata: {name: web-1, namespace: shop}
environment:
  TOKEN: s3cret
execution:
  timeout: 30s
assertions:
  - name: pager gets the error
    target: {exporter: otlp/pager, signal: logs}
    count: {equals: 1}
  - target: {exporter: otlp/archive, signal: logs}
    count: {equals: 2}
`;

describe('assertionCount', () => {
  it.each([
    [SCENARIO, 2],
    ['version: 1\nname: none\n', 0],
    ['', 0],
    ['assertions: [', undefined],
  ])('%#', (text, want) => expect(assertionCount(text)).toBe(want));
});

describe('scenarioSummary', () => {
  it('lists what the scenario holds', () => {
    const s = scenarioSummary(SCENARIO);
    if ('error' in s) throw new Error(s.error);
    expect(s.inputs).toEqual([{ text: '1. otlp', detail: 'file input.json · after 1s' }]);
    expect(s.assertions).toHaveLength(2);
    expect(s.assertions[0].text).toBe('pager gets the error');
    expect(s.assertions[0].detail).toContain('otlp/pager');
    expect(s.assertions[1].text).toContain('otlp/archive');
    expect(s.settings).toEqual([
      { text: 'Name', detail: 'checkout' },
      { text: 'Timeout', detail: '30s' },
    ]);
  });

  it('never shows environment values', () => {
    const s = scenarioSummary(SCENARIO);
    if ('error' in s) throw new Error(s.error);
    expect(JSON.stringify(s)).not.toContain('s3cret');
    expect(s.mocks.map((m) => m.text)).toEqual(['File /var/log/app.log', 'Pod shop/web-1', 'Environment TOKEN']);
  });

  it('reports text it cannot read', () => {
    expect('error' in scenarioSummary('a: [')).toBe(true);
  });
});
