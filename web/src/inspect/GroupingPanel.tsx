import type { GroupAttribute, GroupRequest, Grouping } from '../api/types';
import type { WorkspaceSearch } from '../router';
import { itemNoun } from './batching';
import { blockLabel, requestSummary, targets } from './grouping';

/**
 * GroupingPanel shows what a groupbyattrs processor did to one request: the
 * resource blocks it received, the blocks it emitted with the attributes it
 * moved up from the records, and where each record went. It reads the
 * report's grouping, which the server derives from the record lineage. A
 * record opens its path through the run.
 */
export function GroupingPanel({ grouping, live, search, setSearch }: {
  grouping?: Grouping;
  live?: boolean;
  search: WorkspaceSearch;
  setSearch(patch: Partial<WorkspaceSearch>): void;
}) {
  if (!grouping && live) return <div className="placeholder">The grouping is shown when the run ends.</div>;
  if (!grouping) return <div className="placeholder">This run’s report holds no grouping for this processor.</div>;
  const requests = grouping.requests ?? [];
  if (!requests.length) return <div className="placeholder">This processor received no telemetry in this run.</div>;
  const selected = requests.find((r) => r.input === search.input) ?? requests[0];
  const records = (r: GroupRequest) => (r.before ?? []).reduce((n, b) => n + b.records, 0);
  return (
    <div className="rv-changes rv-padded">
      {requests.length > 1 && (
        <div className="stage-head">
          <select aria-label="Request" value={selected.input} onChange={(e) => setSearch({ input: Number(e.target.value), record: undefined })}>
            {requests.map((r, i) => <option key={r.input} value={r.input}>Request {i + 1} · {itemNoun(grouping.signal, records(r))}</option>)}
          </select>
        </div>
      )}
      <p className="rv-notice rv-info">{requestSummary(selected)}</p>
      {selected.output !== undefined && (
        <GroupingFlow signal={grouping.signal} r={selected} onFollow={(id) => setSearch({ trace: id, tab: 'journey', view: undefined })} />
      )}
      {(grouping.notes ?? []).map((n) => <p key={n} className="muted small">{n}</p>)}
    </div>
  );
}

function Attrs({ attrs }: { attrs?: GroupAttribute[] }) {
  if (!attrs?.length) return <span className="muted small">no attributes</span>;
  return (
    <span className="gb-attrs">
      {attrs.map((a) => (
        <span key={a.key} className={`gb-attr mono${a.moved ? ' gb-attr--moved' : ''}`} title={a.moved ? 'Moved up from the records' : 'The resource’s own attribute'}>
          {a.key}={a.value}
        </span>
      ))}
    </span>
  );
}

/** GroupingFlow lays the input blocks beside the output blocks, each output naming the inputs its records came from. */
function GroupingFlow({ signal, r, onFollow }: { signal: string; r: GroupRequest; onFollow(lineage: string): void }) {
  const before = r.before ?? [];
  const after = r.after ?? [];
  return (
    <div className="gb-flow">
      <section aria-label="Resource blocks in">
        <h3 className="gb-head">In · {before.length} resource block{before.length === 1 ? '' : 's'}</h3>
        {before.map((b, i) => (
          <div key={i} className="gb-block">
            <div className="gb-block__head"><span className="gb-badge">In {i + 1}</span><span className="gb-block__name">{blockLabel(b.attributes)}</span></div>
            <Attrs attrs={b.attributes} />
            <div className="muted small">{itemNoun(signal, b.records)}{targets(r, i).length > 0 && <> → Out {targets(r, i).join(', ')}</>}</div>
          </div>
        ))}
      </section>
      <div className="gb-arrow" aria-hidden="true">→</div>
      <section aria-label="Resource blocks out">
        <h3 className="gb-head">Out · {after.length} resource block{after.length === 1 ? '' : 's'}</h3>
        {after.map((b, i) => {
          const from = b.from ?? [];
          const values = new Map((b.attributes ?? []).map((a) => [a.key, a.value]));
          return (
            <div key={i} className={`gb-block${(b.attributes ?? []).some((a) => a.moved) ? ' gb-block--grouped' : ''}`}>
              <div className="gb-block__head">
                <span className="gb-badge gb-badge--out">Out {i + 1}</span>
                <span className="gb-block__name">{blockLabel(b.attributes)}</span>
                {from.length > 1 && <span className="gb-tag" title="groupbyattrs merges blocks whose attributes end up equal">merged</span>}
              </div>
              <Attrs attrs={b.attributes} />
              <div className="muted small">from {from.map((f) => `In ${f.block + 1} (${f.records})`).join(', ') || 'records the lineage did not follow'}</div>
              {(b.records ?? []).length > 0 && (
                <ul className="gb-records">
                  {b.records!.map((rec) => (
                    <li key={rec.lineage}>
                      <button type="button" className="link" onClick={() => onFollow(rec.lineage)} title="Follow this record through the run">{rec.name || 'record'}</button>
                      {(rec.moved ?? []).length > 0 && <span className="muted small"> gave up {rec.moved!.map((k) => `${k}=${values.get(k) ?? ''}`).join(', ')}</span>}
                    </li>
                  ))}
                </ul>
              )}
            </div>
          );
        })}
      </section>
    </div>
  );
}
