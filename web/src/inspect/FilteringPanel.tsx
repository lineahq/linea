import type { FilterDrop, Filtering } from '../api/types';
import type { WorkspaceSearch } from '../router';
import { itemNoun } from './batching';
import { dropsBy, filterSummary, refLabel, runTotals, totalsSummary, unused } from './filtering';

/**
 * FilteringPanel shows what a filter processor did: with several requests,
 * each condition's counts over the whole run; then, for one request, each
 * configured condition with how often it was evaluated, matched and failed,
 * and the records it removed, each opening its path through the run. It
 * reads the report's filtering; the drops are named only when the server's
 * evaluation reproduces the processor's output.
 */
export function FilteringPanel({ filtering, live, search, setSearch }: {
  filtering: Filtering[];
  live?: boolean;
  search: WorkspaceSearch;
  setSearch(patch: Partial<WorkspaceSearch>): void;
}) {
  if (!filtering.length && live) return <div className="placeholder">The filtering is shown when the run ends.</div>;
  if (!filtering.length) return <div className="placeholder">This run’s report holds no filtering for this processor, or it received no telemetry.</div>;
  const selected = filtering.find((f) => f.input === search.input) ?? filtering[0];
  const verified = selected.verification === 'replay_verified';
  const drops = dropsBy(selected);
  const idle = new Set(unused(selected).map((c) => c.index));
  const follow = (lineage: string) => setSearch({ trace: lineage, tab: 'journey', view: undefined });
  return (
    <div className="rv-changes rv-padded">
      {filtering.length > 1 && <WholeRun filtering={filtering} />}
      {filtering.length > 1 && (
        <div className="stage-head">
          <select aria-label="Request" value={selected.input} onChange={(e) => setSearch({ input: Number(e.target.value), record: undefined })}>
            {filtering.map((f, i) => <option key={f.input} value={f.input}>Request {i + 1} · {itemNoun(f.signal, f.records)}, {f.dropped} dropped</option>)}
          </select>
        </div>
      )}
      <p className={`rv-notice${verified ? ' rv-info' : ''}`}>{filterSummary(selected)}</p>
      {selected.error && <p className="muted small mono">{selected.error}</p>}
      {(selected.conditions ?? []).length > 0 && (
        <table className="grid bt-table" aria-label="Conditions">
          <thead>
            <tr>
              <th scope="col">#</th>
              <th scope="col">Condition</th>
              {verified && <><th scope="col" className="bt-num">Evaluated</th><th scope="col" className="bt-num">Matched</th><th scope="col" className="bt-num">Dropped</th><th scope="col" className="bt-num">Errors</th></>}
            </tr>
          </thead>
          <tbody>
            {selected.conditions!.map((c) => (
              <tr key={c.index}>
                <th scope="row">{c.index + 1}</th>
                <td>
                  <code className="mono">{c.condition}</code>
                  <div className="bt-note small">{c.source} · {c.context} context · error_mode {c.error_mode}{idle.has(c.index) && ' · removed nothing in this request'}</div>
                  {c.error && <div className="bt-note small mono">{c.error}</div>}
                  {verified && (drops.get(c.index) ?? []).length > 0 && (
                    <ul className="ft-drops" aria-label={`Removed by condition ${c.index + 1}`}>
                      {drops.get(c.index)!.map((d) => <Drop key={d.ref} drop={d} signal={selected.signal} onFollow={follow} />)}
                    </ul>
                  )}
                </td>
                {verified && <><td className="bt-num">{c.evaluated}</td><td className="bt-num">{c.matched}</td><td className="bt-num">{c.records}</td><td className="bt-num">{c.errors}</td></>}
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {(selected.notes ?? []).map((n) => <p key={n} className="muted small">{n}</p>)}
    </div>
  );
}

/** WholeRun sums every request of the processor, per condition. */
function WholeRun({ filtering }: { filtering: Filtering[] }) {
  const t = runTotals(filtering);
  return (
    <section aria-label="Whole run" className="ft-run">
      <h3 className="gb-head">Whole run</h3>
      <p className="muted small">{totalsSummary(t, filtering[0].signal)}</p>
      {t.verified > 0 && t.conditions.length > 0 && (
        <table className="grid bt-table" aria-label="Conditions over the whole run">
          <thead>
            <tr>
              <th scope="col">#</th>
              <th scope="col">Condition</th>
              <th scope="col" className="bt-num">Evaluated</th>
              <th scope="col" className="bt-num">Matched</th>
              <th scope="col" className="bt-num">Dropped</th>
              <th scope="col" className="bt-num">Errors</th>
            </tr>
          </thead>
          <tbody>
            {t.conditions.map((c) => (
              <tr key={c.index}>
                <th scope="row">{c.index + 1}</th>
                <td><code className="mono">{c.condition}</code>{c.evaluated > 0 && c.matched === 0 && c.errors === 0 && <div className="bt-note small">removed nothing in this run</div>}</td>
                <td className="bt-num">{c.evaluated}</td>
                <td className="bt-num">{c.matched}</td>
                <td className="bt-num">{c.records}</td>
                <td className="bt-num">{c.errors}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

/** Drop names what one drop removed: its records, each opening its path, or its position when the lineage did not follow a record. */
function Drop({ drop, signal, onFollow }: { drop: FilterDrop; signal: string; onFollow(lineage: string): void }) {
  const removed = drop.removed ?? [];
  const single = removed.length === 1 && removed[0].ref === drop.ref;
  if (single) {
    return (
      <li>
        <button type="button" className="link" onClick={() => onFollow(removed[0].lineage)} title="Follow this record through the run">{removed[0].name || 'record'}</button>
        <span className="muted"> · {refLabel(drop.ref)}</span>
      </li>
    );
  }
  return (
    <li>
      <span>{refLabel(drop.ref)}</span>
      {drop.records > 0 && <span className="muted"> · {itemNoun(signal, drop.records)}</span>}
      {removed.length > 0 && (
        <ul className="ft-drops ft-drops--nested">
          {removed.map((r) => (
            <li key={r.lineage}>
              <button type="button" className="link" onClick={() => onFollow(r.lineage)} title="Follow this record through the run">{r.name || 'record'}</button>
            </li>
          ))}
        </ul>
      )}
    </li>
  );
}
