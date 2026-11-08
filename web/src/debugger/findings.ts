// Key findings of a finished run, derived only from what its report records:
// exporters that received nothing, records a stage removed and fields a stage
// changed. Nothing here infers a cause. A finding says what was observed and
// where to look; "likely cause" explanations would need inference the
// platform does not provide, so they are not produced.
import type { Change, Report, Stage, TopologyComponent } from '../api/types';
import type { IconName } from '../ui/Icon';
import { stageNodeKey } from '../runs/annotations';

/** A piece of finding text: plain, or a component, path or value shown as code. */
export type Segment = string | { code: string };

export type FindingAction =
  | { kind: 'follow'; label: string; trace: string }
  | { kind: 'open'; label: string; node: string };

export interface Finding {
  id: string;
  icon: IconName;
  title: Segment[];
  detail?: Segment[];
  actions: FindingAction[];
}

/** The most findings listed; the rest stay reachable in Records and the graph. */
export const MAX_FINDINGS = 5;

const code = (s: string): Segment => ({ code: s });
const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;

/** stageDrops: records a stage removed (as the graph counts them) and records it received. */
export function stageDrops(stage: Stage): { removed: number; received: number } {
  let removed = 0;
  let received = 0;
  for (const i of stage.inputs ?? []) {
    removed += i.diff?.removed?.length ?? 0;
    received += i.diff?.before.items ?? 0;
  }
  return { removed, received };
}

/** The first record journey that ended as dropped at a component, to follow. */
function droppedAt(report: Report, component: string, pipeline?: string): string | undefined {
  return report.lineage?.find((l) =>
    l.stops?.some((s) => s.end === 'dropped' && s.component === component && (!pipeline || !s.pipeline || s.pipeline === pipeline)))?.id;
}

function exporterPipelines(report: Report, c: TopologyComponent): { id: string; signal: string }[] {
  return (report.topology?.pipelines ?? []).filter((p) => p.exporters?.includes(c.ref)).map((p) => ({ id: p.id, signal: p.signal }));
}

/** exporterItems: items the run captured at an exporter, across signals. */
export function exporterItems(report: Report, c: TopologyComponent): number {
  return (report.result?.outputs ?? [])
    .filter((o) => o.exporter === c.id || o.exporter === c.ref)
    .reduce((n, o) => n + o.counts.items, 0);
}

export interface Upstream {
  component: string;
  pipeline: string;
  node: string;
  removed: number;
  received: number;
}

/** upstreamDrops: processors ahead of an exporter, in its pipelines, that removed records. */
export function upstreamDrops(report: Report, c: TopologyComponent): Upstream[] {
  const pipelines = new Set(exporterPipelines(report, c).map((p) => p.id));
  const out: Upstream[] = [];
  for (const s of report.stages ?? []) {
    if (s.kind !== 'processor' || !pipelines.has(s.pipeline)) continue;
    const d = stageDrops(s);
    if (d.removed > 0) out.push({ component: s.component, pipeline: s.pipeline, node: stageNodeKey(s), ...d });
  }
  return out;
}

/** What a finished run shows about an exporter that captured nothing. */
export interface NothingReached {
  /** Processors that recorded removing records ahead of the exporter. */
  dropped: Upstream[];
  /** A record journey that ended as dropped at one of them, to follow. */
  trace?: string;
}

export function nothingReached(report: Report, c: TopologyComponent): NothingReached {
  const dropped = upstreamDrops(report, c);
  const trace = dropped.map((u) => droppedAt(report, u.component, u.pipeline)).find((t) => !!t);
  return { dropped, trace };
}

/** exporterZeroFinding: an exporter that received nothing although matching input was injected. */
function exporterZeroFindings(report: Report): Finding[] {
  const res = report.result;
  if (res?.status !== 'completed') return []; // After an early end, missing output proves nothing.
  const injected = (signal: string) => (res.inputs ?? []).filter((i) => i.injected && i.signal === signal).reduce((n, i) => n + i.counts.items, 0);
  const out: Finding[] = [];
  for (const c of report.topology?.components ?? []) {
    if (c.kind !== 'exporter') continue;
    const signals = exporterPipelines(report, c).map((p) => p.signal);
    if (!signals.some((s) => injected(s) > 0) || exporterItems(report, c) > 0) continue;
    const n = nothingReached(report, c);
    const actions: FindingAction[] = [];
    if (n.trace) actions.push({ kind: 'follow', label: 'Follow a dropped record', trace: n.trace });
    n.dropped.forEach((u) => actions.push({ kind: 'open', label: `Open ${u.component}`, node: u.node }));
    out.push({
      id: `zero:${c.ref}`,
      icon: 'ban',
      title: [code(c.id), ' received 0 records'],
      detail: n.dropped.length
        ? [...n.dropped.flatMap((u, i): Segment[] => [i ? ', ' : '', `${plural(u.removed, 'record was', 'records were')} dropped at `, code(u.component)]), '.']
        : ['No stage ahead of it recorded dropping records.'],
      actions,
    });
  }
  return out;
}

function dropFindings(report: Report, named: Set<string>): Finding[] {
  const out: Finding[] = [];
  for (const s of report.stages ?? []) {
    if (s.kind !== 'processor') continue;
    const d = stageDrops(s);
    if (d.removed === 0 || named.has(stageNodeKey(s))) continue;
    const trace = droppedAt(report, s.component, s.pipeline);
    out.push({
      id: `drop:${s.pipeline}|${s.component}`,
      icon: 'ban',
      title: [code(s.component), ` dropped ${plural(d.removed, 'record', 'records')}`],
      detail: [`Of ${d.received} it received in `, code(s.pipeline), '.'],
      actions: [
        ...(trace ? [{ kind: 'follow' as const, label: 'Follow a dropped record', trace }] : []),
        { kind: 'open' as const, label: `Open ${s.component}`, node: stageNodeKey(s) },
      ],
    });
  }
  return out;
}

type Level = 'resource' | 'scope' | 'record';

/** levelOf: where in the payload a change applies (entity names follow internal/debugger/analysis). */
export function levelOf(entity: string): Level {
  if (/^resource\[\d+\]$/.test(entity)) return 'resource';
  if (/\.scope\[\d+\]$/.test(entity)) return 'scope';
  return 'record';
}

const NOUN: Record<Level, [string, string]> = { resource: ['resource', 'resources'], scope: ['scope', 'scopes'], record: ['record', 'records'] };

interface Group {
  op: string;
  path: string;
  level: Level;
  entities: Set<string>;
  before: Set<string>;
  after: Set<string>;
}

/**
 * changeGroups: a stage's changes grouped by operation, path and level,
 * largest first. An entity is a position within its own input, so a change
 * names its input to count the same position in two inputs as two records.
 */
export function changeGroups(changes: (Change & { input?: number })[]): Group[] {
  const groups = new Map<string, Group>();
  for (const c of changes) {
    const level = levelOf(c.entity);
    const key = `${c.op}|${c.path}|${level}`;
    let g = groups.get(key);
    if (!g) groups.set(key, (g = { op: c.op, path: c.path, level, entities: new Set(), before: new Set(), after: new Set() }));
    g.entities.add(`${c.input ?? ''}|${c.entity}`);
    if (c.op !== 'added') g.before.add(JSON.stringify(c.before));
    if (c.op !== 'removed') g.after.add(JSON.stringify(c.after));
  }
  return [...groups.values()].sort((a, b) => b.entities.size - a.entities.size);
}

const VERB: Record<string, [string, string]> = { changed: [' changed ', ' on '], added: [' added ', ' to '], removed: [' removed ', ' from '] };

function changeFindings(report: Report): Finding[] {
  const out: Finding[] = [];
  for (const s of report.stages ?? []) {
    const changes = (s.inputs ?? []).flatMap((i) => (i.diff?.changes ?? []).map((c) => ({ ...c, input: i.seq })));
    const top = changeGroups(changes)[0];
    if (!top) continue;
    const [verb, prep] = VERB[top.op] ?? [` ${top.op} `, ' on '];
    const n = top.entities.size;
    const [one, many] = NOUN[top.level];
    const detail: Segment[] | undefined =
      top.op === 'changed' && top.before.size === 1 && top.after.size === 1
        ? [code([...top.before][0]), ' → ', code([...top.after][0]), '.']
        : top.op === 'added' && top.after.size === 1
          ? ['Value ', code([...top.after][0]), '.']
          : undefined;
    out.push({
      id: `change:${s.pipeline}|${s.component}`,
      icon: 'diff-changed',
      title: [code(s.component), verb, code(top.path), `${prep}${plural(n, one, many)}`],
      detail,
      actions: [{ kind: 'open', label: `Open ${s.component}`, node: stageNodeKey(s) }],
    });
  }
  return out;
}

/**
 * keyFindings lists what a finished run's report shows at a glance, most
 * important first: exporters that got nothing, then stages that removed
 * records, then fields that stages changed. Empty for a run without a report.
 */
export function keyFindings(report: Report | undefined): Finding[] {
  if (!report) return [];
  const zero = exporterZeroFindings(report);
  const named = new Set(zero.flatMap((f) => f.actions.filter((a) => a.kind === 'open').map((a) => a.node)));
  const drops = dropFindings(report, named);
  return [...zero, ...drops, ...changeFindings(report)].slice(0, MAX_FINDINGS);
}
