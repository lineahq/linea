/**
 * Packing a module folder into the tar a team server's builders take. Pure, so
 * it is tested without a browser: the checks that mirror the server's
 * (internal/debugger/container/archive.go) and a ustar writer that produces the
 * same bytes for the same folder, so an unchanged folder is not uploaded twice
 * and a build of it can be reused.
 */

/** MAX_BUILD_BYTES is the most one build's module archives hold together. */
export const MAX_BUILD_BYTES = 64 * 1024 * 1024;
/** MAX_MODULE_BYTES and MAX_MODULE_FILES bound one module archive, unpacked. */
export const MAX_MODULE_BYTES = 64 * 1024 * 1024;
export const MAX_MODULE_FILES = 20000;
/** MAX_SOURCES is the most module archives one build takes. */
export const MAX_SOURCES = 32;

/** SKIPPED_DIRS are never packed, as the CLI leaves them out. */
export const SKIPPED_DIRS = ['.git', '.hg', '.svn', 'node_modules'];

/** FolderFile is one regular file of a picked folder: its path inside the folder and its size. */
export interface FolderFile<T = unknown> {
  path: string;
  size: number;
  data: T;
}

/**
 * pathProblem words why a path cannot go into an archive, or undefined: it
 * must be relative, use forward slashes and have no empty, "." or ".." element.
 */
export function pathProblem(path: string): string | undefined {
  if (path === '') return 'a file has no name';
  if (path.startsWith('/') || /^[A-Za-z]:/.test(path)) return `${path} is an absolute path`;
  if (path.includes('\\')) return `${path} has a backslash in its name`;
  // eslint-disable-next-line no-control-regex -- rejects control characters
  if (/[\u0000-\u001f\u007f]/.test(path)) return `${path} has a control character in its name`;
  for (const part of path.split('/')) {
    if (part === '' || part === '.' || part === '..') return `${path} is not a plain relative path`;
  }
  return undefined;
}

/** skipped is true for a path inside a directory the archive leaves out. */
export function skipped(path: string): boolean {
  const parts = path.split('/');
  return parts.slice(0, -1).some((p) => SKIPPED_DIRS.includes(p));
}

/** ModuleSelection is the files of a folder that go into its archive, or why none can. */
export interface ModuleSelection<T> {
  files: FolderFile<T>[];
  bytes: number;
  /** Files left out because they sit in .git, node_modules and the like. */
  skipped: number;
  problems: string[];
}

/**
 * selectModuleFiles keeps the files of a picked folder that belong in its
 * archive and checks them against the server's bounds: safe relative paths,
 * at most MAX_MODULE_FILES files and MAX_MODULE_BYTES bytes, a go.mod at the
 * root. Files in skipped directories are left out and counted. A path that is
 * unsafe is a problem, never silently dropped.
 */
export function selectModuleFiles<T>(files: readonly FolderFile<T>[]): ModuleSelection<T> {
  const out: FolderFile<T>[] = [];
  const problems: string[] = [];
  let left = 0;
  let bytes = 0;
  const seen = new Set<string>();
  for (const f of files) {
    const bad = pathProblem(f.path);
    if (bad) {
      if (!problems.includes(bad)) problems.push(bad);
      continue;
    }
    if (skipped(f.path)) { left++; continue; }
    if (seen.has(f.path)) { problems.push(`${f.path} appears twice`); continue; }
    seen.add(f.path);
    out.push(f);
    bytes += f.size;
  }
  if (out.length > MAX_MODULE_FILES) problems.push(`the folder has ${out.length} files; a build takes at most ${MAX_MODULE_FILES} per module`);
  if (bytes > MAX_MODULE_BYTES) problems.push(`the folder holds ${megabytes(bytes)}; a build takes at most ${megabytes(MAX_MODULE_BYTES)} per module`);
  if (!out.some((f) => f.path === 'go.mod')) problems.push('the folder has no go.mod at its root; pick the directory that holds it');
  out.sort((a, b) => comparePaths(a.path, b.path));
  return { files: out, bytes, skipped: left, problems: problems.slice(0, 5) };
}

/** megabytes prints a size in MiB, as the limits are stated. */
export function megabytes(n: number): string {
  return `${(n / (1024 * 1024)).toFixed(n < 10 * 1024 * 1024 ? 1 : 0)} MiB`;
}

/**
 * comparePaths orders paths as a directory walk lists them: element by
 * element, so a directory's files follow the directory.
 */
export function comparePaths(a: string, b: string): number {
  const x = a.split('/');
  const y = b.split('/');
  for (let i = 0; i < Math.min(x.length, y.length); i++) {
    if (x[i] !== y[i]) return x[i] < y[i] ? -1 : 1;
  }
  return x.length - y.length;
}

/** modulePath reads the module a go.mod declares, or undefined. */
export function modulePath(gomod: string): string | undefined {
  for (const raw of gomod.split(/\r?\n/)) {
    const m = /^\s*module\s+(?:"([^"]+)"|`([^`]+)`|(\S+))/.exec(raw);
    if (m) return m[1] ?? m[2] ?? m[3];
  }
  return undefined;
}

const encoder = new TextEncoder();

function octal(value: number, width: number): Uint8Array {
  const text = value.toString(8).padStart(width - 1, '0');
  if (text.length > width - 1) throw new Error(`${value} does not fit a tar field`);
  const out = new Uint8Array(width);
  out.set(encoder.encode(text));
  return out;
}

/**
 * splitName fits a path into a ustar header: the name field holds 100 bytes
 * and the prefix 155, joined by a slash. It returns undefined when no split
 * fits.
 */
export function splitName(path: string): { name: Uint8Array; prefix: Uint8Array } | undefined {
  const full = encoder.encode(path);
  if (full.length <= 100) return { name: full, prefix: new Uint8Array(0) };
  for (let i = full.length - 1; i > 0; i--) {
    if (full[i] !== 0x2f) continue;
    const prefix = full.subarray(0, i);
    const name = full.subarray(i + 1);
    if (prefix.length <= 155 && name.length <= 100 && name.length > 0) return { name, prefix };
  }
  return undefined;
}

function header(path: string, kind: 'file' | 'dir', size: number): Uint8Array {
  const entry = kind === 'dir' ? `${path}/` : path;
  const split = splitName(entry);
  if (!split) throw new Error(`${path} is too long to pack`);
  const h = new Uint8Array(512);
  h.set(split.name, 0);
  h.set(octal(kind === 'dir' ? 0o755 : 0o644, 8), 100);
  h.set(octal(0, 8), 108);
  h.set(octal(0, 8), 116);
  h.set(octal(size, 12), 124);
  h.set(octal(0, 12), 136);
  h.fill(0x20, 148, 156);
  h[156] = kind === 'dir' ? 0x35 : 0x30;
  h.set(encoder.encode('ustar'), 257);
  h.set(encoder.encode('00'), 263);
  h.set(split.prefix, 345);
  let sum = 0;
  for (const b of h) sum += b;
  h.set(encoder.encode(sum.toString(8).padStart(6, '0')), 148);
  h[154] = 0;
  h[155] = 0x20;
  return h;
}

const END = new Uint8Array(1024);

/**
 * packTar writes the files as an uncompressed ustar archive of directories
 * and regular files: sorted as a walk lists them, without owners, times or
 * permission bits that differ between files, so the same folder packs to the
 * same bytes. The file contents are blobs and are not read here; the result
 * is a blob built from them. It throws for a path that cannot be written.
 */
export function packTar(files: readonly FolderFile<Blob>[]): Blob {
  const sorted = [...files].sort((a, b) => comparePaths(a.path, b.path));
  const parts: BlobPart[] = [];
  const made = new Set<string>();
  for (const f of sorted) {
    const bad = pathProblem(f.path);
    if (bad) throw new Error(bad);
    const dirs = f.path.split('/').slice(0, -1);
    for (let i = 1; i <= dirs.length; i++) {
      const dir = dirs.slice(0, i).join('/');
      if (made.has(dir)) continue;
      made.add(dir);
      parts.push(header(dir, 'dir', 0) as BlobPart);
    }
    // The blob's own size is the truth: a file that changed since it was listed would corrupt the archive.
    const size = f.data.size;
    parts.push(header(f.path, 'file', size) as BlobPart, f.data);
    const pad = (512 - (size % 512)) % 512;
    if (pad) parts.push(new Uint8Array(pad) as BlobPart);
  }
  parts.push(END as BlobPart);
  return new Blob(parts, { type: 'application/x-tar' });
}
