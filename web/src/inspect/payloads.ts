import type { Event } from '../api/types';
import { itemNoun } from './batching';

function requests(n: number): string {
  return `${n} request${n === 1 ? '' : 's'}`;
}

/** totalText sums the items of payloads per signal: "7 log records", "7 log records, 3 spans". */
export function totalText(list: Event[]): string {
  const bySignal = new Map<string, number>();
  for (const e of list) bySignal.set(e.signal ?? '', (bySignal.get(e.signal ?? '') ?? 0) + (e.counts?.items ?? 0));
  return [...bySignal].map(([signal, n]) => itemNoun(signal, n)).join(', ');
}

/**
 * payloadOption names one payload of a component in the picker: a request
 * by its position, never a bare event number, so a batch is not mistaken
 * for everything the component saw.
 */
export function payloadOption(list: Event[], i: number): string {
  const e = list[i];
  const parts = [`Request ${i + 1}`, e.counts ? itemNoun(e.signal ?? '', e.counts.items) : `${e.signal ?? ''} · ? items`];
  if (e.endpoint) parts.push(e.endpoint);
  if (e.pipeline) parts.push(e.pipeline);
  return parts.join(' · ');
}

/** payloadSummary says which request is shown and what all of them carried. */
export function payloadSummary(list: Event[], selected: Event): string {
  const one = selected.counts ? itemNoun(selected.signal ?? '', selected.counts.items) : `${selected.signal ?? ''} · ? items`;
  if (list.length <= 1) return one;
  const i = list.indexOf(selected);
  return `request ${i + 1} of ${list.length} (${one}); all ${requests(list.length)}: ${totalText(list)}`;
}

/** payloadGroups splits payloads by signal, keeping each one's index in the list, in order. */
export function payloadGroups(list: Event[]): { signal: string; indexes: number[] }[] {
  const groups = new Map<string, number[]>();
  list.forEach((e, i) => groups.set(e.signal ?? '', [...(groups.get(e.signal ?? '') ?? []), i]));
  return [...groups].map(([signal, indexes]) => ({ signal, indexes }));
}
