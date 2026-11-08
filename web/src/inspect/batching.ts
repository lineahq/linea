import type { Batching } from '../api/types';

type Flush = NonNullable<Batching['flushes']>[number];
type Arrival = NonNullable<Batching['arrivals']>[number];
type Batcher = NonNullable<Batching['batchers']>[number];

/** Trigger is why the batching processor sent a batch. */
export type Trigger = 'size' | 'timeout' | 'shutdown' | 'immediate' | 'evicted';

const TRIGGERS = ['size', 'timeout', 'shutdown', 'immediate', 'evicted'] as const;

type Settings = Batching['settings'];

/** SettingNames name the settings as the processor's own configuration does. */
export interface SettingNames {
  processor: string;
  size: string;
  maxSize: string;
  timeout: string;
  keys: string;
  /** The bound on batchers: metadata_cardinality_limit, or queue_batch's cache_size. */
  limit: string;
  /** What sizes count: " bytes" for a bytes sizer, else "". */
  unit: string;
}

/** settingNames names the batch processor's settings, or queue_batch's batch section. */
export function settingNames(s: Settings): SettingNames {
  const unit = s.sizer === 'bytes' ? ' bytes' : '';
  if (s.type === 'queue_batch') {
    return { processor: 'queue_batch processor', size: 'batch::min_size', maxSize: 'batch::max_size', timeout: 'batch::flush_timeout',
      keys: 'batch::partition::metadata_keys', limit: 'batch::partition::cache_size', unit };
  }
  return { processor: 'batch processor', size: 'send_batch_size', maxSize: 'send_batch_max_size', timeout: 'timeout',
    keys: 'metadata_keys', limit: 'metadata_cardinality_limit', unit };
}

/** batchingFor picks the batching explanation of one processor instance. */
export function batchingFor(batching: Batching[] | undefined, component: string, pipeline?: string): Batching | undefined {
  return batching?.find((b) => b.processor === component && b.pipeline === pipeline);
}

const NOUNS: Record<string, [string, string]> = {
  traces: ['span', 'spans'],
  logs: ['log record', 'log records'],
  metrics: ['data point', 'data points'],
  profiles: ['sample', 'samples'],
};

/** itemNoun names what the batch processor counts for a signal: "1 log record", "3 spans". */
export function itemNoun(signal: string, n: number): string {
  const [one, many] = NOUNS[signal] ?? ['item', 'items'];
  return `${n} ${n === 1 ? one : many}`;
}

/** TRIGGER_LABELS name each reason to send in a few words. */
export const TRIGGER_LABELS: Record<Trigger, string> = {
  size: 'size reached',
  timeout: 'timer',
  shutdown: 'shutdown',
  immediate: 'right away',
  evicted: 'partition evicted',
};

export function triggerOf(f: Flush): Trigger {
  return TRIGGERS.find((t) => t === f.trigger) ?? 'timeout';
}

/** verificationText says whether the processor's own metrics confirm the reasons shown. */
export function verificationText(b: Batching): { tone: 'pass' | 'warn' | 'neutral'; text: string } {
  const p = settingNames(b.settings).processor;
  // queue_batch's metrics count the batches and their items, not the reasons.
  const what = b.settings.type === 'queue_batch' ? ['the same batches', 'the batches'] : ['the same reasons to send', 'the reasons'];
  switch (b.verification) {
    case 'metrics_agree':
      return { tone: 'pass', text: `The ${p}’s own metrics count ${what[0]}.` };
    case 'metrics_differ':
      return { tone: 'warn', text: `The ${p}’s own metrics count ${what[1]} differently; the reasons shown are worked out from timing and may be wrong.` };
    default:
      return { tone: 'neutral', text: 'The reasons are worked out from timing; the run recorded none of the processor’s own metrics to check them.' };
  }
}

/** durationMs parses a Go duration string ("300ms", "1.5s", "1m0s"). */
export function durationMs(d: string): number {
  const units: Record<string, number> = { ns: 1e-6, us: 1e-3, 'µs': 1e-3, ms: 1, s: 1000, m: 60_000, h: 3_600_000 };
  let total = 0;
  let matched = false;
  for (const m of d.matchAll(/([\d.]+)(ns|us|µs|ms|s|m|h)/g)) {
    total += Number(m[1]) * units[m[2]];
    matched = true;
  }
  return matched ? total : 0;
}

export function fmtMs(ms: number): string {
  if (ms >= 1000) return `${(ms / 1000).toFixed(ms % 1000 === 0 ? 0 : 2)}s`;
  return `${Number.isInteger(ms) ? ms : ms.toFixed(1)}ms`;
}

function round1(n: number): string {
  return Number.isInteger(n) ? String(n) : n.toFixed(1);
}

/** settingsText lists the settings that decide when a batch leaves. */
export function settingsText(b: Batching): string {
  const s = b.settings;
  const n = settingNames(s);
  if (s.unbatched) return 'no batch section: every request is queued and sent as it is';
  const parts = [`${n.size} ${s.send_batch_size}${n.unit}`, `${n.timeout} ${s.timeout}`];
  if (s.send_batch_max_size) parts.push(`${n.maxSize} ${s.send_batch_max_size}${n.unit}`);
  if (s.metadata_keys?.length) parts.push(`${n.keys} [${s.metadata_keys.join(', ')}]`);
  return parts.join(' · ');
}

/** ImpactRow compares what reached the processor with what it sent on. */
export interface ImpactRow {
  label: string;
  before: string;
  after: string;
  /** What the change means, when it needs saying. */
  note?: string;
}

/** impactRows is what batching changed, from the requests in to the batches out. */
export function impactRows(b: Batching): ImpactRow[] {
  const arrivals = b.arrivals ?? [];
  const flushes = b.flushes ?? [];
  const items = (xs: { items: number }[]) => xs.reduce((n, x) => n + x.items, 0);
  const blocks = (xs: { resources: number }[]) => xs.reduce((n, x) => n + x.resources, 0);
  const per = (n: number, d: number) => (d ? round1(n / d) : '0');
  const noun = itemNoun(b.signal, 2).replace(/^2 /, '');
  const n = settingNames(b.settings);
  const rows: ImpactRow[] = [
    { label: 'Requests', before: String(arrivals.length), after: String(flushes.length),
      note: flushes.length < arrivals.length ? 'fewer, larger requests to the exporters' : flushes.length > arrivals.length ? `more requests: ${n.maxSize} splits large ones` : undefined },
    { label: `${noun.charAt(0).toUpperCase()}${noun.slice(1)} per request`, before: per(items(arrivals), arrivals.length), after: per(items(flushes), flushes.length) },
  ];
  const inBlocks = blocks(arrivals);
  const outBlocks = blocks(flushes);
  const distinct = flushes.reduce((n, f) => n + f.distinct_resources, 0);
  rows.push({ label: 'Resource blocks', before: String(inBlocks), after: String(outBlocks),
    note: distinct < outBlocks ? 'not merged: batching appends requests, so equal resources stay separate blocks' : undefined });
  const waits = flushes.flatMap((f) => (f.wait_ms ? [f.wait_ms.max] : []));
  if (waits.length) rows.push({ label: 'Added wait', before: '0', after: `up to ${fmtMs(Math.max(...waits))}` });
  if (b.batchers?.length) {
    rows.push({ label: 'Batchers', before: '1', after: String(b.batchers.filter((x) => !x.refused).length),
      note: `one per ${b.settings.metadata_keys?.join(' + ')} value: a batch never mixes them` });
  }
  return rows;
}

/** triggerCounts counts the batches per reason to send, in a fixed order. */
export function triggerCounts(flushes: Flush[]): [Trigger, number][] {
  const counts = new Map<Trigger, number>();
  for (const f of flushes) counts.set(triggerOf(f), (counts.get(triggerOf(f)) ?? 0) + 1);
  return TRIGGERS.flatMap((t) => (counts.get(t) ? [[t, counts.get(t)!] as [Trigger, number]] : []));
}

/** summaryText is the one sentence above the tab. */
export function summaryText(b: Batching): string {
  const arrivals = b.arrivals ?? [];
  const flushes = b.flushes ?? [];
  const total = flushes.reduce((n, f) => n + f.items, 0);
  const reasons = triggerCounts(flushes).map(([t, n]) => `${n} ${TRIGGER_LABELS[t]}`).join(', ');
  const req = (n: number) => `${n} request${n === 1 ? '' : 's'}`;
  const out = `${flushes.length} batch${flushes.length === 1 ? '' : 'es'}`;
  return `${req(arrivals.length)} in, ${out} out (${itemNoun(b.signal, total)})${reasons ? `: ${reasons}` : ''}.`;
}

/** batcherLabel names a batcher by its metadata values. */
export function batcherLabel(b: Batching, i: number): string {
  const x = b.batchers?.[i];
  if (!x) return '';
  if (x.unknown) return 'unknown metadata';
  const keys = b.settings.metadata_keys?.map((k) => k.toLowerCase()) ?? Object.keys(x.metadata ?? {});
  return [...new Set(keys)].map((k) => {
    const v = x.metadata?.[k] ?? [];
    return v.length ? `${k}=${v.join(',')}` : `no ${k}`;
  }).join(' · ');
}

/** BatcherRow is one batcher of a processor with metadata_keys. */
export interface BatcherRow {
  index: number;
  label: string;
  requests: number;
  items: number;
  batches: number;
  reasons: [Trigger, number][];
  refused: boolean;
  /** evicted: queue_batch flushed and removed the partition to stay within its cache_size. */
  evicted: boolean;
  unknown: boolean;
  /** why says why its batches were sent, or that it was refused. */
  why: string;
}

export function batcherRows(b: Batching): BatcherRow[] {
  const flushes = b.flushes ?? [];
  const n = settingNames(b.settings);
  return (b.batchers ?? []).map((x: Batcher, index) => {
    const reasons = triggerCounts(flushes.filter((f) => f.batcher === index));
    let why = reasons.map(([t, c]) => `${c} ${TRIGGER_LABELS[t]}`).join(', ') || 'nothing sent';
    if (x.refused) why = `refused: over ${n.limit}`;
    else if (x.evicted) why += `; removed beyond ${n.limit}`;
    return {
      index, label: batcherLabel(b, index), requests: x.arrivals, items: x.items, batches: x.flushes,
      reasons, refused: !!x.refused, evicted: !!x.evicted, unknown: !!x.unknown, why,
    };
  });
}

/** BatchRow is one batch the processor sent. */
export interface BatchRow {
  seq: number;
  /** Batch number, from 1 in send order. */
  n: number;
  items: number;
  trigger: Trigger;
  /** Why it left, as one sentence. */
  why: string;
  batcher?: string;
  /** What it carries, by request: request number, service names and records. */
  contents: { request: number; source: string; items: number }[];
  blocks: number;
  distinct: number;
  waited?: string;
  split: boolean;
}

function sourceOf(a: Arrival | undefined, n: number): string {
  const names = [...new Set((a?.services ?? []).filter(Boolean))];
  return names.length ? names.join(', ') : `request ${n}`;
}

export function batchRows(b: Batching): BatchRow[] {
  const arrivals = b.arrivals ?? [];
  const number = new Map(arrivals.map((a, i) => [a.seq, i + 1]));
  const bySeq = new Map(arrivals.map((a) => [a.seq, a]));
  const s = b.settings;
  const names = settingNames(s);
  const noun = (n: number) => itemNoun(b.signal, n);
  // pending counts items; with a bytes sizer the sizes are bytes.
  const size = `${s.send_batch_size}${names.unit}`;
  return (b.flushes ?? []).map((f, i) => {
    const trigger = triggerOf(f);
    let why: string;
    switch (trigger) {
      case 'size':
        why = f.split
          ? `${noun(f.pending)} were waiting; ${names.maxSize} ${s.send_batch_max_size}${names.unit} cut this batch and ${f.left} kept waiting`
          : `${noun(f.pending)} were waiting, at least ${names.size} ${size}`;
        break;
      case 'timeout':
        why = `The ${s.timeout} timer fired with ${noun(f.pending)} waiting, short of ${size}`;
        break;
      case 'shutdown':
        why = `The Collector shut down with ${noun(f.pending)} waiting; they are sent, not lost`;
        break;
      case 'evicted':
        why = `${noun(f.pending)} were waiting when a new partition went past ${names.limit} ${s.cache_size}; the least recently used partition is sent and removed`;
        break;
      default:
        why = f.split ? `Sent on arrival, cut at ${names.maxSize} ${s.send_batch_max_size}${names.unit}`
          : s.unbatched ? 'Sent as it arrived: without a batch section queue_batch does not batch' : `Sent on arrival: ${names.size} 0 does not wait`;
    }
    let waited: string | undefined;
    if (f.wait_ms) {
      waited = f.wait_ms.max === 0 ? 'no wait'
        : f.wait_ms.min === f.wait_ms.max ? `waited ${fmtMs(f.wait_ms.max)}` : `waited ${fmtMs(f.wait_ms.min)}–${fmtMs(f.wait_ms.max)}`;
    }
    return {
      seq: f.seq, n: i + 1, items: f.items, trigger, why, split: !!f.split, waited,
      batcher: b.batchers?.length ? batcherLabel(b, f.batcher) : undefined,
      contents: (f.sources ?? []).map((src) => {
        const n = number.get(src.arrival) ?? 0;
        return { request: n, source: sourceOf(bySeq.get(src.arrival), n), items: src.items };
      }),
      blocks: f.resources, distinct: f.distinct_resources,
    };
  });
}

/** timerNote explains a timer batch whose records waited less than the timeout. */
export function timerNote(b: Batching, r: BatchRow): string | undefined {
  const f = b.flushes?.find((x) => x.seq === r.seq);
  const timeout = durationMs(b.settings.timeout);
  if (r.trigger !== 'timeout' || !f?.wait_ms || !timeout || f.wait_ms.max >= timeout * 0.9) return undefined;
  if (b.settings.type === 'queue_batch') {
    return `The records waited less than ${b.settings.timeout}: the timer restarts after every batch and when a request reaches an empty batcher, not per record.`;
  }
  return `The records waited less than ${b.settings.timeout}: the timer runs in rounds that restart after every batch, not per record.`;
}

/** metadataNote says how metadata_keys splits the requests, or that it does not. */
export function metadataNote(b: Batching): string {
  const keys = b.settings.metadata_keys ?? [];
  const n = settingNames(b.settings);
  if (b.settings.unbatched) return 'Without a batch section queue_batch sends every request on its own, with its own request context.';
  if (!keys.length) {
    return `Every request shares one batcher. With ${n.keys} the processor keeps one batcher per value of the listed request headers, each with its own size count and timer; the receiver needs include_metadata: true.`;
  }
  const limit = b.settings.type === 'queue_batch' && b.settings.cache_size
    ? ` Beyond ${n.limit} ${b.settings.cache_size} partitions the least recently used one is sent and removed.` : '';
  return `${n.keys} [${keys.join(', ')}]: one batcher per value of ${keys.length === 1 ? 'this request header' : 'these request headers'}, each with its own ${n.size} count and timer. Requests without the header share one batcher. Without ${n.keys} every request would share one.${limit}`;
}
