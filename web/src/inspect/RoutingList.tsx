import { useMemo } from 'react';
import type { RecordLineage, Routing, StageInput } from '../api/types';
import { RecordList, type RecordFilter } from './RecordList';
import { Pill, RecordRow } from './RecordRow';
import { explained, trail, type RuleTrail } from './routing';

interface Item {
  id: string;
  lineage: RecordLineage;
  /** Sequence numbers of the deliveries that carried the record. */
  routed: number[];
  end?: string;
  /** Which rules routed it, when the replay confirmed them. */
  rules?: RuleTrail;
}
const filters: RecordFilter<Item>[] = [
  { id: 'all', label: 'All', match: () => true },
  { id: 'routed', label: 'Routed', match: (i) => i.routed.length > 0 },
  { id: 'copied', label: 'Copied', match: (i) => i.routed.length > 1 },
  { id: 'default', label: 'Default', match: (i) => !!i.rules?.byDefault },
  { id: 'none', label: 'Not routed', match: (i) => i.routed.length === 0 },
];

/** names lists "a", "a and b" or "a, b and c". */
const names = (list: string[]) => (list.length < 2 ? list.join('') : `${list.slice(0, -1).join(', ')} and ${list[list.length - 1]}`);

const RESULT: Record<string, string> = { matched: 'matched', 'no-match': 'no match', 'not-reached': 'not reached' };

/**
 * RoutingList shows where a routing connector sent each record of one input
 * in the shared record list: a row per record whose journey passes the input.
 * When the replay confirmed the connector's decisions the row says which rule
 * routed the record and it opens to the rules in table order; otherwise it
 * states only which pipelines received it.
 */
export function RoutingList({ input, lineage, routing, follow, openRoute }: {
  input: StageInput;
  lineage?: RecordLineage[];
  routing?: Routing;
  follow(lineage: string): void;
  openRoute(seq: number, pipeline: string): void;
}) {
  const routes = input.routes ?? [];
  const items = useMemo<Item[]>(() => (lineage ?? []).flatMap((l) => {
    const hops = l.hops ?? [];
    const at = hops.findIndex((h) => h.seq === input.seq);
    if (at < 0) return [];
    return [{
      id: l.id, lineage: l, routed: hops.filter((h) => h.parent === at).map((h) => h.seq), end: hops[at].end,
      rules: routing && explained(routing) && hops[at].ref ? trail(routing, hops[at].ref!) : undefined,
    }];
  }), [lineage, input.seq, routing]);
  const pipelines = (seqs: number[]) => seqs.map((s) => ({ seq: s, name: routes.find((r) => r.seq === s)?.pipeline ?? `delivery ${s}` }));
  const shown = items.some((i) => i.rules) ? filters : filters.filter((f) => f.id !== 'default');
  return <RecordList
    items={items}
    itemId={(i) => i.id}
    filters={shown}
    label="Records and where they were routed"
    hint="see where it went"
    pageNoun="records"
    defaultOpen
    empty={items.length ? undefined : 'No record journeys pass this input.'}
    renderRow={(i, s) => {
      const to = pipelines(i.routed);
      const why = i.rules ? i.rules.sentence
        : to.length > 1 ? `Copied to ${names(to.map((p) => p.name))}` : to.length ? `Delivered to ${to[0].name}` : i.end === 'not_routed' ? 'Not routed to any pipeline' : 'No delivery was recorded';
      return <RecordRow {...s} onToggle={s.toggle} glyph={to.length ? '→' : '✕'} glyphTone={to.length ? 'route' : 'drop'} title={i.lineage.name || i.lineage.id} why={why}
        pills={i.rules
          ? <>{i.rules.matched.map((m) => <Pill key={m} tone="route">rule {m}</Pill>)}{i.rules.byDefault && <Pill>default</Pill>}{!i.rules.matched.length && !i.rules.byDefault && <Pill tone="drop">not routed</Pill>}</>
          : to.length ? to.map((p) => <Pill key={p.seq} tone="route">{p.name}</Pill>) : <Pill tone="drop">not routed</Pill>} />;
    }}
    renderDetail={(i) => {
      const to = pipelines(i.routed);
      return <>
        {i.rules && <div className="rv-group">
          <div className="rv-group-head"><b>Routing table</b><span>rules are checked in order; a moved record skips the rules after it</span></div>
          <ol className="rv-ladder" aria-label="Routing rules">
            {i.rules.steps.map((s) => <li key={s.index} className={`rv-step rv-step-${s.result === 'matched' ? 'match' : 'no-match'}${s.result === 'matched' ? ' rv-decisive' : ''}`}>
              <span className="rv-step-name">Rule {s.index}<span className="rv-type">{s.action}</span></span>
              <span className="rv-step-result">{RESULT[s.result]}</span>
              <span className="rv-step-evidence"><code>{s.condition}</code><span className="rv-step-rule">{s.action === 'copy' ? 'copies to' : 'moves to'} {names(s.pipelines)}</span></span>
            </li>)}
            <li className={`rv-step rv-step-${i.rules.byDefault ? 'match' : 'no-match'}${i.rules.byDefault ? ' rv-decisive' : ''}`}>
              <span className="rv-step-name">Default</span>
              <span className="rv-step-result">{i.rules.byDefault ? 'received it' : 'did not receive it'}</span>
              <span className="rv-step-evidence"><span className="rv-step-rule">{i.rules.defaultPipelines.length ? names(i.rules.defaultPipelines) : 'no default pipeline configured'}</span></span>
            </li>
          </ol>
        </div>}
        {to.length > 0
          ? <div className="rv-group">
            <div className="rv-group-head"><b>Delivered into</b>{to.length > 1 && <span>each pipeline received its own copy</span>}</div>
            <div className="journey-sources">{to.map((p) => <button key={p.seq} type="button" className="link chip" title="Inspect what this pipeline did with it" onClick={() => openRoute(p.seq, p.name)}>{p.name}</button>)}</div>
          </div>
          : <p className="rv-foot">The connector did not deliver this record into any pipeline.</p>}
        <div><button type="button" className="rv-follow" onClick={() => follow(i.lineage.id)} aria-label={`Follow record: ${i.lineage.name || i.lineage.id}`}>Follow record <span aria-hidden="true">→</span></button></div>
      </>;
    }} />;
}
