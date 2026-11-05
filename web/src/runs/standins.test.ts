import { describe, expect, it } from 'vitest';
import type { Report } from '../api/types';
import { standIns } from './standins';

const report = (r: Partial<Report>): Report => ({ report_schema: '0.7', ...r }) as Report;

describe('standIns', () => {
  it('is empty without stand-ins', () => {
    expect(standIns(report({ references: [{ scheme: 'env', target: 'A', source: 'host' }] }))).toEqual({ components: [], references: [] });
    expect(standIns(undefined)).toEqual({ components: [], references: [] });
  });
  it('names stand-in components, stand-in values and unavailable references with their remedy', () => {
    const s = standIns(report({
      topology: { components: [
        { ref: 'connector/servicegraph', kind: 'connector', id: 'servicegraph', type: 'servicegraph', mode: 'stand_in', note: 'not in the distribution; a stand-in forwards data of its own signal' },
        { ref: 'exporter/otlp', kind: 'exporter', id: 'otlp', type: 'otlp', mode: 'captured', stand_in_values: ['env:TOKEN'] },
        { ref: 'receiver/otlp', kind: 'receiver', id: 'otlp', type: 'otlp', mode: 'simulated' },
      ] } as never,
      references: [{ scheme: 'env', target: 'TOKEN', source: 'stand_in', reason: 'unbound', stand_in: 'standin-ab12' }],
    }));
    expect(s.components).toEqual([
      { subject: 'connector/servicegraph', text: 'ran as a stand-in: not in the distribution; a stand-in forwards data of its own signal.' },
      { subject: 'exporter/otlp', text: 'ran with stand-in values for ${env:TOKEN}.' },
    ]);
    expect(s.references).toEqual([{
      subject: '${env:TOKEN}',
      text: 'was unavailable (unbound); the stand-in "standin-ab12" was used. Declare it under the scenario’s environment; the server reads no host environment variable.',
    }]);
  });
});
