import { describe, expect, it } from 'vitest';
import type { ArtifactMetadata, CaptureSessionPage, Run, RuntimeBuild, Suite } from '../api/types';
import type { WorkspaceState } from '../workspace/store';
import {
  editedText,
  activeBuilds,
  activeCaptures,
  attention,
  captureProgress,
  classifyText,
  draftCounts,
  draftSummary,
  hasDraft,
  isYamlName,
  pasteShortcut,
  recentRuns,
  recentSuites,
  runAssertions,
  runExecution,
  savedRows,
  savedStatus,
  startMode,
  suiteOutcome,
  whenDay,
  whenText,
} from './start';

const NOW = new Date(2026, 9, 23, 15, 0, 0); // local time, so the formats do not depend on the zone
const at = (y: number, mo: number, d: number, h = 12, mi = 0) => new Date(y, mo - 1, d, h, mi).toISOString();

const blank: WorkspaceState = { configYAML: '', configPath: '', scenarioYAML: '', scenarioPath: '', compare: false };

function run(over: Partial<Run> & { id: string }): Run {
  return { created: at(2026, 10, 23, 14, 9), state: 'done', ...over } as Run;
}
const completed = (assertions?: { outcome: string; results?: { outcome: string }[] }, scenario?: string): Run['report'] =>
  ({ report_schema: '1', scenario, result: { status: 'completed', mode: 'scenario' }, assertions }) as Run['report'];
/** listing is a run list's summary of a finished run. */
const listing = (over: Partial<NonNullable<Run['summary']>> = {}): NonNullable<Run['summary']> => ({ status: 'completed', inputs: 1, injected: 1, ...over });

describe('hasDraft and draftSummary', () => {
  it.each([
    [{}, false],
    [{ configYAML: '  \n' }, false],
    [{ configYAML: 'receivers: {}' }, true],
    [{ scenarioYAML: 'name: x' }, true],
  ])('hasDraft(%j) is %s', (patch, want) => {
    expect(hasDraft({ ...blank, ...patch })).toBe(want);
  });

  it('names the draft after its file and counts the scenario', () => {
    const scenario = [
      'name: debug-error-logs',
      'inputs:',
      '  - receiver: otlp',
      '    data: "{}"',
      'mocks:',
      '  files:',
      '    - path: /var/log/a.log',
      '      content: x',
      'assertions:',
      '  - name: a',
      '    exporter: debug',
      '    count: 1',
      '  - name: b',
      '    exporter: debug',
      '    count: 2',
    ].join('\n');
    const d = draftSummary({ ...blank, configYAML: 'receivers: {}', configPath: 'C:\\work\\checkout-logs.yaml', scenarioYAML: scenario });
    expect(d).toMatchObject({ name: 'checkout-logs.yaml', scenario: 'debug-error-logs', inputs: 1, mocks: 1, assertions: 2, dirty: true });
    expect(draftCounts(d)).toBe('1 input · 1 mock · 2 assertions');
  });

  it('copes with a hand-written configuration and an unreadable scenario', () => {
    const d = draftSummary({ ...blank, configYAML: 'receivers: {}', scenarioYAML: ': : not yaml\n\t- [' });
    expect(d.name).toBe('Untitled configuration');
    expect(d.scenario).toBeUndefined();
    expect(draftCounts(d)).toBe('0 inputs · 0 mocks · 0 assertions');
  });

  it('is clean when the draft matches its baseline', () => {
    const d = draftSummary({ ...blank, configYAML: 'a: 1', baselines: { config: 'a: 1' } });
    expect(d.dirty).toBe(false);
  });
});

describe('startMode', () => {
  it.each([
    ['a draft makes a returning user at once', { draft: true }, 'returning'],
    ['any run', { draft: false, runs: 1, saved: 0, suites: 0, active: 0 }, 'returning'],
    ['any saved source', { draft: false, runs: 0, saved: 2, suites: 0, active: 0 }, 'returning'],
    ['running work', { draft: false, runs: 0, saved: 0, suites: 0, active: 1 }, 'returning'],
    ['evidence beats lists that are still loading', { draft: false, runs: 3 }, 'returning'],
    ['nothing is known yet', { draft: false }, 'checking'],
    ['one list is still loading', { draft: false, runs: 0, saved: 0, suites: undefined, active: 0 }, 'checking'],
    ['every list answered empty', { draft: false, runs: 0, saved: 0, suites: 0, active: 0 }, 'first'],
  ] as const)('%s', (_, evidence, want) => {
    expect(startMode(evidence)).toBe(want);
  });
});

describe('whenText and whenDay', () => {
  it.each([
    [at(2026, 10, 23, 14, 9), 'Today 14:09'],
    [at(2026, 10, 23, 8, 5), 'Today 08:05'],
    [at(2026, 10, 22, 23, 59), 'Yesterday'],
    [at(2026, 10, 14), 'Oct 14'],
    [at(2025, 12, 31), 'Dec 31, 2025'],
    ['', ''],
    ['not a date', ''],
  ])('%s is %s', (iso, want) => {
    expect(whenText(iso, NOW)).toBe(want);
  });

  it('drops the clock time for saved sources', () => {
    expect(whenDay(at(2026, 10, 23, 14, 9), NOW)).toBe('Today');
    expect(whenDay(at(2026, 10, 14), NOW)).toBe('Oct 14');
  });
});

describe('run wording', () => {
  it.each([
    [{ state: 'queued' }, 'Queued', 'neutral'],
    [{ state: 'running' }, 'Running', 'running'],
    [{ state: 'cancelled' }, 'Cancelled', 'warn'],
    [{ state: 'error' }, 'Failed', 'fail'],
    [{ state: 'error', interrupted: true }, 'Interrupted', 'fail'],
    [{ state: 'done', report: completed() }, 'Completed', 'pass'],
    [{ state: 'done', report: { result: { status: 'partial' } } }, 'Partial', 'warn'],
    [{ state: 'done', report: { result: { status: 'failed' } } }, 'Failed', 'fail'],
    // Run lists carry a summary, never the report.
    [{ state: 'done', summary: listing({ status: 'completed' }) }, 'Completed', 'pass'],
    [{ state: 'done', summary: listing({ status: 'partial' }) }, 'Partial', 'warn'],
    [{ state: 'done' }, 'Finished', 'neutral'],
  ] as const)('%j executes as %s', (r, text, tone) => {
    expect(runExecution(r as Run)).toMatchObject({ text, tone });
  });

  it.each([
    [{ state: 'running' }, 'Not evaluated', 'neutral'],
    [{ state: 'done', report: completed() }, 'No assertions', 'neutral'],
    [{ state: 'done', report: completed({ outcome: 'pass', results: [{ outcome: 'pass' }, { outcome: 'pass' }] }) }, '2 of 2 passed', 'pass'],
    [{ state: 'done', report: completed({ outcome: 'fail', results: [{ outcome: 'pass' }, { outcome: 'fail' }] }) }, '1 of 2 failed', 'fail'],
    [{ state: 'done', report: completed({ outcome: 'inconclusive', results: [{ outcome: 'inconclusive' }, { outcome: 'pass' }] }) }, '1 of 2 inconclusive', 'warn'],
    [{ state: 'done', report: completed({ outcome: 'error', results: [] }) }, 'Not evaluated', 'warn'],
    [{ state: 'done', summary: listing({ status: 'completed' }) }, 'No assertions', 'neutral'],
    [{ state: 'done', summary: listing({ assertions: { outcome: 'pass', pass: 2, fail: 0, inconclusive: 0, error: 0 } }) }, '2 of 2 passed', 'pass'],
    [{ state: 'done', summary: listing({ assertions: { outcome: 'fail', pass: 1, fail: 1, inconclusive: 0, error: 0 } }) }, '1 of 2 failed', 'fail'],
    [{ state: 'done', summary: listing({ assertions: { outcome: 'inconclusive', pass: 2, fail: 0, inconclusive: 1, error: 0 } }) }, '1 of 3 inconclusive', 'warn'],
  ] as const)('%j asserts as %s', (r, text, tone) => {
    expect(runAssertions(r as Run)).toMatchObject({ text, tone });
  });

  it('lists the newest runs first, at most five, with their scenario', () => {
    const runs = Array.from({ length: 7 }, (_, i) =>
      run({ id: `run-000000${i}aaaa`, created: at(2026, 10, 23, 10 + i), report: completed(undefined, i === 6 ? 'debug-error-logs' : undefined) }),
    );
    const rows = recentRuns(runs, NOW);
    expect(rows.map((r) => r.id)).toEqual([6, 5, 4, 3, 2].map((i) => `run-000000${i}aaaa`));
    expect(rows[0]).toMatchObject({ shortId: 'run-0000', scenario: 'debug-error-logs', when: 'Today 16:00' });
    expect(rows[1].scenario).toBe('');
    expect(recentRuns(undefined, NOW)).toEqual([]);
  });

  it('reads the scenario of a listed run from its summary', () => {
    const [row] = recentRuns([run({ id: 'run-listed', summary: listing({ scenario: 'broken-routing' }) })], NOW);
    expect(row.scenario).toBe('broken-routing');
  });
});

describe('suites', () => {
  const member = (run_id: string, error?: string) => ({ label: run_id, run_id, error });
  const pass = run({ id: 'p', report: completed({ outcome: 'pass' }) });
  const fail = run({ id: 'f', report: completed({ outcome: 'fail' }) });
  const live = run({ id: 'l', state: 'running' });
  const listedPass = run({ id: 'lp', summary: listing({ assertions: { outcome: 'pass', pass: 1, fail: 0, inconclusive: 0, error: 0 } }) });
  const listedFail = run({ id: 'lf', summary: listing({ assertions: { outcome: 'fail', pass: 0, fail: 1, inconclusive: 0, error: 0 } }) });
  const runs = new Map([pass, fail, live, listedPass, listedFail].map((r) => [r.id, r]));
  const suite = (members: ReturnType<typeof member>[]): Suite => ({ id: 's', project_id: 'default', config: 'C:\\a\\otel.yaml', created: at(2026, 10, 23), members });

  it.each([
    ['no members', [], 'No scenarios', 'neutral'],
    ['all passed', [member('p'), member('p')], '2 of 2 passed', 'pass'],
    ['one failed', [member('p'), member('f')], '1 of 2 failed', 'fail'],
    ['a member that did not start', [member('p'), member('', 'refused')], '1 of 2 failed', 'fail'],
    ['one still running', [member('p'), member('l')], '1 of 2 passed', 'neutral'],
    ['runs the list no longer has', [member('gone')], 'Result not shown', 'neutral'],
    ['listed runs that passed', [member('lp'), member('lp')], '2 of 2 passed', 'pass'],
    ['a listed run that failed', [member('lp'), member('lf')], '1 of 2 failed', 'fail'],
  ] as const)('%s', (_, members, text, tone) => {
    expect(suiteOutcome(suite([...members]), runs)).toMatchObject({ text, tone });
  });

  it('lists the newest three with the configuration file name', () => {
    const all = Array.from({ length: 4 }, (_, i) => ({ ...suite([member('p')]), id: `s${i}`, created: at(2026, 10, 20 + i) }));
    const rows = recentSuites(all, [pass], NOW);
    expect(rows.map((r) => r.id)).toEqual(['s3', 's2', 's1']);
    expect(rows[0]).toMatchObject({ config: 'otel.yaml', members: 1, when: 'Today 12:00' });
    expect(recentSuites(undefined, undefined, NOW)).toEqual([]);
  });
});

describe('saved work', () => {
  const meta = (id: string, over: Partial<ArtifactMetadata>): ArtifactMetadata => ({
    id, project_id: 'default', workspace_id: 'w', kind: 'configuration', label: id, generation: 3, head_revision_id: `${id}-rev`,
    created: at(2026, 10, 1), updated: at(2026, 10, 2), ...over,
  });

  it('merges the kinds newest first and marks fixtures as not openable as drafts', () => {
    const rows = savedRows(
      {
        configuration: [meta('c1', { label: 'checkout-logs.yaml', updated: at(2026, 10, 22) })],
        scenario: [meta('s1', { label: 'debug-error-logs', updated: at(2026, 10, 23, 9) })],
        fixture: [meta('f1', { label: '', updated: at(2026, 10, 14), generation: 1 })],
      },
      NOW,
    );
    expect(rows.map((r) => [r.id, r.detail, r.when, r.draft])).toEqual([
      ['s1', 'Scenario · rev 3', 'Today', true],
      ['c1', 'Configuration · rev 3', 'Yesterday', true],
      ['f1', 'Fixture · rev 1', 'Oct 14', false],
    ]);
    expect(rows[2].label).toBe('f1');
  });

  it('keeps at most five rows', () => {
    const many = Array.from({ length: 8 }, (_, i) => meta(`c${i}`, { updated: at(2026, 10, 10 + i) }));
    expect(savedRows({ configuration: many }, NOW)).toHaveLength(5);
  });

  it.each([
    ['all loading', [{ status: 'pending' }, { status: 'pending' }], 'loading'],
    ['one loaded, one failed', [{ status: 'success' }, { status: 'error', httpStatus: 500, message: 'x' }], 'ok'],
    ['one loaded, one pending', [{ status: 'success' }, { status: 'pending' }], 'ok'],
    ['saved work unavailable', [{ status: 'error', httpStatus: 503 }, { status: 'error', httpStatus: 503 }], 'none'],
    ['failing', [{ status: 'error', httpStatus: 503 }, { status: 'error', httpStatus: 500, message: 'boom' }], 'error'],
  ] as const)('savedStatus: %s is %s', (_, lists, state) => {
    expect(savedStatus([...lists]).state).toBe(state);
  });
});

describe('active work', () => {
  const session = (id: string, over: Record<string, unknown>) => ({
    id, state: 'active', terminal: false, target_count: 3, active_count: 3, failed_count: 0, uncertain_count: 0, 
    created_at: at(2026, 10, 23, 14, 0), expires_at: at(2026, 10, 23, 16, 0), data: { received_records: 1204 },
    ...over,
  });
  const page = (items: unknown[]) => ({ items }) as CaptureSessionPage;

  it('lists only unfinished captures, worded for the card', () => {
    const rows = activeCaptures(
      page([
        session('cap-aaaa0412', { metadata: { name: 'payments-gw group' } }),
        session('cap-done', { terminal: true, state: 'frozen' }),
        session('cap-bbbb7777', { state: 'stopping', failed_count: 1, uncertain_count: 1, target_count: 1, data: { received_records: 1 } }),
      ]),
      NOW,
    );
    expect(rows.map((r) => r.id)).toEqual(['cap-aaaa0412', 'cap-bbbb7777']);
    expect(rows[0]).toMatchObject({ title: 'payments-gw group', detail: '3 Collectors · 1204 records', progress: 50 });
    expect(rows[0].badge).toMatchObject({ text: 'Receiving', tone: 'running' });
    expect(rows[1]).toMatchObject({ title: 'Capture bbbb7777', detail: '1 Collector · 1 record · 2 Collectors not confirmed' });
    expect(rows[1].badge.text).toBe('Stopping');
    expect(activeCaptures(undefined, NOW)).toEqual([]);
  });

  it.each([
    [at(2026, 10, 23, 14), at(2026, 10, 23, 16), 25],
    [at(2026, 10, 23, 16), at(2026, 10, 23, 17), 0],
    [at(2026, 10, 23, 10), at(2026, 10, 23, 12), 100],
    [at(2026, 10, 23, 14), at(2026, 10, 23, 14), 0],
  ])('captureProgress(%s, %s) is %i', (created, expires, want) => {
    expect(captureProgress(created, expires, new Date(2026, 9, 23, 14, 30))).toBe(want);
  });

  const build = (id: string, over: Partial<RuntimeBuild>): RuntimeBuild =>
    ({ id, name: 'acme-otelcol', collector_version: '0.137.0', state: 'running', created: at(2026, 10, 23, 14), log: [], log_from: 0, log_lines: 0, verified: true, ...over }) as RuntimeBuild;

  it('lists queued and running builds and where each is', () => {
    const rows = activeBuilds([
      build('b1', {}),
      build('b2', { state: 'queued', created: at(2026, 10, 23, 14, 5) }),
      build('b3', { state: 'done' }),
    ]);
    expect(rows.map((r) => r.id)).toEqual(['b2', 'b1']);
    expect(rows[1]).toMatchObject({ title: 'acme-otelcol 0.137.0', detail: 'Compiling on a builder', badge: { text: 'Building' } });
    expect(rows[0]).toMatchObject({ detail: 'Waiting for a builder', badge: { text: 'Queued' } });
  });
});

describe('attention', () => {
  const build = (id: string, over: Partial<RuntimeBuild>): RuntimeBuild =>
    ({ id, name: 'acme', collector_version: '0.137.0', state: 'failed', created: at(2026, 10, 23, 14), log: [], log_from: 0, log_lines: 0, verified: true, ...over }) as RuntimeBuild;

  it('says nothing when nothing is wrong', () => {
    expect(attention({})).toEqual([]);
    expect(attention({ builds: [build('b', { state: 'done' })] })).toEqual([]);
  });

  it('reports a build that failed and was not rebuilt', () => {
    const items = attention({
      builds: [
        build('old-fail', { created: at(2026, 10, 22), error: 'go build: missing module' }),
        build('other-fail', { name: 'other', error: 'out of disk' }),
        build('recovered-fail', { name: 'fixed', created: at(2026, 10, 21) }),
        build('recovered-ok', { name: 'fixed', state: 'done', created: at(2026, 10, 22) }),
      ],
    });
    expect(items.map((i) => i.id)).toEqual(['build-other-fail', 'build-old-fail']);
    expect(items[0]).toMatchObject({ title: 'Build other 0.137.0 failed', body: 'out of disk', to: { kind: 'build', id: 'other-fail' } });
  });

  it('uses only the newest build of a name', () => {
    const items = attention({ builds: [build('new-ok', { state: 'done' }), build('old-fail', { created: at(2026, 10, 1) })] });
    expect(items).toEqual([]);
  });
});

describe('dropped and pasted text', () => {
  it.each([
    ['receivers:\n  otlp: {}\nservice:\n  pipelines: {}\n', 'configuration'],
    ['service:\n  pipelines: {}\n', 'configuration'],
    ['name: s\ninputs:\n  - receiver: otlp\n', 'scenario'],
    ['assertions: []\n', 'scenario'],
    ['hello: world\n', undefined],
    ['- a\n- b\n', undefined],
    ['just text', undefined],
    ['a: [unclosed', undefined],
    ['', undefined],
  ])('classifies %j as %s', (text, want) => {
    expect(classifyText(text)).toBe(want);
  });

  it('takes YAML file names', () => {
    expect(['a.yaml', 'B.YML', 'x.json', 'yaml'].map(isYamlName)).toEqual([true, true, false, false]);
  });

  it('names the paste key for the platform', () => {
    expect(pasteShortcut('MacIntel')).toBe('⌘V');
    expect(pasteShortcut('Win32')).toBe('Ctrl+V');
    expect(pasteShortcut('')).toBe('Ctrl+V');
  });
});

describe('editedText', () => {
  const now = new Date(2026, 9, 23, 12, 0, 0);
  const ago = (min: number) => new Date(now.getTime() - min * 60_000).toISOString();
  it.each([
    [undefined, ''],
    ['not a time', ''],
    [ago(0.5), 'edited just now'],
    [ago(6), 'edited 6 min ago'],
    [ago(185), 'edited 3 h ago'],
    [ago(26 * 60), 'edited yesterday'],
    [ago(5 * 24 * 60), 'edited Oct 18'],
  ])('%s → %s', (iso, want) => {
    expect(editedText(iso, now)).toBe(want);
  });
});
