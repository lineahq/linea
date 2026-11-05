import { useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import type { Run, RunComparison } from '../api/types';
import { FieldTable } from '../inspect/FieldTable';
import { RecordList } from '../inspect/RecordList';
import { Pill, RecordRow } from '../inspect/RecordRow';
import { COMPARE_FILTERS, compareCandidates, compareItems, comparePair, exporterText, fateText, finished, headline } from './compare';
import { useComparedRuns } from './comparedRuns';

interface Props {
  runId: string;
  /** The run picked to compare with, if any. */
  other?: string;
  onOther(id: string | undefined): void;
}

const runLabel = (r: Run) => `${new Date(r.created).toLocaleTimeString()} · ${r.state} · ${r.id.slice(0, 8)}`;

/**
 * ComparePanel compares the selected run with another run of the list: the
 * earlier admitted of the two is the before run. It shows what the platform's comparison
 * found, from the two runs' recorded evidence; nothing executes again.
 */
export function ComparePanel({ runId, other, onOther }: Props) {
  const { runs, list } = useComparedRuns([runId, other]);
  const current = list.find((r) => r.id === runId);
  const picked = other && other !== runId ? list.find((r) => r.id === other) : undefined;
  const pair = current && picked && finished(current) && finished(picked) ? comparePair(list, current.id, picked.id) : undefined;
  const comparison = useQuery({
    queryKey: ['comparison', pair?.before, pair?.after],
    queryFn: () => api.compareRuns(pair!.before, pair!.after),
    enabled: !!pair,
    // Both runs have ended, so their comparison never changes.
    staleTime: Infinity,
  });
  const candidates = compareCandidates(list, runId);

  if (runs.error) return <p className="error">Could not load the runs: {runs.error.message}</p>;
  if (!current) return <div className="placeholder">Loading runs…</div>;
  if (!finished(current)) return <div className="placeholder">This run has not finished; compare it once it ends.</div>;
  return (
    <div className="details compare">
      <label className="compare-pick">
        Compare with…{' '}
        <select value={picked?.id ?? ''} onChange={(e) => onOther(e.target.value || undefined)} disabled={!candidates.length}>
          <option value="">{candidates.length ? 'Choose a run' : 'No other finished run'}</option>
          {candidates.map((r) => <option key={r.id} value={r.id}>{runLabel(r)}</option>)}
        </select>
      </label>
      {!picked && <p className="note">Pick another run from this server's history. The older run is the before run; both are compared from their recorded evidence.</p>}
      {comparison.error && <p className="error" role="alert">Could not compare the runs: {comparison.error.message}</p>}
      {pair && !comparison.data && !comparison.error && <p className="note">Comparing…</p>}
      {comparison.data && <ComparisonView c={comparison.data} />}
    </div>
  );
}

function ComparisonView({ c }: { c: RunComparison }) {
  const head = headline(c);
  const items = compareItems(c);
  const routes = (c.routes ?? []).filter((r) => r.changed);
  const assertions = (c.assertions ?? []).filter((a) => a.changed || !a.before || !a.after);
  return <>
    <p className="compare-outcome"><span className={`status status-${head.cls}`}>{c.outcome}</span> {head.text}</p>
    {(c.reasons ?? []).map((r) => <p key={r.code} className="note lim-warning" role="alert">{r.message}</p>)}
    <dl className="facts">
      {([['Before', c.before], ['After', c.after]] as const).map(([name, r]) => <div key={name} style={{ display: 'contents' }}>
        <dt>{name}</dt>
        <dd>
          <code>{r.id}</code>{r.mode && ` · ${r.mode}`} · {r.end}{r.execution_status && `, execution ${r.execution_status}`}
          {!r.complete && r.incomplete && `, incomplete: ${r.incomplete}`}{r.assertions && `, assertions ${r.assertions}`}
        </dd>
      </div>)}
      <dt>Inputs</dt>
      <dd>{c.inputs_identical ? 'identical (same captured input digest)' : 'not proven identical, so the runs cannot be compared; exporter outputs are shown for information only'}</dd>
    </dl>

    <h4>Exporter outputs</h4>
    {c.exporters?.length ? <table className="grid">
      <thead><tr><th scope="col">Exporter</th><th scope="col">Signal</th><th scope="col">Before</th><th scope="col">After</th><th scope="col">Records</th></tr></thead>
      <tbody>{c.exporters.map((e) => <tr key={`${e.exporter}:${e.signal}`}>
        <td><code>{e.exporter}</code></td><td>{e.signal}</td><td>{e.before}</td><td>{e.after}</td><td>{exporterText(e)}</td>
      </tr>)}</tbody>
    </table> : <p className="note">Neither run delivered any output.</p>}

    {c.records && <>
      <h4>Injected records</h4>
      <p className="muted small">
        {c.records.aligned} aligned, {c.records.changed} changed, {c.records.unaligned} injected in one run only, {c.records.uncertain} could not be followed with confidence.
        {c.records.truncated && ' Only the first changed records are listed.'}
      </p>
      <RecordList
        items={items}
        itemId={(i) => i.id}
        filters={COMPARE_FILTERS}
        label="Injected records that differ between the runs"
        hint="see where it ended and what changed"
        pageNoun="records"
        defaultOpen
        empty={items.length ? undefined : 'No injected record differs between the runs.'}
        renderRow={(i, s) => <RecordRow {...s} onToggle={s.toggle} glyph={i.record.fate_changed ? '≠' : '~'} glyphTone={i.tone}
          badge={i.record.signal} title={i.title} why={i.why}
          pills={i.record.confidence === 'medium' && <Pill>medium-confidence match</Pill>} />}
        renderDetail={(i) => <>
          <div className="rv-group">
            <div className="rv-group-head"><b>Where it ended</b>{i.record.fate_changed && <span>differently in each run</span>}</div>
            <dl className="facts">
              <dt>Before</dt><dd>{fateText(i.record.before)}</dd>
              <dt>After</dt><dd>{fateText(i.record.after)}</dd>
            </dl>
          </div>
          {i.exporters.map((e) => <div key={e.exporter}>
            <p className="rv-foot">At <code>{e.exporter}</code></p>
            <FieldTable fields={e.fields} showAll={false} />
          </div>)}
          {i.record.changes_truncated && <p className="rv-foot">More field changes are not listed.</p>}
        </>} />
    </>}

    {routes.length > 0 && <>
      <h4>Routes</h4>
      <ul>{routes.map((r) => <li key={`${r.connector}:${r.pipeline}`}><code>{r.connector}</code> → <code>{r.pipeline}</code>: {r.before} → {r.after} record(s)</li>)}</ul>
    </>}
    {(c.errors ?? []).length > 0 && <>
      <h4>Errors</h4>
      <ul>{c.errors!.map((e, n) => <li key={n}>{e.source} {e.component && <code>{e.component}</code>}{e.pipeline && ` (${e.pipeline})`}: <code>{e.message}</code> {e.before} → {e.after} time(s)</li>)}</ul>
    </>}
    {assertions.length > 0 && <>
      <h4>Assertions</h4>
      <ul>{assertions.map((a) => <li key={a.name}><code>{a.name}</code>: {a.before || 'absent'} → {a.after || 'absent'}</li>)}</ul>
    </>}
    {(c.ignored ?? []).length > 0 && <>
      <h4>Ignored paths</h4>
      <ul>{c.ignored!.map((i) => <li key={i.rule}><code>{i.rule}</code>: {i.count} difference(s) counted, not treated as different</li>)}</ul>
    </>}
    {(c.notes ?? []).map((n, k) => <p key={k} className="note">{n}</p>)}
  </>;
}
