import type { PickedFolder } from './buildSources';

/**
 * What this browser remembers of the builds it started, so a build page can
 * offer Rebuild with what it was started from. The server returns a build's
 * manifest (GET, manifest_yaml) but never its sources, so the form is kept
 * here, per browser (localStorage, bounded: name, manifest and release), and
 * the picked folders only for this page session (memory): a reload asks for
 * the folders again. A build started elsewhere is not remembered; its Rebuild
 * starts from the server's manifest and asks for the folders.
 */

/** RememberedBuild is the form as a build was started from it. */
export interface RememberedBuild {
  name: string;
  manifest: string;
  /** The release the form was set to, so a rebuild keeps it. */
  version?: string;
}

const KEY = 'linea.runtimeBuilds';
/** KEPT bounds how many builds are remembered, newest first; MAX_CHARS bounds their manifests together. */
const KEPT = 20;
const MAX_CHARS = 1_500_000;

type Stored = { id: string; build: RememberedBuild }[];

function read(storage: Pick<Storage, 'getItem'> | undefined): Stored {
  try {
    const raw = storage?.getItem(KEY);
    const list = raw ? (JSON.parse(raw) as unknown) : [];
    return Array.isArray(list) ? list.filter((e): e is Stored[number] => !!e && typeof e.id === 'string' && typeof e.build?.manifest === 'string' && typeof e.build?.name === 'string') : [];
  } catch {
    return [];
  }
}

/** rememberBuild keeps the form a build was started from; storage may be absent or full, and then nothing is kept. */
export function rememberBuild(storage: Pick<Storage, 'getItem' | 'setItem'> | undefined, id: string, build: RememberedBuild): void {
  const list = [{ id, build }, ...read(storage).filter((e) => e.id !== id)].slice(0, KEPT);
  let chars = 0;
  const kept = list.filter((e) => (chars += e.build.manifest.length) <= MAX_CHARS || e.id === id);
  try {
    storage?.setItem(KEY, JSON.stringify(kept));
  } catch {
    // Per-browser convenience only: a full or blocked store keeps nothing.
  }
}

/** recallBuild is the form a build was started from in this browser, if it is still remembered. */
export function recallBuild(storage: Pick<Storage, 'getItem'> | undefined, id: string): RememberedBuild | undefined {
  return read(storage).find((e) => e.id === id)?.build;
}

/** browserStorage is localStorage when it can be used. */
export function browserStorage(): Storage | undefined {
  try {
    return typeof localStorage === 'undefined' ? undefined : localStorage;
  } catch {
    return undefined;
  }
}

const folders = new Map<string, Record<string, PickedFolder>>();

/** holdFolders keeps a build's picked folders for this page session, the newest few. */
export function holdFolders(id: string, picked: Record<string, PickedFolder>): void {
  if (Object.keys(picked).length === 0) return;
  folders.delete(id);
  folders.set(id, picked);
  while (folders.size > 5) folders.delete(folders.keys().next().value as string);
}

/** heldFolders are the folders a build of this page session was started with. */
export function heldFolders(id: string): Record<string, PickedFolder> | undefined {
  return folders.get(id);
}

/** FormSeed starts the builder from an earlier build. */
export interface FormSeed {
  /** key changes with every seed, so seeding the same build again resets the form. */
  key: number;
  name: string;
  /** The release the build was for. */
  version?: string;
  manifest: string;
  /** The folders that build was started with, when this page session still holds them. */
  folders?: Record<string, PickedFolder>;
  /** True when this browser started the build and remembered its form; false for a build started elsewhere. */
  remembered: boolean;
}

/**
 * rebuildSeed fills the builder with what a build was started from in this
 * browser: its manifest and release, and its folders while this page session
 * holds them. For a build started elsewhere the server's copy of the manifest
 * (manifest_yaml, as sent) seeds the form and the folders are asked for
 * again; a server that does not return it leaves the manifest to be written.
 */
export function rebuildSeed(
  build: { id: string; name: string; collector_version: string; manifest_yaml?: string },
  storage: Pick<Storage, 'getItem'> | undefined,
  key: number,
): FormSeed {
  const kept = recallBuild(storage, build.id);
  return {
    key,
    name: build.name,
    version: kept?.version ?? build.collector_version,
    manifest: kept?.manifest ?? build.manifest_yaml ?? '',
    folders: kept ? heldFolders(build.id) : undefined,
    remembered: !!kept,
  };
}
