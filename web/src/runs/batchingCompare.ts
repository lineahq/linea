import type { Batching } from '../api/types';
import { batcherLabel, durationMs, fmtMs, itemNoun, TRIGGER_LABELS, triggerCounts, triggerOf, type Trigger } from '../inspect/batching';

/** BatchingRow is one setting or outcome of a batch processor in both runs. */
export interface BatchingRow {
  label: string;
  before: string;
  after: string;
  changed: boolean;
}

/** BatchingChange is one batch processor, by processor and pipeline, in the two runs. */
export interface BatchingChange {
  key: string;
  processor: string;
  pipeline?: string;
  signal: string;
  /** Set when the processor batched in one run only. */
  only?: 'before' | 'after';
  settings: BatchingRow[];
  outcome: BatchingRow[];
  /** The batches each run sent, in order, for a side-by-side strip. */
  batches: { before: BatchCell[]; after: BatchCell[] };
}

/** BatchCell is one batch sent: its size, why it left and, with metadata_keys, its batcher. */
export interface BatchCell {
  items: number;
  trigger: Trigger;
  batcher?: string;
}

const NONE = '—';

const row = (label: string, before: string, after: string): BatchingRow => ({ label, before, after, changed: before !== after });

function settingRows(a?: Batching, b?: Batching): BatchingRow[] {
  const s = (x: Batching | undefined, f: (b: Batching) => string) => (x ? f(x) : NONE);
  const max = (x: Batching) => (x.settings.send_batch_max_size ? String(x.settings.send_batch_max_size) : 'no limit');
  const keys = (x: Batching) => (x.settings.metadata_keys?.length ? x.settings.metadata_keys.join(', ') : 'none');
  const rows = [
    row('send_batch_size', s(a, (x) => String(x.settings.send_batch_size)), s(b, (x) => String(x.settings.send_batch_size))),
    row('timeout', s(a, (x) => x.settings.timeout), s(b, (x) => x.settings.timeout)),
    row('send_batch_max_size', s(a, max), s(b, max)),
    row('metadata_keys', s(a, keys), s(b, keys)),
  ];
  // The two duration strings may spell one value differently ("1s", "1000ms").
  if (a && b && durationMs(a.settings.timeout) === durationMs(b.settings.timeout)) rows[1].changed = false;
  if (a?.settings.metadata_keys?.length || b?.settings.metadata_keys?.length) {
    rows.push(row('metadata_cardinality_limit', s(a, (x) => String(x.settings.metadata_cardinality_limit)), s(b, (x) => String(x.settings.metadata_cardinality_limit))));
  }
  return rows;
}

function outcomeRows(a: Batching | undefined, b: Batching | undefined, signal: string): BatchingRow[] {
  const s = (x: Batching | undefined, f: (b: Batching) => string) => (x ? f(x) : NONE);
  const flushes = (x: Batching) => x.flushes ?? [];
  const items = (x: Batching) => flushes(x).reduce((n, f) => n + f.items, 0);
  const noun = itemNoun(signal, 2).replace(/^2 /, '');
  const sizes = (x: Batching) => {
    const n = flushes(x).map((f) => f.items);
    if (!n.length) return '0';
    const lo = Math.min(...n), hi = Math.max(...n);
    return lo === hi ? String(lo) : `${lo}–${hi}`;
  };
  const reasons = (x: Batching) => triggerCounts(flushes(x)).map(([t, n]) => `${n} ${TRIGGER_LABELS[t]}`).join(', ') || 'none';
  const wait = (x: Batching) => {
    const w = flushes(x).flatMap((f) => (f.wait_ms ? [f.wait_ms.max] : []));
    return w.length ? fmtMs(Math.max(...w)) : '0ms';
  };
  const split = (x: Batching) => String(flushes(x).filter((f) => f.split).length);
  const rows = [
    row('Requests in', s(a, (x) => String((x.arrivals ?? []).length)), s(b, (x) => String((x.arrivals ?? []).length))),
    row('Batches out', s(a, (x) => String(flushes(x).length)), s(b, (x) => String(flushes(x).length))),
    row(`${noun.charAt(0).toUpperCase()}${noun.slice(1)} sent`, s(a, (x) => String(items(x))), s(b, (x) => String(items(x)))),
    row(`${noun.charAt(0).toUpperCase()}${noun.slice(1)} per batch`, s(a, sizes), s(b, sizes)),
    row('Why batches left', s(a, reasons), s(b, reasons)),
    row('Longest added wait', s(a, wait), s(b, wait)),
  ];
  if ((a && flushes(a).some((f) => f.split)) || (b && flushes(b).some((f) => f.split))) rows.push(row('Split batches', s(a, split), s(b, split)));
  if (a?.batchers?.length || b?.batchers?.length) {
    const batchers = (x: Batching) => String(x.batchers?.length ? x.batchers.filter((y) => !y.refused).length : 1);
    rows.push(row('Batchers', s(a, batchers), s(b, batchers)));
  }
  return rows;
}

function cells(x?: Batching): BatchCell[] {
  if (!x) return [];
  return (x.flushes ?? []).map((f) => ({
    items: f.items,
    trigger: triggerOf(f),
    batcher: x.batchers?.length && f.batcher !== undefined ? batcherLabel(x, f.batcher) : undefined,
  }));
}

/**
 * batchingChanges pairs the batch processors of two runs by processor and
 * pipeline and puts each one's settings and outcome side by side, baseline
 * order first. Both come from each run's own recorded batching; nothing is
 * matched across the runs beyond the processor's name.
 */
export function batchingChanges(before: Batching[] | undefined, after: Batching[] | undefined): BatchingChange[] {
  const key = (b: Batching) => `${b.processor}\u0000${b.pipeline ?? ''}`;
  const keys = [...new Set([...(before ?? []).map(key), ...(after ?? []).map(key)])];
  return keys.map((k) => {
    const a = before?.find((b) => key(b) === k);
    const b = after?.find((x) => key(x) === k);
    const any = (a ?? b)!;
    return {
      key: k, processor: any.processor, pipeline: any.pipeline, signal: any.signal,
      only: !a ? 'after' : !b ? 'before' : undefined,
      settings: settingRows(a, b),
      outcome: outcomeRows(a, b, any.signal),
      batches: { before: cells(a), after: cells(b) },
    };
  });
}

/** batchingHeadline says in one line what changed: the settings, then the batches. */
export function batchingHeadline(c: BatchingChange): string {
  if (c.only) return c.only === 'before' ? 'Batches only in the baseline run.' : 'Batches only in the candidate run.';
  const settings = c.settings.filter((r) => r.changed).map((r) => `${r.label} ${r.before} → ${r.after}`);
  const batches = c.outcome.find((r) => r.label === 'Batches out')!;
  const out = batches.changed ? `${batches.before} → ${batches.after} batches` : `same ${batches.after} batches`;
  if (!settings.length) return c.outcome.some((r) => r.changed) ? `Same settings, different batches: ${out}.` : 'Same settings and the same batches.';
  return `${settings.join(', ')}: ${out}.`;
}
