import { describe, expect, it } from 'vitest';
import { heldFolders, holdFolders, rebuildSeed, recallBuild, rememberBuild } from './buildMemory';
import type { PickedFolder } from './buildSources';

class FakeStorage {
  items = new Map<string, string>();
  full = false;
  getItem(k: string) { return this.items.get(k) ?? null; }
  setItem(k: string, v: string) {
    if (this.full) throw new Error('QuotaExceededError');
    this.items.set(k, v);
  }
}
const picked = (): Record<string, PickedFolder> => ({ m: { name: 'm', files: [], bytes: 0, skipped: 0, problems: [] } });

describe('rememberBuild and recallBuild', () => {
  it('recalls the form a build was started from', () => {
    const s = new FakeStorage();
    rememberBuild(s, 'b1', { name: 'v0162', manifest: 'dist: {}\n', version: '0.162.0' });
    expect(recallBuild(s, 'b1')).toEqual({ name: 'v0162', manifest: 'dist: {}\n', version: '0.162.0' });
    expect(recallBuild(s, 'other')).toBeUndefined();
  });

  it('keeps the newest builds and moves a remembered one to the front', () => {
    const s = new FakeStorage();
    for (let i = 0; i < 25; i++) rememberBuild(s, `b${i}`, { name: `n${i}`, manifest: 'm' });
    expect(recallBuild(s, 'b24')).toBeDefined();
    expect(recallBuild(s, 'b5')).toBeDefined();
    expect(recallBuild(s, 'b4')).toBeUndefined();
    rememberBuild(s, 'b5', { name: 'again', manifest: 'm2' });
    const list = JSON.parse(s.items.get('linea.runtimeBuilds')!) as { id: string }[];
    expect(list[0].id).toBe('b5');
    expect(list.filter((e) => e.id === 'b5')).toHaveLength(1);
  });

  it('bounds the manifests together but always keeps the newest', () => {
    const s = new FakeStorage();
    const big = 'x'.repeat(1_000_000);
    rememberBuild(s, 'old', { name: 'o', manifest: big });
    rememberBuild(s, 'new', { name: 'n', manifest: big });
    expect(recallBuild(s, 'new')).toBeDefined();
    expect(recallBuild(s, 'old')).toBeUndefined();
  });

  it('survives a full, absent, broken or foreign store', () => {
    const full = new FakeStorage();
    full.full = true;
    expect(() => rememberBuild(full, 'b', { name: 'n', manifest: 'm' })).not.toThrow();
    expect(() => rememberBuild(undefined, 'b', { name: 'n', manifest: 'm' })).not.toThrow();
    expect(recallBuild(undefined, 'b')).toBeUndefined();
    const broken = new FakeStorage();
    broken.items.set('linea.runtimeBuilds', '{not json');
    expect(recallBuild(broken, 'b')).toBeUndefined();
    broken.items.set('linea.runtimeBuilds', JSON.stringify([{ id: 'ok', build: { name: 'n', manifest: 'm' } }, { id: 1 }, null, { id: 'bad', build: {} }]));
    expect(recallBuild(broken, 'ok')).toEqual({ name: 'n', manifest: 'm' });
    expect(recallBuild(broken, 'bad')).toBeUndefined();
    broken.items.set('linea.runtimeBuilds', '"a string"');
    expect(recallBuild(broken, 'ok')).toBeUndefined();
  });
});

describe('holdFolders', () => {
  it('holds folders for the page session, the newest few', () => {
    for (let i = 0; i < 7; i++) holdFolders(`h${i}`, picked());
    expect(heldFolders('h6')).toBeDefined();
    expect(heldFolders('h2')).toBeDefined();
    expect(heldFolders('h1')).toBeUndefined();
  });

  it('holds nothing for a build with no folders', () => {
    holdFolders('none', {});
    expect(heldFolders('none')).toBeUndefined();
  });
});

describe('rebuildSeed', () => {
  const build = { id: 'r1', name: 'mine', collector_version: '0.162.0' };

  it('seeds the manifest, release and held folders of a build this browser started', () => {
    const s = new FakeStorage();
    rememberBuild(s, 'r1', { name: 'mine', manifest: 'dist: {}\n', version: '0.161.0' });
    holdFolders('r1', picked());
    const seed = rebuildSeed(build, s, 3);
    expect(seed).toMatchObject({ key: 3, name: 'mine', version: '0.161.0', manifest: 'dist: {}\n', remembered: true });
    expect(seed.folders).toBeDefined();
  });

  it('seeds the server manifest for a build started elsewhere, without folders', () => {
    const seed = rebuildSeed({ ...build, id: 'elsewhere', manifest_yaml: 'dist: {}\n' }, new FakeStorage(), 2);
    expect(seed).toEqual({ key: 2, name: 'mine', version: '0.162.0', manifest: 'dist: {}\n', folders: undefined, remembered: false });
  });

  it('seeds the name and release alone for a build started elsewhere on a server without its manifest', () => {
    const seed = rebuildSeed({ ...build, id: 'elsewhere' }, new FakeStorage(), 1);
    expect(seed).toEqual({ key: 1, name: 'mine', version: '0.162.0', manifest: '', folders: undefined, remembered: false });
  });
});
