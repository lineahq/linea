import { parse } from 'yaml';
import type { RuntimeBuildRequest } from '../api/types';
import {
  MAX_BUILD_BYTES, MAX_SOURCES, megabytes, modulePath, packTar, selectModuleFiles, type FolderFile,
} from './tar';

/**
 * The module sources of a server build: which modules of a
 * manifest the build takes from a local folder, a folder picked for each, and
 * the request that uploads them. A team server builds nothing from a path on
 * the user's machine, so each folder is packed in the browser and sent with
 * the manifest. Pure, so it is tested without a browser.
 */

const ENTRY_LISTS = ['receivers', 'processors', 'exporters', 'connectors', 'extensions', 'providers', 'converters'];

/**
 * localModules lists, sorted, the modules a manifest builds from a local
 * directory: `path:` entries and replaces whose target is a directory. It
 * reads only what it needs; a manifest that does not parse needs none, and
 * the server says why it is refused.
 */
export function localModules(manifest: string): string[] {
  let doc: unknown;
  try {
    doc = parse(manifest);
  } catch {
    return [];
  }
  if (!doc || typeof doc !== 'object') return [];
  const m = doc as Record<string, unknown>;
  const found = new Set<string>();
  const entry = (e: unknown) => {
    if (!e || typeof e !== 'object') return;
    const { gomod, path } = e as { gomod?: unknown; path?: unknown };
    if (typeof gomod !== 'string' || typeof path !== 'string' || path === '') return;
    const module = gomod.trim().split(/\s+/)[0];
    if (module) found.add(module);
  };
  for (const key of ENTRY_LISTS) if (Array.isArray(m[key])) m[key].forEach(entry);
  entry(m.telemetry);
  if (Array.isArray(m.replaces)) {
    for (const r of m.replaces) {
      if (typeof r !== 'string') continue;
      const [left, right] = r.split('=>');
      const lf = (left ?? '').trim().split(/\s+/).filter(Boolean);
      const rf = (right ?? '').trim().split(/\s+/).filter(Boolean);
      // A replace with a version is another module; one without is a directory.
      if (rf.length === 1 && lf.length >= 1) found.add(lf[0]);
    }
  }
  return [...found].sort();
}

/** PickedFolder is a folder chosen for a module: what it holds and what is wrong with it. */
export interface PickedFolder {
  /** The folder's name, as the user knows it. */
  name: string;
  files: FolderFile<Blob>[];
  bytes: number;
  skipped: number;
  problems: string[];
}

/**
 * inspectFolder checks a picked folder for a module before it is packed:
 * the server's bounds, safe paths, and a go.mod at the root that declares
 * the module the manifest names for it.
 */
export async function inspectFolder(name: string, module: string, files: readonly FolderFile<Blob>[]): Promise<PickedFolder> {
  const sel = selectModuleFiles(files);
  const problems = [...sel.problems];
  const gomod = sel.files.find((f) => f.path === 'go.mod');
  if (gomod) {
    const declared = modulePath(await gomod.data.text());
    if (!declared) problems.push('its go.mod declares no module');
    else if (declared !== module) problems.push(`its go.mod declares module ${declared}, not ${module}`);
  }
  return { name, files: sel.files, bytes: sel.bytes, skipped: sel.skipped, problems };
}

/** sourceStatus words what a module's folder slot holds for the form. */
export function sourceStatus(f: PickedFolder | undefined): string {
  if (!f) return 'No folder chosen';
  const files = `${f.files.length} ${f.files.length === 1 ? 'file' : 'files'}, ${megabytes(f.bytes)}`;
  return f.skipped ? `${f.name}: ${files} (${f.skipped} in .git or node_modules left out)` : `${f.name}: ${files}`;
}

/**
 * sourceProblems names what stops a build with these folders: a module
 * without a folder, a folder with a problem, or more than a build takes. It
 * is empty when the build can be sent.
 */
export function sourceProblems(modules: readonly string[], folders: Readonly<Record<string, PickedFolder | undefined>>): string[] {
  const out: string[] = [];
  let bytes = 0;
  for (const m of modules) {
    const f = folders[m];
    if (!f) { out.push(`Choose the folder of ${m}: the manifest builds it from a local directory.`); continue; }
    for (const p of f.problems) out.push(`${m}: ${p}`);
    bytes += f.bytes;
  }
  if (modules.length > MAX_SOURCES) out.push(`The manifest builds ${modules.length} modules from local directories; a build takes at most ${MAX_SOURCES}.`);
  if (bytes > MAX_BUILD_BYTES) out.push(`The folders hold ${megabytes(bytes)} together; a build takes at most ${megabytes(MAX_BUILD_BYTES)}.`);
  return out;
}

/** PreparedBuild is the request of a build and the archives it uploads, by the part that names each. */
export interface PreparedBuild {
  request: RuntimeBuildRequest;
  /** Module archives by multipart part name; empty when the build needs none. */
  archives: Record<string, Blob>;
}

/**
 * prepareBuild packs the folder of each local module the manifest names and
 * builds the request that names them. Folders of modules the manifest no
 * longer names are not sent. It throws if a folder cannot be packed; check
 * `sourceProblems` first.
 */
export function prepareBuild(o: {
  name: string;
  manifest: string;
  modules: readonly string[];
  folders: Readonly<Record<string, PickedFolder | undefined>>;
}): PreparedBuild {
  const archives: Record<string, Blob> = {};
  const sources: NonNullable<RuntimeBuildRequest['sources']> = [];
  o.modules.forEach((module, i) => {
    const part = `source${i + 1}`;
    const folder = o.folders[module];
    if (!folder) throw new Error(`${module} has no folder`);
    archives[part] = packTar(folder.files);
    sources.push({ module, part });
  });
  const request: RuntimeBuildRequest = { name: o.name, manifest_yaml: o.manifest, ...(sources.length ? { sources } : {}) };
  return { request, archives };
}
