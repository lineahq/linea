import { describe, expect, it } from 'vitest';
import { inspectFolder, localModules, prepareBuild, sourceProblems, sourceStatus, type PickedFolder } from './buildSources';
import { MAX_BUILD_BYTES, MAX_SOURCES, type FolderFile } from './tar';

const file = (path: string, text = 'x'): FolderFile<Blob> => ({ path, size: text.length, data: new Blob([text]) });
const folder = (patch: Partial<PickedFolder> = {}): PickedFolder => ({ name: 'dir', files: [file('go.mod', 'module m\n')], bytes: 9, skipped: 0, problems: [], ...patch });

describe('localModules', () => {
  it('lists modules built from a path, in every component list and the telemetry entry', () => {
    const manifest = `
dist:
  otelcol_version: 0.162.0
receivers:
  - gomod: go.opentelemetry.io/collector/receiver/otlpreceiver v0.162.0
processors:
  - gomod: example.com/b v0.0.0
    path: ./b
exporters:
  - gomod: example.com/a v0.0.0
    path: ../a
telemetry:
  gomod: example.com/t v0.0.0
  path: ./t
`;
    expect(localModules(manifest)).toEqual(['example.com/a', 'example.com/b', 'example.com/t']);
  });

  it('takes a replace to a directory, not one to a version of another module', () => {
    const manifest = `
replaces:
  - example.com/x => ./x
  - example.com/y v1.0.0 => ../y
  - example.com/z => example.org/z v1.2.3
`;
    expect(localModules(manifest)).toEqual(['example.com/x', 'example.com/y']);
  });

  it.each([
    ['empty', ''],
    ['not YAML', 'a: [unclosed'],
    ['a scalar', '42'],
    ['lists of the wrong shape', 'processors: nope\nreplaces: 3\n'],
    ['entries without a path', 'processors:\n  - gomod: example.com/p v1.0.0\n'],
    ['an empty path', 'processors:\n  - gomod: example.com/p v1.0.0\n    path: ""\n'],
  ])('finds none in %s', (_n, manifest) => expect(localModules(manifest)).toEqual([]));

  it('lists a module once', () => {
    const manifest = 'processors:\n  - gomod: example.com/p v0.0.0\n    path: ./p\nreplaces:\n  - example.com/p => ./p\n';
    expect(localModules(manifest)).toEqual(['example.com/p']);
  });
});

describe('inspectFolder', () => {
  it('accepts a folder whose go.mod declares the module', async () => {
    const f = await inspectFolder('mod', 'example.com/m', [file('main.go', 'package m'), file('go.mod', 'module example.com/m\n'), file('.git/HEAD')]);
    expect(f.problems).toEqual([]);
    expect(f.name).toBe('mod');
    expect(f.files.map((x) => x.path)).toEqual(['go.mod', 'main.go']);
    expect(f.skipped).toBe(1);
  });

  it('says when the go.mod declares another module or none', async () => {
    expect((await inspectFolder('m', 'example.com/m', [file('go.mod', 'module example.com/other\n')])).problems)
      .toEqual(['its go.mod declares module example.com/other, not example.com/m']);
    expect((await inspectFolder('m', 'example.com/m', [file('go.mod', 'go 1.22\n')])).problems).toEqual(['its go.mod declares no module']);
  });

  it('carries the archive checks: no go.mod at the root, unsafe paths', async () => {
    expect((await inspectFolder('m', 'x', [file('a.go')])).problems).toEqual(['the folder has no go.mod at its root; pick the directory that holds it']);
    expect((await inspectFolder('m', 'x', [file('go.mod', 'module x'), file('../up.go')])).problems).toEqual(['../up.go is not a plain relative path']);
  });
});

describe('sourceStatus', () => {
  it('words an empty slot, a folder and a folder with files left out', () => {
    expect(sourceStatus(undefined)).toBe('No folder chosen');
    expect(sourceStatus(folder({ files: [file('go.mod'), file('a.go')], bytes: 2048 }))).toBe('dir: 2 files, 0.0 MiB');
    expect(sourceStatus(folder({ files: [file('go.mod')], bytes: 1024 * 1024, skipped: 4 }))).toBe('dir: 1 file, 1.0 MiB (4 in .git or node_modules left out)');
  });
});

describe('sourceProblems', () => {
  it('asks for a folder of each local module', () => {
    expect(sourceProblems(['a/m', 'b/m'], { 'a/m': folder() })).toEqual(['Choose the folder of b/m: the manifest builds it from a local directory.']);
    expect(sourceProblems([], {})).toEqual([]);
  });

  it('prefixes each folder problem with its module', () => {
    expect(sourceProblems(['a/m'], { 'a/m': folder({ problems: ['p1', 'p2'] }) })).toEqual(['a/m: p1', 'a/m: p2']);
  });

  it('bounds the folders together and the number of modules', () => {
    const half = folder({ bytes: MAX_BUILD_BYTES / 2 + 1 });
    expect(sourceProblems(['a', 'b'], { a: half, b: half })).toEqual(['The folders hold 64 MiB together; a build takes at most 64 MiB.']);
    const many = Array.from({ length: MAX_SOURCES + 1 }, (_, i) => `m${i}`);
    const all = Object.fromEntries(many.map((m) => [m, folder()]));
    expect(sourceProblems(many, all)).toEqual([`The manifest builds ${MAX_SOURCES + 1} modules from local directories; a build takes at most ${MAX_SOURCES}.`]);
  });
});

describe('prepareBuild', () => {
  it('is a plain request without local modules', () => {
    expect(prepareBuild({ name: 'v0162', manifest: 'dist: {}\n', modules: [], folders: {} })).toEqual({
      request: { name: 'v0162', manifest_yaml: 'dist: {}\n' },
      archives: {},
    });
  });

  it('packs one archive per module and names it in the sources, ignoring folders of modules the manifest dropped', async () => {
    const p = prepareBuild({
      name: 'mine', manifest: 'm', modules: ['a/m', 'b/m'],
      folders: { 'a/m': folder(), 'b/m': folder({ files: [file('go.mod', 'module b/m\n'), file('x.go')] }), 'gone/m': folder() },
    });
    expect(p.request).toEqual({ name: 'mine', manifest_yaml: 'm', sources: [{ module: 'a/m', part: 'source1' }, { module: 'b/m', part: 'source2' }] });
    expect(Object.keys(p.archives)).toEqual(['source1', 'source2']);
    expect(p.archives.source1.type).toBe('application/x-tar');
    expect(p.archives.source2.size).toBeGreaterThan(p.archives.source1.size);
  });

  it('throws for a module without a folder', () => {
    expect(() => prepareBuild({ name: 'n', manifest: 'm', modules: ['a/m'], folders: {} })).toThrow('a/m has no folder');
  });
});
