import { describe, expect, it } from 'vitest';
import {
  comparePaths, MAX_MODULE_BYTES, MAX_MODULE_FILES, megabytes, modulePath, packTar, pathProblem, selectModuleFiles, skipped, splitName,
  type FolderFile,
} from './tar';

const file = (path: string, text = 'x'): FolderFile<Blob> => ({ path, size: text.length, data: new Blob([text]) });
const sized = (path: string, size: number): FolderFile<null> => ({ path, size, data: null });

interface Entry { name: string; type: string; size: number; mode: number; text: string }

/** read is a minimal ustar reader: just enough to check what packTar wrote. */
async function read(blob: Blob): Promise<{ entries: Entry[]; length: number }> {
  const b = new Uint8Array(await blob.arrayBuffer());
  const dec = new TextDecoder();
  const str = (o: number, n: number) => dec.decode(b.subarray(o, o + n)).replace(/\0.*$/s, '');
  const entries: Entry[] = [];
  let at = 0;
  while (at + 512 <= b.length && b.subarray(at, at + 512).some((x) => x !== 0)) {
    const size = parseInt(str(at + 124, 12), 8);
    const prefix = str(at + 345, 155);
    const name = prefix ? `${prefix}/${str(at, 100)}` : str(at, 100);
    // The checksum counts the header with its own field read as spaces.
    const h = b.slice(at, at + 512);
    h.fill(0x20, 148, 156);
    expect(parseInt(str(at + 148, 8), 8)).toBe(h.reduce((s, x) => s + x, 0));
    expect(str(at + 257, 6)).toBe('ustar');
    const body = dec.decode(b.subarray(at + 512, at + 512 + size));
    entries.push({ name, type: str(at + 156, 1), size, mode: parseInt(str(at + 100, 8), 8), text: body });
    at += 512 + Math.ceil(size / 512) * 512;
  }
  return { entries, length: b.length };
}

describe('pathProblem', () => {
  it.each([
    ['go.mod', undefined],
    ['internal/a/b.go', undefined],
    ['.hidden/x', undefined],
    ['', 'a file has no name'],
    ['/etc/passwd', '/etc/passwd is an absolute path'],
    ['C:/x', 'C:/x is an absolute path'],
    ['a\\b.go', 'a\\b.go has a backslash in its name'],
    ['../up.go', '../up.go is not a plain relative path'],
    ['a/../b.go', 'a/../b.go is not a plain relative path'],
    ['a//b.go', 'a//b.go is not a plain relative path'],
    ['./a.go', './a.go is not a plain relative path'],
    ['a/', 'a/ is not a plain relative path'],
    ['a\u0000b', 'a\u0000b has a control character in its name'],
  ])('%j', (path, want) => expect(pathProblem(path)).toBe(want));
});

describe('skipped', () => {
  it('leaves out files inside version control and dependency directories only', () => {
    expect(skipped('.git/config')).toBe(true);
    expect(skipped('a/node_modules/x/y.js')).toBe(true);
    expect(skipped('.svn/entries')).toBe(true);
    expect(skipped('git/config')).toBe(false);
    expect(skipped('.gitignore')).toBe(false);
    expect(skipped('node_modules')).toBe(false);
  });
});

describe('selectModuleFiles', () => {
  it('keeps and sorts the files, counts the skipped ones, and finds no problem', () => {
    const sel = selectModuleFiles([file('b/z.go'), file('go.mod', 'module m\n'), file('.git/HEAD'), file('a.go'), file('node_modules/p/i.js')]);
    expect(sel.files.map((f) => f.path)).toEqual(['a.go', 'b/z.go', 'go.mod']);
    expect(sel.skipped).toBe(2);
    expect(sel.bytes).toBe(1 + 1 + 9);
    expect(sel.problems).toEqual([]);
  });

  it('requires a go.mod at the root, not below it', () => {
    expect(selectModuleFiles([file('sub/go.mod'), file('a.go')]).problems).toEqual(['the folder has no go.mod at its root; pick the directory that holds it']);
  });

  it('reports unsafe paths instead of dropping them silently', () => {
    const sel = selectModuleFiles([file('go.mod'), file('../evil.go'), file('/abs.go'), file('a\\b.go'), file('../evil.go')]);
    expect(sel.problems).toEqual([
      '../evil.go is not a plain relative path',
      '/abs.go is an absolute path',
      'a\\b.go has a backslash in its name',
    ]);
    expect(sel.files.map((f) => f.path)).toEqual(['go.mod']);
  });

  it('refuses a path that appears twice', () => {
    expect(selectModuleFiles([file('go.mod'), file('a.go'), file('a.go')]).problems).toEqual(['a.go appears twice']);
  });

  it('enforces 64 MiB per module', () => {
    const ok = selectModuleFiles([sized('go.mod', 10), sized('big.bin', MAX_MODULE_BYTES - 10)]);
    expect(ok.problems).toEqual([]);
    const over = selectModuleFiles([sized('go.mod', 10), sized('big.bin', MAX_MODULE_BYTES - 9)]);
    expect(over.problems).toEqual(['the folder holds 64 MiB; a build takes at most 64 MiB per module']);
  });

  it('enforces 20000 files per module', () => {
    const many = (n: number) => [sized('go.mod', 1), ...Array.from({ length: n - 1 }, (_, i) => sized(`f${i}.go`, 1))];
    expect(selectModuleFiles(many(MAX_MODULE_FILES)).problems).toEqual([]);
    expect(selectModuleFiles(many(MAX_MODULE_FILES + 1)).problems).toEqual([`the folder has ${MAX_MODULE_FILES + 1} files; a build takes at most ${MAX_MODULE_FILES} per module`]);
  });

  it('shows at most five problems', () => {
    const bad = Array.from({ length: 9 }, (_, i) => sized(`../${i}`, 1));
    expect(selectModuleFiles(bad).problems).toHaveLength(5);
  });
});

describe('comparePaths', () => {
  it('lists a directory\'s files after the directory, element by element', () => {
    expect(['a.go', 'a/b.go', 'a-b.go', 'a/a.go'].sort(comparePaths)).toEqual(['a/a.go', 'a/b.go', 'a-b.go', 'a.go']);
  });
});

describe('modulePath', () => {
  it.each([
    ['module example.com/m\n\ngo 1.22\n', 'example.com/m'],
    ['// c\nmodule   "example.com/q"  // x\n', 'example.com/q'],
    ['module `example.com/r`\r\n', 'example.com/r'],
    ['go 1.22\n', undefined],
    ['// module not.this\n', undefined],
  ])('%j', (text, want) => expect(modulePath(text)).toBe(want));
});

describe('splitName', () => {
  const dec = new TextDecoder();
  it('keeps a short name whole', () => {
    const s = splitName('a/b.go')!;
    expect([dec.decode(s.prefix), dec.decode(s.name)]).toEqual(['', 'a/b.go']);
  });

  it('splits a long name at a slash into prefix and name', () => {
    const dir = `${'d'.repeat(60)}/${'e'.repeat(60)}`;
    const s = splitName(`${dir}/${'f'.repeat(50)}.go`)!;
    expect(dec.decode(s.prefix)).toBe(dir);
    expect(dec.decode(s.name)).toBe(`${'f'.repeat(50)}.go`);
  });

  it('gives up on a name no split fits', () => {
    expect(splitName('x'.repeat(101))).toBeUndefined();
    expect(splitName(`${'p/'.repeat(80)}${'n'.repeat(101)}`)).toBeUndefined();
  });
});

describe('packTar', () => {
  it('writes directories before their files, regular entries only, padded to 512 bytes, ended by two zero blocks', async () => {
    const blob = packTar([file('pkg/sub/b.go', 'package sub\n'), file('go.mod', 'module m\n'), file('pkg/a.go', 'a'.repeat(512))]);
    const { entries, length } = await read(blob);
    expect(entries.map((e) => [e.name, e.type, e.size])).toEqual([
      ['go.mod', '0', 9],
      ['pkg/', '5', 0],
      ['pkg/a.go', '0', 512],
      ['pkg/sub/', '5', 0],
      ['pkg/sub/b.go', '0', 12],
    ]);
    expect(entries.find((e) => e.name === 'go.mod')!.text).toBe('module m\n');
    expect(entries.find((e) => e.name === 'pkg/sub/b.go')!.text).toBe('package sub\n');
    // 5 headers + 1 + 1 + 2 data blocks (9 and 12 bytes round up; 512 stays) + 1024 end.
    expect(length).toBe(512 * (5 + 1 + 1 + 1) + 1024);
    expect(length % 512).toBe(0);
  });

  it('uses fixed modes and no owner or time, so the same folder packs to the same bytes', async () => {
    const a = packTar([file('go.mod'), file('d/x.go')]);
    const b = packTar([file('d/x.go'), file('go.mod')]);
    expect(new Uint8Array(await a.arrayBuffer())).toEqual(new Uint8Array(await b.arrayBuffer()));
    const { entries } = await read(a);
    expect(entries.map((e) => e.mode)).toEqual([0o755, 0o644, 0o644]);
  });

  it('writes a path longer than 100 bytes through the prefix field', async () => {
    const path = `${'d'.repeat(70)}/${'e'.repeat(70)}/main.go`;
    const { entries } = await read(packTar([file(path, 'x')]));
    expect(entries.at(-1)!.name).toBe(path);
  });

  it('takes the size from the blob, not from the listing', async () => {
    const f = { path: 'a.go', size: 999, data: new Blob(['abc']) };
    const { entries } = await read(packTar([f]));
    expect(entries[0]).toMatchObject({ name: 'a.go', size: 3, text: 'abc' });
  });

  it('writes an empty archive as the end block alone', async () => {
    expect((await read(packTar([]))).length).toBe(1024);
  });

  it.each(['../x.go', '/abs.go', 'a\\b.go', 'a/./b.go'])('throws for %j', (path) => {
    expect(() => packTar([file(path)])).toThrow();
  });

  it('throws for a path too long for ustar', () => {
    expect(() => packTar([file('n'.repeat(101))])).toThrow(/too long/);
  });
});

describe('megabytes', () => {
  it('prints MiB with one decimal below ten', () => {
    expect(megabytes(1.5 * 1024 * 1024)).toBe('1.5 MiB');
    expect(megabytes(64 * 1024 * 1024)).toBe('64 MiB');
  });
});
