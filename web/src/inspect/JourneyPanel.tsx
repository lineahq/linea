import { useMemo, useState, type ReactNode } from 'react';
import type { Event, RecordLineage } from '../api/types';
import type { WorkspaceSearch } from '../router';
import { eventStep } from './navigation';
import { END_LABELS, confidenceLabel, endMark, hopEntity, hopTree, routeMatrix, type Hop, type HopNode } from './journey';
import { display } from './grouped';

interface Props {
  lineage?: RecordLineage[];
  events: Event[];
  live: boolean;
  search: WorkspaceSearch;
  setSearch(patch: Partial<WorkspaceSearch>): void;
}

/**
 * JourneyPanel answers where telemetry went: without a selection it lists
 * every injected record against the exporters it reached (or where it
 * stopped); with one (?trace=) it follows that record hop by hop, branching
 * where a connector or a receiver's fan-out copied it, with what each stage
 * changed. A record an aggregating connector counted links to the data
 * points it went into, which have journeys of their own back to their
 * sources. The graph highlights the selected journey.
 */
export function JourneyPanel(p: Props) {
  if (p.live) return <div className="placeholder">Journeys are available when the run finishes.</div>;
  if (!p.lineage?.length) return <div className="placeholder">This run has no records to follow.</div>;
  const selected = p.search.trace ? p.lineage.find((l) => l.id === p.search.trace) : undefined;
  return selected ? <Journey {...p} l={selected} /> : <RoutingMatrix {...p} />;
}

const CELL: Record<string, [string, string]> = {
  delivered: ['✓', 'delivered unchanged'],
  changed: ['✓~', 'delivered, changed on the way'],
};

function RoutingMatrix({ lineage, setSearch }: Props) {
  const m = useMemo(() => routeMatrix(lineage), [lineage]);
  const [query, setQuery] = useState('');
  const [page, setPage] = useState(0);
  const rows = useMemo(() => m.rows.filter((r) => `${r.name} ${r.id} ${r.signal} ${r.receiver} ${r.stops.map((s) => `${s.end} ${s.component}`).join(' ')}`.toLowerCase().includes(query.trim().toLowerCase())), [m, query]);
  const pages = Math.max(1, Math.ceil(rows.length / 25));
  const current = Math.min(page, pages - 1);
  return (
    <div className="details journeys">
      <div className="journey-filter"><input type="search" aria-label="Search record journeys" placeholder="Search records, sources or stops…" value={query} onChange={(e) => { setQuery(e.target.value); setPage(0); }} />
        <span className="muted">{rows.length} of {m.rows.length} records</span></div>
      <p className="muted">
        Where each injected record went. ✓ delivered unchanged, ✓~ delivered after changes; a record copied by a connector
        or a fan-out reaches several exporters. Select a record to follow it.
      </p>
      <table className="grid matrix">
        <thead>
          <tr>
            <th>Record</th>
            {m.exporters.map((x) => (
              <th key={x} className="mono">
                {x}
              </th>
            ))}
            <th>Other ends</th>
          </tr>
        </thead>
        <tbody>
          {rows.slice(current * 25, (current + 1) * 25).map((r) => (
            <tr key={r.id} className="clickable" onClick={() => setSearch({ trace: r.id })}>
              <td>
                <button
                  type="button"
                  className="link"
                  title={`${r.signal} ${r.origin === 'aggregated' ? 'data point counted by' : 'from'} ${r.receiver} · ${r.id}`}
                >
                  {r.origin === 'aggregated' && <span className="hop-kind kind-connector">Σ</span>} {r.name || r.id}
                </button>
              </td>
              {m.exporters.map((x) => {
                const c = r.cells[x];
                return (
                  <td key={x} className={c ? `cell-${c}` : 'cell-none'} title={c ? CELL[c][1] : 'not delivered here'}>
                    {c ? CELL[c][0] : '·'}
                  </td>
                );
              })}
              <td>
                {r.stops.map((s, i) => (
                  <span key={i} className={`end end-${s.end}`}>
                    {END_LABELS[s.end] ?? s.end}
                    {s.component && ` at ${s.component}`}
                  </span>
                ))}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {!rows.length && <p className="muted">No record journeys match this search.</p>}
      {pages > 1 && <div className="signal-pagination"><button type="button" disabled={current === 0} onClick={() => setPage(current - 1)}>Previous records</button>
        <span>Page {current + 1} of {pages}</span><button type="button" disabled={current + 1 >= pages} onClick={() => setPage(current + 1)}>Next records</button></div>}
    </div>
  );
}

/** Links name other journeys (sources and data points) and open them. */
interface Links {
  name(id: string): string;
  follow(id: string): void;
}

function Journey({ l, lineage, events, setSearch }: Props & { l: RecordLineage }) {
  const roots = useMemo(() => hopTree(l), [l]);
  const names = useMemo(() => new Map((lineage ?? []).map((x) => [x.id, x.name || x.id])), [lineage]);
  const links: Links = { name: (id) => names.get(id) ?? id, follow: (id) => setSearch({ trace: id }) };
  const bySeq = useMemo(() => new Map(events.map((e) => [e.seq, e])), [events]);
  const hops = l.hops ?? [];
  const open = (h: Hop) => {
    const e = bySeq.get(h.seq);
    const step = e && eventStep(e);
    if (!step) return;
    // Stage views name records by their place in the stage input.
    const inputHop = h.kind === 'stage.output' && h.parent >= 0 ? hops[h.parent] : h;
    setSearch({ node: step.node, input: step.input, record: hopEntity(l, inputHop), tab: 'telemetry', view: undefined });
  };
  return (
    <div className="details journey">
      <div className="stage-head">
        <button type="button" onClick={() => setSearch({ trace: undefined })}>
          ← All records
        </button>
        <strong>{l.name || l.id}</strong>
        <span className="muted">
          {l.origin === 'aggregated' ? `${l.signal} data point counted by ${l.connector}` : `${l.signal} from ${l.receiver}`}
        </span>
      </div>
      {l.origin === 'aggregated' && (
        <div className="journey-sources">
          <span className="muted small">Counted from</span>{' '}
          {(l.sources ?? []).map((id) => (
            <button key={id} type="button" className="link chip" onClick={() => links.follow(id)} title="Follow this record">
              {links.name(id)}
            </button>
          ))}
        </div>
      )}
      <div className="journey-ends">
        {(l.deliveries ?? []).map((d, i) => (
          <span key={i} className={`end end-delivered${d.changed ? ' changed' : ''}`}>
            ✓ {d.exporter}
            {d.pipeline && <small> via {d.pipeline}</small>}
            {d.changed && <small> · changed</small>}
          </span>
        ))}
        {(l.stops ?? []).map((s, i) => (
          <span key={i} className={`end end-${s.end}`}>
            {endMark(s.end)} {END_LABELS[s.end] ?? s.end}
            {s.component && ` at ${s.component}`}
            {s.pipeline && <small> in {s.pipeline}</small>}
          </span>
        ))}
      </div>
      <ol className="hops">{roots.flatMap((n) => chain(n, hops, open, links))}</ol>
      <p className="muted small">
        Each hop is linked to the one before it by the call that carried the payload. Across a stage the record is followed
        by span ID, as the same record the stage passed on, or by content where neither applies; the match confidence is
        shown.
      </p>
    </div>
  );
}

type Open = (h: Hop) => void;

/**
 * chain renders hops in order while the path is linear and splits into
 * branches where it forks: a receiver feeding several pipelines, or a
 * connector delivering copies. A processor's input and output are one row.
 */
function chain(n: HopNode, hops: Hop[], open: Open, links: Links): ReactNode[] {
  const rows: ReactNode[] = [];
  let cur: HopNode | undefined = n;
  while (cur) {
    const h: Hop = cur.hop;
    const merged = h.kind === 'stage.input' && !!h.pipeline && cur.children.length === 1 && cur.children[0].hop.kind === 'stage.output';
    const last: HopNode = merged ? cur.children[0] : cur;
    rows.push(<HopRow key={cur.index} h={h} out={last.hop} merged={merged} open={open} links={links} />);
    if (last.children.length === 1 && !isDelivery(last.children[0].hop, hops)) {
      cur = last.children[0];
      continue;
    }
    if (last.children.length) {
      rows.push(
        <li key={`${last.index}-branches`} className="branches">
          {last.children.map((c) => (
            <div key={c.index} className="branch">
              <div className="hop-route">⇢ {c.hop.pipeline ?? c.hop.component}</div>
              <ol className="hops">{isDelivery(c.hop, hops) ? deliveryRows(c, hops, open, links) : chain(c, hops, open, links)}</ol>
            </div>
          ))}
        </li>,
      );
    }
    cur = undefined;
  }
  return rows;
}

/** isDelivery: a connector's stage.output into a pipeline, a branch head. A connector's input hop has no pipeline. */
const isDelivery = (h: Hop, hops: Hop[]) =>
  h.kind === 'stage.output' && h.parent >= 0 && hops[h.parent].kind === 'stage.input' && !hops[h.parent].pipeline;

function deliveryRows(n: HopNode, hops: Hop[], open: Open, links: Links): ReactNode[] {
  const rows: ReactNode[] = [];
  if (n.hop.end || n.hop.note) {
    rows.push(
      <li key={n.index} className={`hop hop-${n.hop.end ?? 'passed'}`}>
        {n.hop.end && <span className={`end end-${n.hop.end}`}>{END_LABELS[n.hop.end] ?? n.hop.end}</span>}
        {n.hop.note && <div className="note small">{n.hop.note}</div>}
      </li>,
    );
  }
  return [...rows, ...n.children.flatMap((c) => chain(c, hops, open, links))];
}

function hopTitle(h: Hop): { kind: string; name: string; where?: string } {
  switch (h.kind) {
    case 'input.injected':
      return { kind: 'receiver', name: h.component ?? '' };
    case 'exporter.captured':
      return { kind: 'exporter', name: h.component ?? '' };
    case 'stage.input':
      return { kind: 'in', name: h.component ?? '', where: h.pipeline };
    case 'stage.output':
      return { kind: 'out', name: h.component ?? '', where: h.pipeline };
  }
  return { kind: h.kind, name: h.component ?? '' };
}

function HopRow({ h, out, merged, open, links }: { h: Hop; out: Hop; merged: boolean; open: Open; links: Links }) {
  const t = hopTitle(h);
  const kind = merged ? 'processor' : h.kind === 'stage.input' && !h.pipeline ? 'connector' : t.kind;
  return (
    <li className={`hop hop-${out.end ?? (out.changes?.length ? 'changed' : 'passed')}`}>
      <div className="hop-head">
        <span className={`hop-kind kind-${kind}`}>{kind}</span>
        <button type="button" className="link" onClick={() => open(h)} title="Inspect the telemetry here">
          {t.name}
        </button>
        {t.where && <span className="muted">in {t.where}</span>}
        {out.confidence && (
          <span className={`conf conf-${out.confidence}`}>{confidenceLabel(out.confidence)}</span>
        )}
        {out.end && <span className={`end end-${out.end}`}>{END_LABELS[out.end] ?? out.end}</span>}
        {merged && !out.end && !out.changes?.length && <span className="muted small">unchanged</span>}
      </div>
      {out.changes?.length ? (
        <details>
        <summary>{out.changes.length} field change(s)</summary>
        <ul className="chg-list">
          {out.changes.map((c, i) => (
            <li key={i} className={`chg chg-${c.op}`}>
              <span className="chg-mark">{c.op === 'added' ? '+' : c.op === 'removed' ? '−' : '~'}</span>
              <code className="chg-path">
                {/^resource\[\d+\]$/.test(c.entity) ? 'resource ' : /\.scope\[\d+\]$/.test(c.entity) ? 'scope ' : ''}
                {c.path}
              </code>
              {c.op !== 'added' && <code className="val before">{display(c.before)}</code>}
              {c.op !== 'added' && c.op !== 'removed' && <span className="muted">→</span>}
              {c.op !== 'removed' && <code className="val after">{display(c.after)}</code>}
            </li>
          ))}
        </ul></details>
      ) : null}
      {out.note && <div className="note small">{out.note}</div>}
      {out.into?.length ? (
        <div className="journey-sources">
          <span className="muted small">Counted into</span>{' '}
          {out.into.map((id) => (
            <button key={id} type="button" className="link chip" onClick={() => links.follow(id)} title="Follow this data point">
              {links.name(id)}
            </button>
          ))}
        </div>
      ) : null}
    </li>
  );
}
