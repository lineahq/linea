// Routing decisions: a routing connector sends each record by the first rules
// of its table that match it (internal/debugger/routing). These helpers turn the
// report's explanation into what the Routing tab shows for one record: the
// rules in table order with what each did, and a sentence that says which
// rule routed it.
import type { Routing } from '../api/types';

export interface RuleStep {
  index: number;
  condition: string;
  action: 'move' | 'copy';
  pipelines: string[];
  /** not-reached: an earlier rule moved the record away before this rule saw it. */
  result: 'matched' | 'no-match' | 'not-reached';
}

export interface RuleTrail {
  steps: RuleStep[];
  matched: number[];
  /** The record went to the default pipelines. */
  byDefault: boolean;
  defaultPipelines: string[];
  sentence: string;
}

const list = (items: string[]) => (items.length < 2 ? items.join('') : `${items.slice(0, -1).join(', ')} and ${items[items.length - 1]}`);

/** explained reports whether the replay confirmed which rule routed each record. */
export const explained = (r?: Routing): r is Routing & { records: NonNullable<Routing['records']> } => r?.verification === 'replay_verified' && !!r.records;

/** trail follows one input record (by its position in the input) through the routing table. */
export function trail(r: Routing, ref: string): RuleTrail | undefined {
  const rec = r.records?.find((x) => x.ref === ref);
  if (!explained(r) || !rec) return undefined;
  const matched = rec.rules ?? [];
  let gone = false;
  const steps = (r.rules ?? []).map((rule): RuleStep => {
    const action = rule.action === 'copy' ? 'copy' : 'move';
    const hit = matched.includes(rule.index);
    const result = hit ? 'matched' : gone ? 'not-reached' : 'no-match';
    if (hit && action === 'move') gone = true;
    return { index: rule.index, condition: rule.condition, action, pipelines: rule.pipelines ?? [], result };
  });
  const defaults = r.default_pipelines ?? [];
  const byDefault = !!rec.default;
  const did = steps.filter((s) => s.result === 'matched').map((s) => `${s.action === 'copy' ? 'copied' : 'moved'} to ${list(s.pipelines)}`);
  const tail = byDefault ? (matched.length ? ', and the original continued to the default' : '') : '';
  const sentence = matched.length
    ? `Matched rule${matched.length > 1 ? 's' : ''} ${list(matched.map(String))} · ${list(did)}${tail}`
    : byDefault ? `No rule matched · sent to the default (${list(defaults)})` : 'No rule matched and there is no default pipeline';
  return { steps, matched, byDefault, defaultPipelines: defaults, sentence };
}
