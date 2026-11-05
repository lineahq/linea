import { describe, expect, it } from 'vitest';
import type { Report, Run } from '../api/types';
import { limitations } from './limitations';

const run = (report: Partial<Report>, state: Run['state'] = 'done'): Run => ({
  id: 'r', project_id: 'default',
  state,
  created: '2026-10-10T00:00:00Z',
  report: { report_schema: '0.2', ...report } as Report,
});

describe('limitations', () => {
  it('explains an incomplete run and substituted components', () => {
    const l = limitations(
      run({
        result: { mode: 'instrumented', status: 'partial', status_reason: 'deadline exceeded' },
        topology: {
          components: [{ ref: 'exporter/otlp', kind: 'exporter', id: 'otlp', type: 'otlp', mode: 'captured', note: 'recorded, not sent' }],
        },
      }),
    );
    expect(l.map((x) => x.level)).toEqual(['warning', 'info', 'info']);
    expect(l[0].text).toContain('missing output proves nothing');
    expect(l[1].text).toBe('exporter otlp is captured: recorded, not sent.');
    expect(l[2].text).toContain('No reference comparison');
  });

  it('reports a differing reference run and unverified replays', () => {
    const l = limitations(
      run({
        result: { mode: 'instrumented', status: 'completed' },
        comparison: { equal: false, differences: ['otlp/logs: 1 vs 2 items'] },
        ottl: [{ processor: 'transform', input: 4, signal: 'logs', verification: 'disagrees' }],
      }),
    );
    expect(l.map((x) => x.level)).toEqual(['warning', 'error']);
  });

  it('reports what each mocked dependency supplied and where it differs', () => {
    const l = limitations(
      run({
        result: {
          mode: 'instrumented',
          status: 'completed',
          mocks: [
            {
              provider: 'kubernetes',
              component: 'processor/k8sattributes',
              mode: 'dependency_mocked',
              limitations: ['objects do not change during the run'],
              warnings: ['the processor watches Node objects, but none are mocked'],
            },
          ],
        },
        comparison: { equal: true },
      }),
    );
    expect(l).toEqual([
      { level: 'warning', text: 'processor/k8sattributes (kubernetes mock): the processor watches Node objects, but none are mocked.' },
      { level: 'info', text: 'processor/k8sattributes (kubernetes mock): objects do not change during the run.' },
    ]);
  });

  it('warns about wall clock reads unless the inputs were rebased', () => {
    const wall_clock = [{ component: 'processor/filter', expression: 'time < Now() - Duration("1h")' }];
    const recorded = limitations(
      run({ result: { mode: 'instrumented', status: 'completed' }, comparison: { equal: true }, timestamps: { mode: 'recorded', offset: '48h0m0s', wall_clock } }),
    );
    expect(recorded).toHaveLength(1);
    expect(recorded[0].level).toBe('warning');
    expect(recorded[0].text).toContain('was 48h0m0s old');
    const rebased = limitations(
      run({ result: { mode: 'instrumented', status: 'completed' }, comparison: { equal: true }, timestamps: { mode: 'rebased', offset: '48h0m0s', wall_clock } }),
    );
    expect(rebased).toEqual([{ level: 'info', text: "Input timestamps were shifted by 48h0m0s so the latest record falls at the run's start." }]);
  });

  it('reports engine errors alone', () => {
    expect(limitations({ id: 'r', project_id: 'default', state: 'error', error: 'bad config', created: '' })).toEqual([
      { level: 'error', text: 'The engine could not run the request: bad config' },
    ]);
  });
});
