import { describe, expect, it } from 'vitest';
import type { Runtime } from '../../api/types';
import { buildChip, canBeDefault, catalogChip, defaultFact, runtimeDetail, runtimeRows, selectedRow } from './runtimeList';

const runtime = (name: string, version: string, patch: Partial<Runtime> = {}): Runtime => ({
  name, collector_version: version, verified: true, go_image: 'golang', image_id: `sha256:${name}${version}abcdef0123456789`, manifest: '',
  module_set: '', product_version: 'dev', protocol: 1, size: 70_000_000, ...patch,
});
describe('runtimeRows', () => {
  it('lists the built-in runtime, then runtimes by name', () => {
    const rows = runtimeRows(
      [
        runtime('staging', '0.135.1', { verified: false }),
        runtime('acme', '0.136.0'),
      ],
      '0.136.0',
    );
    expect(rows.map((r) => [r.key, r.type, r.version, r.state.label])).toEqual([
      ['built-in', 'Built-in', '0.136.0', 'Verified'],
      ['runtime:acme', 'Local container', '0.136.0', 'Verified'],
      ['runtime:staging', 'Local container', '0.135.1', 'Unverified'],
    ]);
  });

  it('has the built-in runtime alone when no runtime is listed or the list is unavailable', () => {
    expect(runtimeRows(undefined, undefined).map((r) => r.key)).toEqual(['built-in']);
  });

  it('does not list an older image that only a pushed reference keeps', () => {
    const current = runtime('acme', '0.137.0', { tags: ['linea-runtime:acme'] });
    const older = runtime('acme', '0.136.0', { tags: ['registry.example/acme:1'] });
    expect(runtimeRows([older, current], '0.136.0').map((r) => r.version)).toEqual(['0.136.0', '0.137.0']);
  });
});

describe('the default runtime', () => {
  const catalog = (name: string, patch: Partial<Runtime> = {}) => runtime(name, '0.162.0', {
    catalog: { identity: 'sha256:abc', image: 'img', registered_at: '2026-01-01T00:00:00Z', registered_by: 'ana', workers: 1, slots: 2 }, ...patch,
  } as Partial<Runtime>);

  it('marks the built-in row when the default is built-in, and no row while it is unknown', () => {
    const list = [catalog('lab'), runtime('acme', '0.162.0')];
    expect(runtimeRows(list, '0.162.0', 'built-in').map((r) => r.isDefault)).toEqual([true, false, false]);
    expect(runtimeRows(list, '0.162.0').map((r) => r.isDefault)).toEqual([false, false, false]);
  });

  it('marks the catalog runtime by name or by the list, never a local container', () => {
    const list = [catalog('lab'), catalog('ci', { default: true }), runtime('acme', '0.162.0')];
    expect(runtimeRows(list, '0.162.0', 'lab').map((r) => [r.name, r.isDefault])).toEqual([
      ['built-in', false], ['acme', false], ['ci', true], ['lab', true],
    ]);
    expect(runtimeRows([runtime('acme', '0.162.0')], '0.162.0', 'acme')[1].isDefault).toBe(false);
  });

  it('shows the built-in runtime a team server lists as the one built-in row, with its workers', () => {
    const rows = runtimeRows([catalog('built-in', { default: true, image: '' } as Partial<Runtime>), catalog('lab')], '0.162.0');
    expect(rows.map((r) => [r.key, r.type, r.state.label, r.isDefault])).toEqual([
      ['built-in', 'Built-in', '1 worker', true], ['catalog:lab', 'Worker image', '1 worker', false],
    ]);
    expect(runtimeDetail(rows[0], '0.162.0').facts).toContainEqual(['Workers', '1 connected · 2 slots']);
  });

  it('offers Make default on built-in and catalog rows that are not the default', () => {
    const rows = runtimeRows([catalog('lab'), runtime('acme', '0.162.0')], '0.162.0', 'lab');
    expect(rows.map((r) => [r.name, canBeDefault(r)])).toEqual([['built-in', true], ['acme', false], ['lab', false]]);
  });

  it('words the Default fact: who chose it and when, or that nobody has yet', () => {
    expect(defaultFact(undefined)).toBe('Requests that name no runtime run on it until the team chooses another');
    expect(defaultFact({ name: 'built-in' })).toBe('Requests that name no runtime run on it until the team chooses another');
    expect(defaultFact({ name: 'lab', set_by: 'ana', set: '2026-03-04T10:00:00Z' })).toMatch(/^Requests that name no runtime run on it · chosen by ana .*2026/);
    expect(defaultFact({ name: 'lab', set: '2026-03-04T10:00:00Z' })).toMatch(/^Requests that name no runtime run on it · chosen .*2026/);
  });

  it('adds the Default fact to the default row only', () => {
    const rows = runtimeRows([catalog('lab')], '0.162.0', 'lab');
    expect(runtimeDetail(rows[1], '0.162.0', { name: 'lab' }).facts[1]).toEqual(['Default', 'Requests that name no runtime run on it until the team chooses another']);
    expect(runtimeDetail(rows[0], '0.162.0', { name: 'lab' }).facts.map(([k]) => k)).not.toContain('Default');
  });
});

describe('selectedRow', () => {
  it('falls back to the built-in runtime when the selection is gone', () => {
    const rows = runtimeRows([runtime('acme', '0.136.0')], '0.136.0');
    expect(selectedRow(rows, 'runtime:acme').name).toBe('acme');
    expect(selectedRow(rows, 'runtime:removed').name).toBe('built-in');
    expect(selectedRow(rows, undefined).name).toBe('built-in');
  });
});

describe('runtimeDetail', () => {
  it('words an imported runtime from what the API reports', () => {
    const r = runtime('acme', '0.136.0', {
      manifest: 'processors:\n  - gomod: x v1\n  - gomod: y v1\n', tags: ['linea-runtime:acme'],
      custom_modules: [{ module: 'github.com/acme/enrich', source: './components/enrich' }],
      providers: [{ name: 'stamp', package: 'example.com/stamp', components: ['processor/stamp'] }],
    });
    const row = runtimeRows([r], '0.136.0')[1];
    const d = runtimeDetail(row, '0.136.0');
    expect(d.title).toBe('acme 0.136.0');
    expect(d.facts).toEqual([
      ['Type', 'Local container'], ['Verification', 'Verified release'], ['Image', '70.0 MB · acme0.136.0a'], ['Tags', 'linea-runtime:acme'],
    ]);
    expect(d.providers).toEqual(['stamp from example.com/stamp (processor/stamp)']);
    expect(d.custom).toEqual(['github.com/acme/enrich from ./components/enrich']);
  });

  it('names an unverified runtime and the built-in runtime by its release', () => {
    const unverified = runtimeRows([runtime('staging', '0.135.1', { verified: false })], '0.136.0')[1];
    expect(runtimeDetail(unverified, '0.136.0').facts[1]).toEqual(
      ['Verification', 'Unverified release: newer than the releases this debugger was verified with'],
    );
    expect(runtimeDetail(runtimeRows([], '0.136.0')[0], '0.136.0').title).toBe('built-in 0.136.0');
  });
});

describe('catalog runtimes', () => {
  const catalog = (patch: Partial<NonNullable<Runtime['catalog']>> = {}): Runtime => runtime('edge', '0.162.0', {
    image_id: '', size: 0,
    catalog: {
      identity: `sha256:${'a'.repeat(64)}`, image: 'registry.example/linea-runtime:edge', digest: `sha256:${'d'.repeat(64)}`,
      registered_by: 'alice', registered_at: '2026-10-30T09:00:00Z', workers: 0, slots: 0, ...patch,
    },
  });

  it.each([
    [{}, 'Unconfirmed', 'pending'],
    [{ confirmed_at: '2026-10-30T09:05:00Z' }, 'No worker', 'warn'],
    [{ confirmed_at: '2026-10-30T09:05:00Z', workers: 1, slots: 4 }, '1 worker', 'pass'],
    [{ confirmed_at: '2026-10-30T09:05:00Z', workers: 3, slots: 6 }, '3 workers', 'pass'],
    [{ confirmed_at: '2026-10-30T09:05:00Z', workers: 1, slots: 4, suspect: { cause: 'bad digest', pool: 'pool:linux/amd64', at: '2026-10-30T09:06:00Z' } }, 'Suspect binary', 'fail'],
  ])('words the workers of %j', (patch, label, tone) => {
    expect(catalogChip(catalog(patch).catalog!)).toMatchObject({ label, tone });
  });

  it('names the pool and cause of a suspect binary', () => {
    const r = catalog({
      binary: { digest: `sha256:${'b'.repeat(64)}`, bytes: 80_000_000, os: 'linux', arch: 'amd64' }, build: 'build-1',
      suspect: { cause: 'the downloaded runtime binary is not the one assigned', pool: 'pool:linux/amd64', at: '2026-10-30T09:06:00Z' },
    });
    const rows = runtimeRows([r], '0.162.0');
    const facts = Object.fromEntries(runtimeDetail(rows[1], '0.162.0').facts);
    expect(facts.Suspect).toContain('pool:linux/amd64 found the binary wrong');
    expect(facts.Suspect).toContain('not the one assigned');
  });

  it('lists a catalog runtime beside a built one of its name and words its registration', () => {
    const built = runtime('edge', '0.161.0', { tags: ['linea-runtime:edge'] });
    const rows = runtimeRows([catalog(), built], '0.162.0');
    expect(rows.map((r) => [r.key, r.type])).toEqual([
      ['built-in', 'Built-in'], ['catalog:edge', 'Worker image'], ['runtime:edge', 'Local container'],
    ]);
    const d = runtimeDetail(rows[1], '0.162.0');
    expect(d.subtitle).toBe('registered in the catalog · runs on connected workers');
    expect(Object.fromEntries(d.facts)).toMatchObject({
      Image: 'registry.example/linea-runtime:edge · dddddddddddd', Identity: 'aaaaaaaaaaaa',
      Confirmed: 'Not yet: no worker of this identity has connected', Workers: 'None connected: runs queue until one connects',
    });
  });
});

describe('built runtimes', () => {
  const built = runtime('mine', '0.162.0', {
    catalog: { identity: 'sha256:abc123abc123abc123', image: '', registered_at: '2026-10-31T10:00:00Z', registered_by: 'nico', slots: 8, workers: 1, build: 'b-1', binary: { digest: 'sha256:0123456789abcdef', bytes: 86_000_000, os: 'linux', arch: 'amd64' } },
  });
  it('lists a runtime a build registered as built, and words its binary instead of an image', () => {
    const [, row] = runtimeRows([built], '0.162.0');
    expect(row).toMatchObject({ key: 'catalog:mine', type: 'Built runtime' });
    const d = runtimeDetail(row, '0.162.0');
    expect(d.subtitle).toBe('built on this server · runs on pool workers');
    expect(d.facts).toContainEqual(['Binary', '86.0 MB · 0123456789ab · linux/amd64']);
    expect(d.facts.some(([k]) => k === 'Image')).toBe(false);
  });
});

describe('buildChip', () => {
  it('words every state of a build', () => {
    expect(['queued', 'running', 'done', 'cancelled', 'failed'].map((state) => buildChip({ state }).label)).toEqual(['Queued', 'Building', 'Built', 'Cancelled', 'Failed']);
  });
});
