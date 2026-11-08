import type { Filtering } from '../api/types';
import type { WorkspaceSearch } from '../router';
import { itemNoun } from './batching';
import { conditionSum, filterSummary, runTotals, totalsSummary, unused } from './filtering';

/** AllConditions is the table's last row: every condition's counts added up. */
function AllConditions({ conditions }: { conditions: Parameters<typeof conditionSum>[0] }) {
  if (conditions.length < 2) return null;
  const t = conditionSum(conditions);
  return (
    <tfoot>
      <tr>
        <th scope="row" />
        <td>All conditions</td>
        <td className="bt-num">{t.evaluated}</td><td className="bt-num">{t.matched}</td><td className="bt-num">{t.records}</td><td className="bt-num">{t.errors}</td>
      </tr>
    </tfoot>
  );
}

/**
 * FilteringPanel answers how each configured condition of a filter processor
 * behaved: with several requests, its counts over the whole run; then, for
 * one request, how often it was evaluated, matched, dropped and failed, and
 * whether it removed nothing. The records themselves live on Changes, which
 * names the condition that removed each one, so this view does not list them.
 * It reads the report's filtering; counts are shown only when the server's
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
  const idle = new Set(unused(selected).map((c) => c.index));
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
                <th scope="row">{c.index}</th>
                <td>
                  <code className="mono">{c.condition}</code>
                  <div className="bt-note small">{c.source} · {c.context} context · error_mode {c.error_mode}{idle.has(c.index) && ' · removed nothing in this request'}</div>
                  {c.error && <div className="bt-note small mono">{c.error}</div>}
                </td>
                {verified && <><td className="bt-num">{c.evaluated}</td><td className="bt-num">{c.matched}</td><td className="bt-num">{c.records}</td><td className="bt-num">{c.errors}</td></>}
              </tr>
            ))}
          </tbody>
          {verified && <AllConditions conditions={selected.conditions!} />}
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
                <th scope="row">{c.index}</th>
                <td><code className="mono">{c.condition}</code>{c.evaluated > 0 && c.matched === 0 && c.errors === 0 && <div className="bt-note small">removed nothing in this run</div>}</td>
                <td className="bt-num">{c.evaluated}</td>
                <td className="bt-num">{c.matched}</td>
                <td className="bt-num">{c.records}</td>
                <td className="bt-num">{c.errors}</td>
              </tr>
            ))}
          </tbody>
          <AllConditions conditions={t.conditions} />
        </table>
      )}
    </section>
  );
}
