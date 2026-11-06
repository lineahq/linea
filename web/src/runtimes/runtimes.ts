import { ApiError } from '../api/client';
import type {
  CatalogComponent, CollectorCatalog, OperationError, PrunedItem, ReportProvider, ReportRuntime, Runtime, RuntimeBuild, RuntimePruneResult,
} from '../api/types';

/**
 * The runtimes view model: the selector's options, how a report
 * names its runtime, build names and logs, catalog picking, and the storage
 * and image-reference helpers. A runtime is chosen in one place only, the
 * selector beside Run; everything else here builds or manages runtimes.
 */

/**
 * BUILT_IN is the selector value that sends no `runtime`: the team's default
 * runtime answers, which is the built-in one until the team
 * chooses another. BUILT_IN_NAME is how the server names the built-in runtime
 * and the selector value that asks for it explicitly.
 */
export const BUILT_IN = '';
export const BUILT_IN_NAME = 'built-in';

export interface RuntimeOption {
  value: string;
  label: string;
  unverified: boolean;
  /** The selected runtime is not among the built ones (removed, or Docker is unavailable). */
  missing: boolean;
}

/**
 * runtimeOptions lists the team's default first (the empty value: no runtime
 * sent), then the built-in runtime when the default is another one, then built
 * runtimes by name. `defaultName` is the default the server reports; the
 * default option only says `Default` while it is unknown. An unverified
 * runtime always says so in its label; a selected runtime that no longer
 * exists stays listed, marked missing, so the selection is never silently
 * changed.
 */
export function runtimeOptions(runtimes: Runtime[] | undefined, selected: string, builtInVersion?: string, defaultName?: string): RuntimeOption[] {
  // A name selects one image: an older one a pushed reference keeps is not
  // another choice. A team server lists the built-in runtime too; it is the
  // built-in option, not another.
  const all = (runtimes ?? []).filter((r) => r.name !== BUILT_IN_NAME);
  const sorted = all.filter((r) => !olderImage(r, all)).sort((a, b) => a.name.localeCompare(b.name));
  const out: RuntimeOption[] = [defaultOption(sorted, builtInVersion, defaultName)];
  // Built-in stays choosable when the default is another runtime (or not
  // known yet); a selection that names it stays listed.
  if (defaultName !== BUILT_IN_NAME || selected === BUILT_IN_NAME) {
    out.push({ value: BUILT_IN_NAME, label: `Built-in${builtInVersion ? ` · Collector ${builtInVersion}` : ''}`, unverified: false, missing: false });
  }
  for (const r of sorted) {
    out.push({
      value: r.name,
      label: `${r.name} · Collector ${r.collector_version}${r.verified ? '' : ' · unverified'}`,
      unverified: !r.verified,
      missing: false,
    });
  }
  if (selected !== BUILT_IN && selected !== BUILT_IN_NAME && !sorted.some((r) => r.name === selected)) {
    out.push({ value: selected, label: `${selected} · not found`, unverified: false, missing: true });
  }
  return out;
}

/** defaultOption words the empty option: "Default · name · Collector version", shorter while the default or its version is unknown. */
function defaultOption(sorted: Runtime[], builtInVersion: string | undefined, defaultName: string | undefined): RuntimeOption {
  const option = (label: string, unverified = false): RuntimeOption => ({ value: BUILT_IN, label, unverified, missing: false });
  if (!defaultName) return option('Default');
  if (defaultName === BUILT_IN_NAME) return option(`Default · ${BUILT_IN_NAME}${builtInVersion ? ` · Collector ${builtInVersion}` : ''}`);
  const r = sorted.find((x) => x.name === defaultName);
  if (!r) return option(`Default · ${defaultName}`);
  return option(`Default · ${defaultName} · Collector ${r.collector_version}${r.verified ? '' : ' · unverified'}`, !r.verified);
}

/**
 * effectiveRuntime is the runtime name a selection resolves to: the team's
 * default for the empty selection, and empty (BUILT_IN) for the built-in
 * runtime. Undefined while the default is unknown.
 */
export function effectiveRuntime(selected: string, defaultName: string | undefined): string | undefined {
  const name = selected === BUILT_IN ? defaultName : selected;
  if (name === undefined) return undefined;
  return name === BUILT_IN_NAME ? BUILT_IN : name;
}

/** runtimeField is the request field for a selection: absent for the built-in runtime. */
export function runtimeField(selected: string | undefined): { runtime?: string } {
  return selected ? { runtime: selected } : {};
}

export interface RuntimeFacts {
  label: string;
  unverified: boolean;
  /** Custom modules with their source and commit. */
  custom: string[];
  /** Module providers compiled in (report schema 0.13): name, package and the types they drive. */
  providers: string[];
  /** Optional analyses the runtime lacks, worded (report schema 0.10). */
  missing: string[];
}

/** FEATURES words each optional feature a runtime can lack. */
const FEATURES: Record<string, string> = {
  ottl_replay: 'transform statement replay (no processor/transform)',
  count_aggregation: 'count connector aggregation (no connector/count)',
  routing_explanation: 'routing explanations (no connector/routing)',
  sampling_explanation: 'tail_sampling explanations (no processor/tail_sampling)',
};

/** reportRuntime states the runtime a report ran on; reports before 0.9 have none. */
export function reportRuntime(rt: ReportRuntime | undefined): RuntimeFacts | undefined {
  if (!rt) return undefined;
  const name = rt.kind === 'built_in' ? 'Built-in' : (rt.name || rt.image_id?.slice(0, 19) || 'container');
  return {
    label: `${name} · Collector ${rt.collector_version}${rt.kind === 'remote' ? ` · remote worker${rt.endpoint ? ` at ${rt.endpoint}` : ''}` : ''}`,
    unverified: !rt.verified,
    custom: (rt.custom_modules ?? []).map((m) => `${m.module} from ${m.source}${m.commit ? ` @ ${m.commit.slice(0, 12)}` : ''}`),
    providers: providerLines(rt.providers),
    missing: (rt.missing_features ?? []).map((f) => FEATURES[f] ?? f),
  };
}

/** providerLines words a runtime's module providers: "stamp from example.com/stamp/debug (processor/stamp)". */
export function providerLines(ps: { name: string; package: string; components?: string[] }[] | undefined): string[] {
  return (ps ?? []).map((p) => `${p.name} from ${p.package}${p.components?.length ? ` (${p.components.join(', ')})` : ''}`);
}

/**
 * runtimeProviders is the module providers compiled into the selected
 * runtime (the team's default for the empty selection): none for the
 * built-in one, and undefined while that is unknown (the default or the list
 * is not loaded, or the list has no runtime by that name).
 */
export function runtimeProviders(runtimes: Runtime[] | undefined, selected: string, defaultName?: string): ReportProvider[] | undefined {
  const name = effectiveRuntime(selected, defaultName);
  if (name === undefined) return undefined;
  if (name === BUILT_IN) return [];
  const all = runtimes ?? [];
  const r = all.find((x) => x.name === name && !olderImage(x, all));
  return r ? (r.providers ?? []) : undefined;
}

/** UNVERIFIED explains the unverified badge wherever it appears. */
export const UNVERIFIED =
  'This Collector release is newer than the releases this debugger was verified with. It runs, but its evidence is unverified.';

/**
 * problemOf words a refused request: the message and, for a refused
 * manifest, each diagnostic (validation_failed always carries them). A 503
 * means the runtime store is unavailable, except for a build (`plain`), where
 * it is a full queue or a server without builders, which the message says.
 */
export function problemOf(e: unknown, plain = false): { message: string; details: string[] } {
  if (e instanceof ApiError) {
    const body = e.body as OperationError | undefined;
    const details = (body?.diagnostics ?? []).map((d) => `${d.severity === 'error' ? '' : `${d.severity}: `}${d.message}`);
    if (e.status === 503 && !plain) return { message: `Runtimes are unavailable on this server: ${e.message}`, details };
    return { message: e.message, details };
  }
  return { message: e instanceof Error ? e.message : String(e), details: [] };
}

/** nameProblem words a break of the platform's runtime name rule (BuildRuntime). */
export function nameProblem(name: string): string | undefined {
  if (!name) return 'Name the runtime.';
  if (name === 'built-in') return '"built-in" names the built-in runtime.';
  if (name.length > 63 || !/^[a-z0-9][a-z0-9._-]*$/.test(name)) {
    return "Use lower-case letters, digits, '.', '_' and '-' (not first), at most 63 characters.";
  }
  return undefined;
}

/** defaultName suggests a runtime name for a release: 0.162.0 is v0162. */
export function defaultName(version: string | undefined): string {
  const m = /^v?0\.(\d+)\./.exec(version ?? '');
  return m ? `v0${m[1]}` : '';
}

/** finished reports whether a build ended. */
export function finished(state: string): boolean {
  return state === 'done' || state === 'failed' || state === 'cancelled';
}

export interface BuildLog {
  lines: string[];
  /** Lines before the first kept one: the server keeps the last 2000. */
  skipped: number;
}

/**
 * mergeLog adds a build page (lines from log_from on) to the log read so
 * far. Pages overlap or follow on; a page starting past what was read means
 * the server no longer keeps the lines between.
 */
export function mergeLog(prev: BuildLog, build: Pick<RuntimeBuild, 'log' | 'log_from'>): BuildLog {
  const page = build.log ?? [];
  const end = prev.skipped + prev.lines.length;
  if (build.log_from > end) return { lines: page, skipped: build.log_from };
  const keep = Math.max(0, build.log_from - prev.skipped);
  return { lines: [...prev.lines.slice(0, keep), ...page], skipped: prev.skipped };
}

/** nextLogFrom is the log_from that asks for the lines not read yet. */
export function nextLogFrom(log: BuildLog): number {
  return log.skipped + log.lines.length;
}

/** formatSize prints bytes in decimal units, as Docker does; negative is unknown. */
export function formatSize(n: number): string {
  if (n < 0) return 'unknown';
  if (n < 1000) return `${n} B`;
  const units = ['kB', 'MB', 'GB', 'TB', 'PB'];
  let v = n / 1000, i = 0;
  while (v >= 1000 && i < units.length - 1) { v /= 1000; i++; }
  return `${v.toFixed(1)} ${units[i]}`;
}

/** IMAGE_STATES words each runtime image state of the storage view. */
export const IMAGE_STATES: Record<string, { label: string; note: string }> = {
  named: { label: 'current', note: 'Its name selects it.' },
  replaced: { label: 'replaced', note: 'A rebuild took its name and nothing else tags it. Prune removes it.' },
  tagged: { label: 'tagged', note: 'A rebuild took its name, but a pushed or imported reference still names it. Prune keeps it.' },
  stale: { label: 'stale', note: 'Built for another worker protocol: this debugger cannot run it. Prune removes it when stale images are included.' },
};

/** PruneScope is what a prune removes: the shape `pruneQuestion` words. */
export interface PruneScope {
  images: number;
  bytes: number;
  containers: number;
  caches: number;
  empty: boolean;
}

/** prunedText words one removed or kept object of a prune result. */
export function prunedText(i: PrunedItem): string {
  const what = i.kind === 'runtime' ? `runtime ${i.name} (image ${shortImage(i.image_id ?? '')})` : `${i.kind} ${i.name}`;
  return i.reason ? `${what}: ${i.reason}` : what;
}

/** pruneSummary is the one-line result of a prune. */
export function pruneSummary(r: RuntimePruneResult): string {
  const removed = r.removed?.length ?? 0, kept = r.kept?.length ?? 0;
  if (removed === 0 && kept === 0) return 'Nothing to remove.';
  return `Removed ${removed} ${removed === 1 ? 'object' : 'objects'}, reclaiming ${formatSize(r.reclaimed)}${kept ? `; kept ${kept}` : ''}.`;
}

/** shortImage is an image ID as Docker prints it. */
export function shortImage(id: string): string {
  return id.replace(/^sha256:/, '').slice(0, 12);
}

/**
 * referenceProblem checks an image reference before a push or import: it
 * must be one word, and a push target must name a repository other than
 * the runtime's own local one.
 */
export function referenceProblem(ref: string, push = false): string | undefined {
  const r = ref.trim();
  if (!r) return push ? 'Name the reference to push to, such as registry.example.com/team/runtime:tag.' : 'Name the image reference to import.';
  if (/\s/.test(r)) return 'An image reference has no spaces.';
  if (push && r.startsWith(`${LOCAL_REPOSITORY}:`)) return 'Push to a registry reference, not the local name of a runtime.';
  return undefined;
}

/** LOCAL_REPOSITORY is the repository of a runtime's local name tag. */
export const LOCAL_REPOSITORY = 'linea-runtime';

/**
 * olderImage is true for a listed runtime image its name no longer selects:
 * another image of the same name carries the local name tag, while a pushed
 * or imported reference keeps this one (state `tagged`). Push and remove
 * act on a name, so they belong to the current image only.
 */
export function olderImage(r: Runtime, all: Runtime[]): boolean {
  // A catalog runtime has no image here: nothing replaces it.
  if (r.catalog) return false;
  const local = `${LOCAL_REPOSITORY}:${r.name}`;
  return !(r.tags ?? []).includes(local) && all.some((o) => o !== r && o.name === r.name && (o.tags ?? []).includes(local));
}

/** pruneQuestion asks before a prune, naming what it removes and its size. */
export function pruneQuestion(scope: PruneScope): string {
  const parts = [];
  if (scope.images) parts.push(`${scope.images} runtime ${scope.images === 1 ? 'image' : 'images'}`);
  if (scope.caches) parts.push(`${scope.caches} build ${scope.caches === 1 ? 'cache' : 'caches'}`);
  if (scope.containers) parts.push(`${scope.containers} stopped ${scope.containers === 1 ? 'container' : 'containers'}`);
  const list = parts.length > 1 ? `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}` : parts[0] ?? 'nothing';
  return `Remove ${list} (${formatSize(scope.bytes)})? Runs keep their evidence.`;
}

/** DELETE_RUNTIME_WARNING asks before a runtime image is removed. */
export const DELETE_RUNTIME_WARNING = 'Remove this runtime image? Runs that used it keep their evidence, but cannot run on it again.';

/** DELETE_CATALOG_WARNING says what removing a catalog runtime costs. */
export const DELETE_CATALOG_WARNING = 'Remove this runtime from the catalog? Its workers stay connected, but no request can select it until it is registered again.';

/** DEFAULT_REMOVE_REASON is why the default runtime cannot be removed. */
export const DEFAULT_REMOVE_REASON = 'This is the default runtime; make another runtime the default first.';

/** makeDefaultQuestion asks before the team's default changes. */
export function makeDefaultQuestion(name: string): string {
  return `Make ${name} the default runtime? Every run, validation and preview that names no runtime runs on it, for the whole team.`;
}

/** PRUNE_CACHES_WARNING says what removing the build caches costs. */
export const PRUNE_CACHES_WARNING =
  'The next build downloads its modules again (several GB), and an offline build fails until one has.';

/** componentKey is a catalog component as the manifest request names it. */
export const componentKey = (c: Pick<CatalogComponent, 'kind' | 'type'>) => `${c.kind}/${c.type}`;

export interface CatalogGroup {
  kind: string;
  components: CatalogComponent[];
}

const KIND_ORDER = ['receivers', 'processors', 'exporters', 'connectors', 'extensions'];

/**
 * catalogGroups groups a catalog by kind in pipeline order, filtered by type,
 * deprecated type or module; components the debugger cannot run stay listed
 * with their reason.
 */
export function catalogGroups(catalog: CollectorCatalog | undefined, filter: string): CatalogGroup[] {
  const f = filter.trim().toLowerCase();
  const byKind = new Map<string, CatalogComponent[]>();
  for (const c of catalog?.components ?? []) {
    const names = [c.type, c.deprecated_type ?? '', c.gomod];
    if (f && !names.some((n) => n.toLowerCase().includes(f))) continue;
    const list = byKind.get(c.kind) ?? [];
    list.push(c);
    byKind.set(c.kind, list);
  }
  const rank = (k: string) => (KIND_ORDER.indexOf(k) + KIND_ORDER.length + 1) % (KIND_ORDER.length + 1);
  return [...byKind.entries()]
    .sort(([a], [b]) => rank(a) - rank(b) || a.localeCompare(b))
    .map(([kind, components]) => ({ kind, components: components.sort((a, b) => a.type.localeCompare(b.type)) }));
}
