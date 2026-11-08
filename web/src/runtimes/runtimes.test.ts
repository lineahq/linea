import { describe, expect, it } from 'vitest';
import { ApiError } from '../api/client';
import type { CatalogComponent, CollectorCatalog, Runtime } from '../api/types';
import {
  BUILT_IN, BUILT_IN_NAME, catalogGroups, componentKey, defaultName, effectiveRuntime, makeDefaultQuestion, finished, formatSize, IMAGE_STATES, olderImage, problemOf, pruneQuestion, prunedText, pruneSummary, referenceProblem,
  mergeLog, nameProblem, nextLogFrom, reportRuntime, runtimeField, runtimeOptions, runtimeProviders,
} from './runtimes';

const runtime = (name: string, version: string, verified: boolean): Runtime => ({
  name, collector_version: version, verified, go_image: 'golang', image_id: `sha256:${name}`, manifest: '',
  module_set: '', product_version: 'dev', protocol: 1, size: 70_000_000,
});

describe('runtimeOptions', () => {
  const all = [runtime('v0163', '0.163.0', false), runtime('mine', '0.162.0', true)];

  it('lists the default first, naming the built-in runtime it is until the team chooses another', () => {
    const opts = runtimeOptions(all, BUILT_IN, '0.162.0', BUILT_IN_NAME);
    expect(opts.map((o) => [o.value, o.label])).toEqual([
      [BUILT_IN, 'Default · built-in · Collector 0.162.0'],
      ['mine', 'mine · Collector 0.162.0'],
      ['v0163', 'v0163 · Collector 0.163.0 · unverified'],
    ]);
    expect(opts.map((o) => o.unverified)).toEqual([false, false, true]);
  });

  it('offers the built-in runtime a team server lists once', () => {
    const opts = runtimeOptions([runtime(BUILT_IN_NAME, '0.162.0', true), ...all], BUILT_IN, '0.162.0', 'mine');
    expect(opts.map((o) => o.value)).toEqual([BUILT_IN, BUILT_IN_NAME, 'mine', 'v0163']);
  });

  it('names the default runtime and keeps built-in choosable beside it', () => {
    const opts = runtimeOptions(all, BUILT_IN, '0.162.0', 'mine');
    expect(opts.map((o) => [o.value, o.label])).toEqual([
      [BUILT_IN, 'Default · mine · Collector 0.162.0'],
      [BUILT_IN_NAME, 'Built-in · Collector 0.162.0'],
      ['mine', 'mine · Collector 0.162.0'],
      ['v0163', 'v0163 · Collector 0.163.0 · unverified'],
    ]);
  });

  it('says unverified for an unverified default', () => {
    const opts = runtimeOptions(all, BUILT_IN, '0.162.0', 'v0163');
    expect(opts[0]).toEqual({ value: BUILT_IN, label: 'Default · v0163 · Collector 0.163.0 · unverified', unverified: true, missing: false });
  });

  it('says only Default while the default is unknown, and only its name when the list lacks it', () => {
    expect(runtimeOptions(all, BUILT_IN, '0.162.0')[0].label).toBe('Default');
    expect(runtimeOptions(all, BUILT_IN, '0.162.0')[1].value).toBe(BUILT_IN_NAME);
    expect(runtimeOptions(undefined, BUILT_IN, '0.162.0', 'mine')[0].label).toBe('Default · mine');
    expect(runtimeOptions(all, BUILT_IN, undefined, BUILT_IN_NAME)[0].label).toBe('Default · built-in');
  });

  it('never marks built-in missing, even when the default is built-in', () => {
    const opts = runtimeOptions([], BUILT_IN_NAME, '0.162.0', BUILT_IN_NAME);
    expect(opts.map((o) => [o.value, o.missing])).toEqual([[BUILT_IN, false], [BUILT_IN_NAME, false]]);
  });

  it('keeps a selected runtime that is gone, marked missing', () => {
    const opts = runtimeOptions([], 'gone');
    expect(opts.at(-1)).toEqual({ value: 'gone', label: 'gone · not found', unverified: false, missing: true });
    expect(runtimeOptions(undefined, BUILT_IN)).toHaveLength(2);
    expect(runtimeOptions(undefined, BUILT_IN, undefined, BUILT_IN_NAME)).toHaveLength(1);
  });
});

describe('effectiveRuntime', () => {
  it('resolves the empty selection to the default, and built-in to empty', () => {
    expect(effectiveRuntime(BUILT_IN, 'mine')).toBe('mine');
    expect(effectiveRuntime(BUILT_IN, BUILT_IN_NAME)).toBe(BUILT_IN);
    expect(effectiveRuntime(BUILT_IN_NAME, 'mine')).toBe(BUILT_IN);
    expect(effectiveRuntime('v0163', 'mine')).toBe('v0163');
    expect(effectiveRuntime(BUILT_IN, undefined)).toBeUndefined();
    expect(effectiveRuntime('v0163', undefined)).toBe('v0163');
  });
});

describe('makeDefaultQuestion', () => {
  it('names the runtime and who it affects', () => {
    expect(makeDefaultQuestion('mine')).toBe('Make mine the default runtime? Every run, validation and preview that names no runtime runs on it, for the whole team.');
  });
});

describe('runtimeProviders', () => {
  const lc = { name: 'livecapture', package: 'github.com/lineahq/linea/collector/livecapture/livecapturedebug' };
  const withProvider = { ...runtime('lab', '0.162.0', true), providers: [lc] };
  it('names the selected runtime providers, none for the built-in one', () => {
    expect(runtimeProviders([withProvider], 'lab')).toEqual([lc]);
    expect(runtimeProviders([runtime('plain', '0.162.0', true)], 'plain')).toEqual([]);
    expect(runtimeProviders([withProvider], BUILT_IN_NAME)).toEqual([]);
    expect(runtimeProviders([withProvider], BUILT_IN, BUILT_IN_NAME)).toEqual([]);
  });
  it('names the default runtime providers for the empty selection', () => {
    expect(runtimeProviders([withProvider], BUILT_IN, 'lab')).toEqual([lc]);
    expect(runtimeProviders([withProvider], BUILT_IN_NAME, 'lab')).toEqual([]);
  });
  it('does not know them before the list or the default loads, or for a runtime that is gone', () => {
    expect(runtimeProviders(undefined, 'lab')).toBeUndefined();
    expect(runtimeProviders([withProvider], 'gone')).toBeUndefined();
    expect(runtimeProviders([withProvider], BUILT_IN)).toBeUndefined();
    expect(runtimeProviders([withProvider], BUILT_IN, 'gone')).toBeUndefined();
  });
});

describe('runtimeField', () => {
  it('sends no runtime for the built-in one', () => {
    expect(runtimeField('')).toEqual({});
    expect(runtimeField(undefined)).toEqual({});
    expect(runtimeField('mine')).toEqual({ runtime: 'mine' });
  });
});

describe('reportRuntime', () => {
  it('names built-in and container runtimes with their custom modules', () => {
    expect(reportRuntime(undefined)).toBeUndefined();
    expect(reportRuntime({ kind: 'built_in', collector_version: '0.162.0', verified: true }))
      .toEqual({ label: 'Built-in · Collector 0.162.0', unverified: false, custom: [], providers: [], missing: [] });
    const facts = reportRuntime({
      kind: 'container', name: 'v0163', collector_version: '0.163.0', verified: false,
      custom_modules: [
        { module: 'example.com/stamp', source: 'git https://example.com/s.git', commit: '0123456789abcdef0123' },
        { module: 'example.com/x', source: 'dir' },
      ],
      providers: [{ name: 'stamp', package: 'example.com/stamp/stampdebug', components: ['processor/stamp'] }],
    });
    expect(facts).toEqual({
      label: 'v0163 · Collector 0.163.0', unverified: true,
      custom: ['example.com/stamp from git https://example.com/s.git @ 0123456789ab', 'example.com/x from dir'],
      providers: ['stamp from example.com/stamp/stampdebug (processor/stamp)'],
      missing: [],
    });
    expect(reportRuntime({ kind: 'container', name: 'lean', collector_version: '0.160.0', verified: true,
      missing_features: ['ottl_replay', 'future_feature'] })!.missing)
      .toEqual(['transform statement replay (no processor/transform)', 'future_feature']);
  });
});

describe('words', () => {
  it('lists manifest diagnostics of a refusal', () => {
    const e = new ApiError(422, 'manifest refused', {
      code: 'validation_failed', error: 'manifest refused',
      diagnostics: [
        { severity: 'error', code: 'manifest_modules', message: 'module x is off the release' },
        { severity: 'warning', code: 'w', message: 'ignored' },
      ],
    });
    expect(problemOf(e)).toEqual({ message: 'manifest refused', details: ['module x is off the release', 'warning: ignored'] });
    expect(problemOf(new ApiError(503, 'no Docker')).message).toBe('Runtimes are unavailable on this server: no Docker');
    expect(problemOf(new Error('boom'))).toEqual({ message: 'boom', details: [] });
  });
});

describe('runtime storage', () => {
  it('asks what a prune removes, naming each kind and the size', () => {
    expect(pruneQuestion({ images: 2, bytes: 69e6 + 60e6 + 5.5e9, containers: 1, caches: 2, empty: false }))
      .toBe('Remove 2 runtime images, 2 build caches and 1 stopped container (5.6 GB)? Runs keep their evidence.');
    expect(pruneQuestion({ images: 1, bytes: 69e6, containers: 0, caches: 0, empty: false })).toBe('Remove 1 runtime image (69.0 MB)? Runs keep their evidence.');
  });

  it('words sizes, prune results and image states', () => {
    expect([formatSize(-1), formatSize(512), formatSize(69_400_000), formatSize(5_500_000_000)]).toEqual(['unknown', '512 B', '69.4 MB', '5.5 GB']);
    expect(pruneSummary({ removed: [], kept: [], reclaimed: 0 })).toBe('Nothing to remove.');
    expect(pruneSummary({ removed: [{ kind: 'runtime', name: 'mine', image_id: 'sha256:0123456789abcdef', size: 69e6 }], kept: [{ kind: 'cache', name: 'linea-gomod', size: 0, reason: 'volume is in use' }], reclaimed: 69e6 }))
      .toBe('Removed 1 object, reclaiming 69.0 MB; kept 1.');
    expect(prunedText({ kind: 'cache', name: 'linea-gomod', size: 0, reason: 'volume is in use' })).toBe('cache linea-gomod: volume is in use');
    expect(prunedText({ kind: 'runtime', name: 'mine', image_id: 'sha256:0123456789abcdef', size: 0 })).toBe('runtime mine (image 0123456789ab)');
    expect(Object.keys(IMAGE_STATES)).toEqual(['named', 'replaced', 'tagged', 'stale']);
  });

  it('checks references before a push or import', () => {
    expect(referenceProblem('')).toMatch(/import/);
    expect(referenceProblem('', true)).toMatch(/push to/);
    expect(referenceProblem('a b')).toMatch(/spaces/);
    expect(referenceProblem('linea-runtime:mine', true)).toMatch(/registry/);
    expect(referenceProblem('registry.example.com/team/rt:1', true)).toBeUndefined();
  });

  it('treats an image a rebuild took the name from as older, and offers its name once', () => {
    const current = { ...runtime('mine', '0.162.0', true), tags: ['linea-runtime:mine'] };
    const pushed = { ...runtime('mine', '0.161.0', true), image_id: 'sha256:pushed', tags: ['registry.example.com/team/rt:1'] };
    const imported = { ...runtime('ci', '0.162.0', true), tags: ['registry.example.com/team/rt:2'] };
    const all = [current, pushed, imported];
    expect(all.map((r) => olderImage(r, all))).toEqual([false, true, false]);
    expect(runtimeOptions(all, BUILT_IN, undefined, BUILT_IN_NAME).map((o) => o.value)).toEqual([BUILT_IN, 'ci', 'mine']);
  });
});

describe('remote worker reports', () => {
  it('words the runtime of a report from a connected worker', () => {
    expect(reportRuntime({ kind: 'remote', name: 'far', collector_version: '0.162.0', verified: true })?.label)
      .toBe('far · Collector 0.162.0 · remote worker');
    // A report of an earlier debugger that dialed the worker names where.
    expect(reportRuntime({ kind: 'remote', name: 'far', endpoint: 'http://127.0.0.1:8080', collector_version: '0.162.0', verified: true })?.label)
      .toBe('far · Collector 0.162.0 · remote worker at http://127.0.0.1:8080');
  });
});

describe('runtime names', () => {
  it.each([
    ['', 'Name the runtime.'],
    ['built-in', '"built-in" names the built-in runtime.'],
    ['Upper', "Use lower-case letters, digits, '.', '_' and '-' (not first), at most 63 characters."],
    ['-lead', "Use lower-case letters, digits, '.', '_' and '-' (not first), at most 63 characters."],
    ['has space', "Use lower-case letters, digits, '.', '_' and '-' (not first), at most 63 characters."],
    ['a'.repeat(64), "Use lower-case letters, digits, '.', '_' and '-' (not first), at most 63 characters."],
    ['v0162', undefined],
    ['team.rt_1-x', undefined],
    ['a'.repeat(63), undefined],
  ])('checks %j', (name, want) => expect(nameProblem(name)).toBe(want));

  it('suggests a name from the release', () => {
    expect(defaultName('0.162.0')).toBe('v0162');
    expect(defaultName('v0.99.1')).toBe('v099');
    expect(defaultName('1.2.3')).toBe('');
    expect(defaultName(undefined)).toBe('');
  });
});

describe('build log', () => {
  it('merges pages that follow on, overlap or skip ahead', () => {
    const base = { lines: ['a', 'b'], skipped: 0 };
    expect(mergeLog(base, { log: ['c'], log_from: 2 })).toEqual({ lines: ['a', 'b', 'c'], skipped: 0 });
    expect(mergeLog(base, { log: ['b', 'c'], log_from: 1 })).toEqual({ lines: ['a', 'b', 'c'], skipped: 0 });
    expect(mergeLog(base, { log: ['z'], log_from: 40 })).toEqual({ lines: ['z'], skipped: 40 });
    expect(nextLogFrom({ lines: ['z'], skipped: 40 })).toBe(41);
  });

  it('knows which states are over', () => {
    expect(['queued', 'running'].some(finished)).toBe(false);
    expect(['done', 'failed', 'cancelled'].every(finished)).toBe(true);
  });
});

describe('catalogGroups', () => {
  const c = (kind: string, type: string, extra: Partial<CatalogComponent> = {}): CatalogComponent => ({ class: 'x', gomod: `example.com/${kind}/${type} v1`, kind, type, ...extra });
  const catalog = {
    collector_version: '0.162.0', source: 't',
    components: [c('exporters', 'otlp'), c('processors', 'batch'), c('receivers', 'otlp'), c('extensions', 'zpages'), c('connectors', 'count'), c('receivers', 'filelog', { deprecated_type: 'oldlog' }), c('widgets', 'w')],
  } as CollectorCatalog;

  it('orders kinds as a pipeline reads and types alphabetically, unknown kinds last', () => {
    expect(catalogGroups(catalog, '').map((g) => [g.kind, g.components.map((x) => x.type)])).toEqual([
      ['receivers', ['filelog', 'otlp']],
      ['processors', ['batch']],
      ['exporters', ['otlp']],
      ['connectors', ['count']],
      ['extensions', ['zpages']],
      ['widgets', ['w']],
    ]);
  });

  it('filters by type, deprecated type or module, ignoring case', () => {
    expect(catalogGroups(catalog, ' BATCH ').map((g) => g.kind)).toEqual(['processors']);
    expect(catalogGroups(catalog, 'oldlog').flatMap((g) => g.components.map((x) => x.type))).toEqual(['filelog']);
    expect(catalogGroups(catalog, 'example.com/exporters').flatMap((g) => g.components.map((x) => x.type))).toEqual(['otlp']);
    expect(catalogGroups(catalog, 'nothing')).toEqual([]);
    expect(catalogGroups(undefined, '')).toEqual([]);
  });

  it('names a component as the manifest request does', () => {
    expect(componentKey({ kind: 'receivers', type: 'otlp' })).toBe('receivers/otlp');
  });
});
