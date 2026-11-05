import { MAX_MODULE_FILES, SKIPPED_DIRS, type FolderFile } from './tar';

/**
 * Reading a folder the user chose (the directory picker) or dropped (drag and
 * drop) into the files the build packs. The DOM objects are read through the
 * small shapes below, so the traversal is tested with plain fakes. Symbolic
 * links are not visible from a browser: a picked or dropped link arrives as
 * the file or folder it points to.
 */

export interface PickedFiles {
  /** The folder's own name. */
  name: string;
  files: FolderFile<Blob>[];
}

/** TRAVERSAL_LIMIT bounds the entries a dropped folder is walked through, so a huge tree is refused rather than read. */
export const TRAVERSAL_LIMIT = MAX_MODULE_FILES * 2;

/**
 * filesOfInput reads the files of a directory picker (`webkitdirectory`):
 * each carries its path from the chosen folder's parent, so the folder's own
 * name is dropped from the front. Files inside .git and the like are not
 * listed, as a huge tree would only be refused.
 */
export function filesOfInput(list: ArrayLike<File>): PickedFiles | undefined {
  const files: FolderFile<Blob>[] = [];
  let name = '';
  for (let i = 0; i < list.length; i++) {
    const f = list[i];
    const rel = (f as File & { webkitRelativePath?: string }).webkitRelativePath ?? '';
    const cut = rel.indexOf('/');
    if (cut <= 0) return undefined;
    name = rel.slice(0, cut);
    const path = rel.slice(cut + 1);
    if (path.split('/').slice(0, -1).some((p) => SKIPPED_DIRS.includes(p))) continue;
    files.push({ path, size: f.size, data: f });
  }
  return name ? { name, files } : undefined;
}

/** EntryLike is the part of a FileSystemEntry the traversal uses. */
export interface EntryLike {
  isFile: boolean;
  isDirectory: boolean;
  name: string;
  file?(ok: (f: File) => void, fail: (e: unknown) => void): void;
  createReader?(): { readEntries(ok: (entries: EntryLike[]) => void, fail: (e: unknown) => void): void };
}

const fileOf = (e: EntryLike) => new Promise<File>((ok, fail) => (e.file ? e.file(ok, fail) : fail(new Error(`${e.name} cannot be read`))));
const listOf = async (e: EntryLike): Promise<EntryLike[]> => {
  if (!e.createReader) return [];
  const reader = e.createReader();
  const all: EntryLike[] = [];
  // readEntries returns a batch at a time (Chrome: 100) until it returns none.
  for (;;) {
    const batch = await new Promise<EntryLike[]>((ok, fail) => reader.readEntries(ok, fail));
    if (batch.length === 0) return all;
    all.push(...batch);
  }
};

/** filesOfEntry walks a dropped directory entry, leaving out .git, node_modules and the like. */
export async function filesOfEntry(root: EntryLike): Promise<PickedFiles> {
  const files: FolderFile<Blob>[] = [];
  let visited = 0;
  const walk = async (dir: EntryLike, prefix: string) => {
    for (const e of await listOf(dir)) {
      if (++visited > TRAVERSAL_LIMIT) throw new Error(`The folder has more than ${TRAVERSAL_LIMIT} entries; a build takes at most ${MAX_MODULE_FILES} files per module.`);
      if (e.isDirectory) {
        if (!SKIPPED_DIRS.includes(e.name)) await walk(e, `${prefix}${e.name}/`);
      } else if (e.isFile) {
        const f = await fileOf(e);
        files.push({ path: `${prefix}${e.name}`, size: f.size, data: f });
      }
    }
  };
  await walk(root, '');
  return { name: root.name, files };
}

/** DropItem is the part of a DataTransferItem the drop reads. */
export interface DropItem { kind: string; webkitGetAsEntry(): EntryLike | null }

/**
 * folderOfDrop reads one dropped folder. Several items, a plain file or a
 * browser without directory entries is an error the form words.
 */
export async function folderOfDrop(items: ArrayLike<DropItem>): Promise<PickedFiles> {
  if (items.length !== 1) throw new Error('Drop one folder for this module.');
  const entry = items[0].kind === 'file' ? items[0].webkitGetAsEntry?.() : null;
  if (!entry) throw new Error('This browser cannot read a dropped folder; use Choose folder.');
  if (!entry.isDirectory) throw new Error(`${entry.name} is a file; drop the folder that holds the module's go.mod.`);
  return filesOfEntry(entry);
}
