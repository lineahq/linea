// Reading the report's filtering: what a filter processor did to each
// request. The server evaluates the configured conditions on the recorded
// input and attributes drops only when that reproduces the processor's
// output (internal/debugger/filterreplay, docs/internals/filtering.md); this
// module only names and summarizes it.
import type { FilterCondition, FilterDrop, Filtering } from '../api/types';
import { itemNoun } from './batching';

/** filteringFor lists the explained inputs of one filter processor in one pipeline, in input order. */
export function filteringFor(filtering: Filtering[] | undefined, component: string, pipeline?: string): Filtering[] {
  return (filtering ?? []).filter((f) => f.processor === component && f.pipeline === pipeline).sort((a, b) => a.input - b.input);
}

const REF_NAMES: Record<string, string> = {
  resource: 'resource', scope: 'scope', log_record: 'log record', span: 'span', event: 'event', metric: 'metric', data_point: 'data point',
};

/** refLabel names an input position, 1-based: "resource[0].scope[0].log_record[2]" is "resource 1 › scope 1 › log record 3". */
export function refLabel(ref: string): string {
  return ref.split('.').map((part) => {
    const m = /^([a-z_]+)\[(\d+)\]$/.exec(part);
    return m ? `${REF_NAMES[m[1]] ?? m[1]} ${Number(m[2]) + 1}` : part;
  }).join(' › ');
}

/** dropsBy groups the drops by the index of the condition that made them. */
export function dropsBy(f: Filtering): Map<number, FilterDrop[]> {
  const out = new Map<number, FilterDrop[]>();
  for (const d of f.drops ?? []) out.set(d.condition, [...(out.get(d.condition) ?? []), d]);
  return out;
}

/** unused lists the conditions that were evaluated and never matched: they removed nothing in this request. */
export function unused(f: Filtering): FilterCondition[] {
  if (f.verification !== 'replay_verified') return [];
  return (f.conditions ?? []).filter((c) => c.evaluated > 0 && c.matched === 0 && c.errors === 0);
}

/** filterSummary is the one sentence above a request's view. */
export function filterSummary(f: Filtering): string {
  const kept = f.records - f.dropped;
  const parts = [`${itemNoun(f.signal, f.records)} in, ${f.dropped} dropped, ${kept} passed on.`];
  if (f.error) parts.push('A condition failed under error_mode propagate, so the processor returned an error and the whole request was lost.');
  switch (f.verification) {
    case 'replay_verified':
      parts.push(f.drops?.length ? 'Evaluating the conditions on the recorded input reproduces the output, so each drop is named with its condition.' : 'Evaluating the conditions on the recorded input reproduces the output.');
      break;
    case 'replay_differs':
      parts.push('Evaluating the conditions did not reproduce what the processor emitted, so no drop is attributed to a condition.');
      break;
    default:
      parts.push('This processor filters with settings the debugger does not evaluate, so no drop is attributed to a condition.');
  }
  return parts.join(' ');
}

/** ConditionTotal is one condition's counts summed over a processor's requests. */
export interface ConditionTotal {
  index: number;
  condition: string;
  source: string;
  evaluated: number;
  matched: number;
  records: number;
  errors: number;
}

/** RunTotals sums a processor's requests; condition counts only from verified requests. */
export interface RunTotals {
  requests: number;
  verified: number;
  records: number;
  dropped: number;
  conditions: ConditionTotal[];
}

/** runTotals sums every request of one filter processor in the run. */
export function runTotals(list: Filtering[]): RunTotals {
  const out: RunTotals = { requests: list.length, verified: 0, records: 0, dropped: 0, conditions: [] };
  const byIndex = new Map<number, ConditionTotal>();
  for (const f of list) {
    out.records += f.records;
    out.dropped += f.dropped;
    for (const c of f.conditions ?? []) {
      if (!byIndex.has(c.index)) byIndex.set(c.index, { index: c.index, condition: c.condition, source: c.source, evaluated: 0, matched: 0, records: 0, errors: 0 });
    }
    if (f.verification !== 'replay_verified') continue;
    out.verified++;
    for (const c of f.conditions ?? []) {
      const t = byIndex.get(c.index)!;
      t.evaluated += c.evaluated;
      t.matched += c.matched;
      t.records += c.records;
      t.errors += c.errors;
    }
  }
  out.conditions = [...byIndex.values()].sort((a, b) => a.index - b.index);
  return out;
}

/** totalsSummary is the one sentence above the whole-run table. */
export function totalsSummary(t: RunTotals, signal: string): string {
  const parts = [`${t.requests} requests, ${itemNoun(signal, t.records)} in, ${t.dropped} dropped.`];
  if (t.verified < t.requests) parts.push(`Condition counts cover the ${t.verified} request${t.verified === 1 ? '' : 's'} whose evaluation reproduces the output.`);
  return parts.join(' ');
}
