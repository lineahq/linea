import type { Runtime, RuntimeBuild, RuntimeDefault } from '../../api/types';
import type { IconName } from '../../ui/Icon';
import { BUILT_IN_NAME, formatSize, olderImage, providerLines } from '../runtimes';

/**
 * The view model of the Runtimes page: the list rows (built-in, catalog
 * runtimes served by connected workers and imported runtimes) and the
 * facts of the selected row. Pure, so it is tested without rendering.
 */

/** Tone is the chip colour of a state; it maps to an od-chip modifier. */
export type Tone = 'pass' | 'warn' | 'fail' | 'running' | 'pending';

export interface StateChip {
  label: string;
  tone: Tone;
  icon: IconName;
}

export type RowKind = 'built-in' | 'catalog' | 'container';

export interface RuntimeRow {
  /** Stable key: also the selection. */
  key: string;
  name: string;
  kind: RowKind;
  type: string;
  version: string;
  state: StateChip;
  /** Requests that name no runtime run on this one. */
  isDefault: boolean;
  runtime?: Runtime;
  /** On a team server, the workers that serve the built-in runtime. */
  workers?: NonNullable<Runtime['catalog']>;
}

export const BUILT_IN_KEY = 'built-in';

const VERIFIED: StateChip = { label: 'Verified', tone: 'pass', icon: 'circle-check' };
const UNVERIFIED_CHIP: StateChip = { label: 'Unverified', tone: 'warn', icon: 'triangle-alert' };

/**
 * catalogChip words a catalog runtime's workers: connected ones serve it;
 * one that said hello once confirmed it; else no worker of it ever came. A
 * binary a pool found wrong fails its runs whatever the workers.
 */
export function catalogChip(c: NonNullable<Runtime['catalog']>): StateChip {
  if (c.suspect) return { label: 'Suspect binary', tone: 'fail', icon: 'circle-x' };
  if (c.workers > 0) return { label: c.workers === 1 ? '1 worker' : `${c.workers} workers`, tone: 'pass', icon: 'circle-check' };
  if (c.confirmed_at) return { label: 'No worker', tone: 'warn', icon: 'triangle-alert' };
  return { label: 'Unconfirmed', tone: 'pending', icon: 'clock' };
}

/** buildChip words a build's state as a chip. */
export function buildChip(b: Pick<RuntimeBuild, 'state'>): StateChip {
  switch (b.state) {
    case 'queued': return { label: 'Queued', tone: 'pending', icon: 'clock' };
    case 'running': return { label: 'Building', tone: 'running', icon: 'loader-circle' };
    case 'done': return { label: 'Built', tone: 'pass', icon: 'circle-check' };
    case 'cancelled': return { label: 'Cancelled', tone: 'warn', icon: 'circle-minus' };
    default: return { label: 'Failed', tone: 'fail', icon: 'circle-x' };
  }
}

/**
 * runtimeRows lists the built-in runtime first, then catalog and imported
 * runtimes by name (an older image that only a pushed reference keeps is not
 * a row: its name selects a newer one). `defaultName` is the team's default
 * runtime; no row is the default while it is unknown, except a catalog
 * runtime the list itself marks. A team server lists the built-in runtime
 * too, served by its workers: that entry is the built-in row, not another.
 */
export function runtimeRows(runtimes: Runtime[] | undefined, builtInVersion: string | undefined, defaultName?: string): RuntimeRow[] {
  const all = (runtimes ?? []).filter((r) => r.name !== BUILT_IN_NAME);
  const served = runtimes?.find((r) => r.name === BUILT_IN_NAME);
  const rows: RuntimeRow[] = [{
    key: BUILT_IN_KEY, name: BUILT_IN_NAME, kind: 'built-in', type: 'Built-in', version: builtInVersion || served?.collector_version || '',
    state: served?.catalog ? catalogChip(served.catalog) : VERIFIED,
    isDefault: defaultName === BUILT_IN_NAME || served?.default === true, workers: served?.catalog,
  }];
  const current = all.filter((r) => !olderImage(r, all)).sort((a, b) => a.name.localeCompare(b.name));
  for (const r of current) {
    if (r.catalog) {
      rows.push({
        key: `catalog:${r.name}`, name: r.name, kind: 'catalog', type: r.catalog.binary ? 'Built runtime' : 'Worker image',
        version: r.collector_version, state: catalogChip(r.catalog), isDefault: r.default === true || r.name === defaultName, runtime: r,
      });
      continue;
    }
    rows.push({
      key: `runtime:${r.name}`, name: r.name, kind: 'container', type: 'Local container',
      version: r.collector_version, state: r.verified ? VERIFIED : UNVERIFIED_CHIP, isDefault: false, runtime: r,
    });
  }
  return rows;
}

/**
 * canBeDefault is true for a row the server accepts as the team's default
 * and that is not already: the built-in runtime and catalog runtimes, never
 * a local container.
 */
export function canBeDefault(row: RuntimeRow): boolean {
  return !row.isDefault && (row.kind === 'built-in' || row.kind === 'catalog');
}

/** selectedRow is the row with the key, else the first one (the built-in runtime). */
export function selectedRow(rows: RuntimeRow[], key: string | undefined): RuntimeRow {
  return rows.find((r) => r.key === key) ?? rows[0];
}

export interface RuntimeDetail {
  title: string;
  subtitle: string;
  facts: [string, string][];
  providers: string[];
  custom: string[];
}

/** runtimeDetail words the selected row for the detail pane. */
export function runtimeDetail(row: RuntimeRow, builtInVersion: string | undefined, team?: RuntimeDefault): RuntimeDetail {
  const detail = baseDetail(row, builtInVersion);
  if (row.isDefault) detail.facts.splice(1, 0, ['Default', defaultFact(team)]);
  return detail;
}

/** defaultFact words the Default fact: what it means and who chose it when; before the team chooses, the built-in runtime is the default. */
export function defaultFact(team: RuntimeDefault | undefined): string {
  const what = 'Requests that name no runtime run on it';
  if (!team?.set) return `${what} until the team chooses another`;
  return `${what} · chosen${team.set_by ? ` by ${team.set_by}` : ''} ${when(team.set)}`;
}

function baseDetail(row: RuntimeRow, builtInVersion: string | undefined): RuntimeDetail {
  if (row.kind === 'built-in') {
    return {
      title: `built-in${builtInVersion ? ` ${builtInVersion}` : ''}`,
      subtitle: 'ships with the debugger',
      facts: [
        ['Type', 'Built-in'], ['Verification', 'Verified: the release this debugger is built against'],
        ...(row.workers ? [['Workers', workersFact(row.workers)] as [string, string]] : []),
      ],
      providers: [], custom: [],
    };
  }
  const r = row.runtime!;
  if (row.kind === 'catalog') return catalogDetail(row, r);
  const facts: [string, string][] = [['Type', row.type]];
  facts.push(['Verification', r.verified ? 'Verified release' : 'Unverified release: newer than the releases this debugger was verified with']);
  facts.push(['Image', `${formatSize(r.size)} · ${r.image_id.replace(/^sha256:/, '').slice(0, 12)}`]);
  if (r.tags?.length) facts.push(['Tags', r.tags.join(', ')]);
  return {
    title: `${r.name} ${r.collector_version}`,
    subtitle: 'imported here · runs in Docker',
    facts,
    providers: providerLines(r.providers),
    custom: (r.custom_modules ?? []).map((m) => `${m.module} from ${m.source}${m.commit ? ` @ ${m.commit.slice(0, 12)}` : ''}`),
  };
}

function workersFact(c: NonNullable<Runtime['catalog']>): string {
  return c.workers ? `${c.workers} connected · ${c.slots} ${c.slots === 1 ? 'slot' : 'slots'}` : 'None connected: runs queue until one connects';
}

function catalogDetail(row: RuntimeRow, r: Runtime): RuntimeDetail {
  const c = r.catalog!;
  const facts: [string, string][] = [['Type', row.type]];
  facts.push(['Verification', r.verified ? 'Verified release' : 'Unverified release: newer than the releases this debugger was verified with']);
  if (c.binary) {
    facts.push(['Binary', `${formatSize(c.binary.bytes)} · ${c.binary.digest.replace(/^sha256:/, '').slice(0, 12)} · ${c.binary.os}/${c.binary.arch}`]);
  } else {
    facts.push(['Image', c.digest ? `${c.image} · ${c.digest.replace(/^sha256:/, '').slice(0, 12)}` : c.image]);
  }
  if (c.suspect) {
    facts.push(['Suspect', `${c.suspect.pool} found the binary wrong ${when(c.suspect.at)}: ${c.suspect.cause}. Runs fail until it is built or registered again.`]);
  }
  facts.push(['Identity', c.identity.replace(/^sha256:/, '').slice(0, 12)]);
  facts.push(['Registered', `${when(c.registered_at)} by ${c.registered_by || 'unknown'}`]);
  facts.push(['Confirmed', c.confirmed_at ? when(c.confirmed_at) : 'Not yet: no worker of this identity has connected']);
  facts.push(['Workers', workersFact(c)]);
  return {
    title: `${r.name} ${r.collector_version}`,
    subtitle: c.binary ? 'built on this server · runs on pool workers' : 'registered in the catalog · runs on connected workers',
    facts,
    providers: providerLines(r.providers),
    custom: [],
  };
}

function when(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString([], { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
}
