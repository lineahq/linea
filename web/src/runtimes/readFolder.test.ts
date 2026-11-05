import { describe, expect, it } from 'vitest';
import { filesOfEntry, filesOfInput, folderOfDrop, TRAVERSAL_LIMIT, type DropItem, type EntryLike } from './readFolder';

const picked = (rel: string, text = 'x') => Object.assign(new File([text], rel.split('/').at(-1)!), { webkitRelativePath: rel });

describe('filesOfInput', () => {
  it('drops the folder\'s own name from each path and leaves out .git and node_modules', () => {
    const got = filesOfInput([picked('mod/go.mod', 'module m'), picked('mod/pkg/a.go'), picked('mod/.git/HEAD'), picked('mod/node_modules/p/i.js'), picked('mod/.gitignore')])!;
    expect(got.name).toBe('mod');
    expect(got.files.map((f) => [f.path, f.size])).toEqual([['go.mod', 8], ['pkg/a.go', 1], ['.gitignore', 1]]);
  });

  it('is undefined when the browser gave no relative paths or no files', () => {
    expect(filesOfInput([new File(['x'], 'a.go')])).toBeUndefined();
    expect(filesOfInput([])).toBeUndefined();
  });
});

/** dir and leaf build fake FileSystemEntry trees; a reader gives its entries a batch at a time. */
const leaf = (name: string, text = 'x'): EntryLike => ({ isFile: true, isDirectory: false, name, file: (ok) => ok(new File([text], name)) });
const dir = (name: string, children: EntryLike[], batch = 2): EntryLike => ({
  isFile: false, isDirectory: true, name,
  createReader: () => {
    let at = 0;
    return { readEntries: (ok) => { const out = children.slice(at, at + batch); at += batch; ok(out); } };
  },
});

describe('filesOfEntry', () => {
  it('walks a dropped folder across reader batches, skipping .git and node_modules', async () => {
    const root = dir('mod', [leaf('go.mod', 'module m'), dir('pkg', [leaf('a.go'), leaf('b.go')]), dir('.git', [leaf('HEAD')]), dir('node_modules', [leaf('i.js')]), leaf('z.go')]);
    const got = await filesOfEntry(root);
    expect(got.name).toBe('mod');
    expect(got.files.map((f) => f.path).sort()).toEqual(['go.mod', 'pkg/a.go', 'pkg/b.go', 'z.go']);
    expect(got.files.find((f) => f.path === 'go.mod')!.size).toBe(8);
  });

  it('refuses a tree larger than the traversal bound', async () => {
    const many = Array.from({ length: TRAVERSAL_LIMIT + 1 }, (_, i) => leaf(`f${i}`));
    await expect(filesOfEntry(dir('big', many, 1000))).rejects.toThrow(/more than/);
  });

  it('passes on a file that cannot be read', async () => {
    const bad: EntryLike = { isFile: true, isDirectory: false, name: 'x', file: (_ok, fail) => fail(new Error('denied')) };
    await expect(filesOfEntry(dir('d', [bad]))).rejects.toThrow('denied');
  });
});

describe('folderOfDrop', () => {
  const item = (entry: EntryLike | null, kind = 'file'): DropItem => ({ kind, webkitGetAsEntry: () => entry });

  it('reads one dropped folder', async () => {
    const got = await folderOfDrop([item(dir('mod', [leaf('go.mod')]))]);
    expect(got.name).toBe('mod');
    expect(got.files.map((f) => f.path)).toEqual(['go.mod']);
  });

  it('words what it cannot take', async () => {
    await expect(folderOfDrop([])).rejects.toThrow('Drop one folder');
    await expect(folderOfDrop([item(dir('a', [])), item(dir('b', []))])).rejects.toThrow('Drop one folder');
    await expect(folderOfDrop([item(null)])).rejects.toThrow('cannot read a dropped folder');
    await expect(folderOfDrop([item(leaf('go.mod'), 'string')])).rejects.toThrow('cannot read a dropped folder');
    await expect(folderOfDrop([item(leaf('go.mod'))])).rejects.toThrow('go.mod is a file');
  });
});
