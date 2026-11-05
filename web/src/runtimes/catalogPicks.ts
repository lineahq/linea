import type { CatalogComponent, CollectorCatalog, ManifestRequest } from '../api/types';
import { catalogGroups, componentKey, type CatalogGroup } from './runtimes';

/**
 * Component picking for the runtime form: what a start (template) selects in
 * the catalog, how the picks differ from it, and the picker's view. Picks are
 * catalog keys, "<kind>/<type>", as the manifest request names them.
 */

/** Start is how the manifest starts; every start but import preselects picks. */
export type Start = 'built-in' | 'config' | 'all' | 'pick' | 'import';

export const STARTS: { id: Start; label: string; note: string }[] = [
  { id: 'built-in', label: 'Standard set', note: 'The built-in runtime\'s components. Add the ones you need.' },
  { id: 'config', label: 'My configuration', note: 'Every component the open configuration uses.' },
  { id: 'all', label: 'All components', note: 'Every core and contrib component the debugger can run: a large, slow build.' },
  { id: 'pick', label: 'Start empty', note: 'Pick each component yourself.' },
  { id: 'import', label: 'Import a manifest', note: 'Paste or open an OCB manifest.' },
];

const KINDS = ['receivers', 'processors', 'exporters', 'connectors', 'extensions'];

/** ManifestEntry is one component entry of an OCB manifest. */
export interface ManifestEntry { kind: string; module: string; import?: string }

const unquote = (s: string) => s.replace(/^["']|["']$/g, '');

/** manifestEntries reads the component entries of an OCB manifest, by kind. */
export function manifestEntries(text: string): ManifestEntry[] {
  const out: ManifestEntry[] = [];
  let section = '';
  let last: ManifestEntry | undefined;
  for (const line of text.split(/\r?\n/)) {
    const top = /^([a-z_]+):\s*(#.*)?$/.exec(line);
    if (top) { section = top[1]; last = undefined; continue; }
    if (!KINDS.includes(section)) continue;
    const item = /^\s*-\s*gomod:\s*(\S+)/.exec(line);
    if (item) { last = { kind: section, module: unquote(item[1]) }; out.push(last); continue; }
    const imp = /^\s+import:\s*(\S+)/.exec(line);
    if (imp && last) last.import = unquote(imp[1]);
    else if (/^\s*-/.test(line)) last = undefined;
  }
  return out;
}

const modulePath = (gomod: string) => gomod.trim().split(/\s+/)[0];

/**
 * catalogKeysOf matches a manifest's entries to a catalog by kind, module path
 * and import, so a manifest of one release selects the same components in
 * another; `missing` names the entries the catalog lacks.
 */
export function catalogKeysOf(text: string, catalog: CollectorCatalog): { keys: string[]; missing: string[] } {
  const keys: string[] = [];
  const missing: string[] = [];
  for (const e of manifestEntries(text)) {
    const c = (catalog.components ?? []).find((x) => x.kind === e.kind && modulePath(x.gomod) === e.module && (x.import ?? '') === (e.import ?? ''));
    if (c) { if (!keys.includes(componentKey(c))) keys.push(componentKey(c)); } else missing.push(`${e.kind}/${e.import ?? e.module}`);
  }
  return { keys, missing };
}

/** supportedKeys is every component the debugger can run: the "all" preset. */
export function supportedKeys(catalog: CollectorCatalog): string[] {
  return (catalog.components ?? []).filter((c) => !c.unsupported).map(componentKey);
}

/** templateDiff is how the picks differ from the start's components. */
export function templateDiff(picked: ReadonlySet<string>, template: readonly string[]): { added: string[]; removed: string[] } {
  const base = new Set(template);
  return {
    added: [...picked].filter((k) => !base.has(k)).sort(),
    removed: template.filter((k) => !picked.has(k)).sort(),
  };
}

/**
 * manifestRequest asks for the picks. An unchanged start keeps its preset: the
 * standard set only at the built-in runtime's release, where it is that
 * runtime's manifest as written; elsewhere the picks are listed.
 */
export function manifestRequest(o: {
  start: Start; release: string; builtIn: string; picked: ReadonlySet<string>; template?: readonly string[];
}): ManifestRequest {
  const req: ManifestRequest = { version: o.release };
  const d = o.template ? templateDiff(o.picked, o.template) : undefined;
  const unchanged = !!d && !d.added.length && !d.removed.length;
  if (unchanged && o.start === 'all') req.preset = 'all';
  else if (unchanged && o.start === 'built-in' && o.release === o.builtIn) req.preset = 'built-in';
  else req.components = [...o.picked].sort();
  return req;
}

export interface KindCount { kind: string; picked: number; total: number }

/** kindCounts counts the picks of each kind in pipeline order. */
export function kindCounts(catalog: CollectorCatalog | undefined, picked: ReadonlySet<string>): KindCount[] {
  return catalogGroups(catalog, '').map((g) => ({
    kind: g.kind, total: g.components.length, picked: g.components.filter((c) => picked.has(componentKey(c))).length,
  }));
}

/** PickerView is what the picker lists: a search, one kind or all, and every component or the picked ones. */
export interface PickerView { filter: string; kind: string; selectedOnly: boolean }

/** pickerGroups lists the catalog for a view; a group's `picked` counts its whole kind. */
export function pickerGroups(catalog: CollectorCatalog | undefined, picked: ReadonlySet<string>, view: PickerView): (CatalogGroup & { picked: number; total: number })[] {
  const counts = new Map(kindCounts(catalog, picked).map((k) => [k.kind, k]));
  return catalogGroups(catalog, view.filter)
    .filter((g) => !view.kind || g.kind === view.kind)
    .map((g) => ({
      ...g,
      components: view.selectedOnly ? g.components.filter((c) => picked.has(componentKey(c))) : g.components,
      picked: counts.get(g.kind)?.picked ?? 0,
      total: counts.get(g.kind)?.total ?? 0,
    }))
    .filter((g) => g.components.length > 0);
}

/** stabilityText words a component's stability by level: "beta: metrics · alpha: logs". */
export function stabilityText(c: Pick<CatalogComponent, 'stability'>): string {
  const order = ['stable', 'beta', 'alpha', 'development', 'deprecated', 'unmaintained'];
  return Object.entries(c.stability ?? {})
    .sort(([a], [b]) => (order.indexOf(a) + 99) % 99 - (order.indexOf(b) + 99) % 99)
    .map(([level, signals]) => (signals.length ? `${level}: ${signals.join(', ')}` : level))
    .join(' · ');
}

/** RuntimeComponent is one component a runtime's manifest names. */
export interface RuntimeComponent {
  /** The catalog type, or the package's last path element when the catalog lacks it. */
  name: string;
  /** The package: the import path, or the module. */
  module: string;
  /** False for a module the release's catalog does not have (a custom one); undefined without a catalog. */
  inCatalog?: boolean;
}

/**
 * runtimeComponents lists what a runtime was built with, by kind in pipeline
 * order: its manifest's entries named by the release's catalog when there is
 * one, otherwise by their package.
 */
export function runtimeComponents(manifest: string, catalog?: CollectorCatalog): { kind: string; items: RuntimeComponent[] }[] {
  const byKind = new Map<string, RuntimeComponent[]>();
  for (const e of manifestEntries(manifest)) {
    const pkg = e.import ?? e.module;
    const c = catalog?.components?.find((x) => x.kind === e.kind && modulePath(x.gomod) === e.module && (x.import ?? '') === (e.import ?? ''));
    const item: RuntimeComponent = { name: c?.type ?? pkg.split('/').at(-1)!, module: pkg };
    if (catalog) item.inCatalog = !!c;
    byKind.set(e.kind, [...(byKind.get(e.kind) ?? []), item]);
  }
  return KINDS.filter((k) => byKind.has(k))
    .map((kind) => ({ kind, items: byKind.get(kind)!.sort((a, b) => a.name.localeCompare(b.name)) }));
}
