import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import type { ComparisonRecord, Run, RunComparison } from '../api/types';
import { UnifiedDiff } from '../inspect/DiffViews';
import { FieldTable } from '../inspect/FieldTable';
import { RecordList } from '../inspect/RecordList';
import { Pill, RecordRow } from '../inspect/RecordRow';
import { Icon } from '../ui/Icon';
import { Crumbs } from '../ui/PageHead';
import {
  assertionOutcomes, COMPARE_FILTERS, compareCandidates, compareItems, comparePair, fateText, finished, outputRows, summaryCards, verdict,
} from '../runs/compare';
import { differingInputs } from '../runs/inputDiff';
import { followRows } from '../runs/followBoth';
import { useComparedRuns } from '../runs/comparedRuns';
import { FactChip } from './FactChip';
import {
  assertionOutcomeFact, comparedAssertions, comparedEvidence, comparedExecution, inputCount, runLabel, runtimeLabel, sourceText, whenText,
  type ComparedRun,
} from './runFacts';
import { useRunDetails } from './useRunDetails';

interface Props {
  /** The selected run (`?run=`). */
  runId: string;
  /** The run it is compared with (`?compare=`). */
  compareId: string;
  /** Called with the pair the picker chose; the caller puts it in the URL. */
  onChangeRuns(runId: string, compareId: string): void;
}

const callout = (tone: 'fail' | 'warn' | 'info', title: string, body?: string) => (
  <div className={`od-callout od-callout--${tone}`} role={tone === 'fail' ? 'alert' : 'status'}>
    <Icon name={tone === 'info' ? 'info' : tone === 'warn' ? 'triangle-alert' : 'octagon-alert'} size={16} />
    <div className="od-callout__main">
      <div className="od-callout__title">{title}</div>
      {body && <div className="od-callout__body">{body}</div>}
    </div>
  </div>
);

/**
 * ComparisonView is the whole-page comparison of two runs: baseline and
 * candidate identities, then eligibility with every reason a verdict is
 * missing, then where behaviour differs (exporter outputs) and the per-record,
 * route, error and assertion detail. The earlier admitted run is the baseline
 * (list order, since a configuration comparison admits both runs at once), so
 * there is no way to swap the sides. Nothing is matched or counted here; every
 * claim is the platform comparison's (`runs/compare.ts`). It is a whole-run
 * view, never a mode of the Changes tab.
 */
export function ComparisonView({ runId, compareId, onChangeRuns }: Props) {
  const { runs, list, pending } = useComparedRuns([runId, compareId]);
  const first = list.find((r) => r.id === runId);
  const second = list.find((r) => r.id === compareId);
  const ready = !!first && !!second && finished(first) && finished(second) && runId !== compareId;
  const pair = ready ? comparePair(list, runId, compareId) : undefined;
  const comparison = useQuery({
    queryKey: ['comparison', pair?.before, pair?.after],
    queryFn: () => api.compareRuns(pair!.before, pair!.after),
    enabled: !!pair,
    // Both runs have ended, so their comparison never changes.
    staleTime: Infinity,
  });
  const [picking, setPicking] = useState(false);
  const before = pair?.before ?? runId;
  const after = pair?.after ?? compareId;
  const title = `Compare runs ${runLabel(before)} and ${runLabel(after)}`;

  let body;
  if (runs.error) body = callout('fail', 'Could not load the runs', runs.error.message);
  else if (!runs.data || pending) body = <div className="od-empty" role="status"><div className="od-empty__body">Loading runs…</div></div>;
  else if (runId === compareId) body = callout('warn', 'Pick two different runs', 'A run cannot be compared with itself.');
  else if (!first || !second) {
    body = callout('warn', `${runLabel(!first ? runId : compareId)} is not in the run list`, 'It may have been deleted. Pick another run to compare.');
  } else if (!finished(first) || !finished(second)) {
    body = callout('info', 'A compared run has not finished', 'Compare it once it ends; both runs are compared from their recorded evidence.');
  } else if (comparison.error) body = callout('fail', 'Could not compare the runs', comparison.error.message);
  else if (!comparison.data) body = <div className="od-empty" role="status"><div className="od-empty__body">Comparing…</div></div>;
  else body = <Comparison c={comparison.data} runs={list} />;

  return (
    <>
      <header className="od-wbhead">
        <Crumbs items={[{ label: 'Debugger', to: '/' }, { label: title }]} />
        <span style={{ flex: 1 }} />
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <button type="button" className="od-btn" aria-expanded={picking} onClick={() => setPicking(!picking)}>
            <Icon name="git-compare" size={16} />
            <span>Change runs</span>
          </button>
        </div>
      </header>
      <div className="od-page">
        <div style={{ maxWidth: 1180, margin: '0 auto', display: 'flex', flexDirection: 'column', gap: 16 }}>
          {picking && <RunPicker runs={list} runId={runId} compareId={compareId}
            onApply={(a, b) => { setPicking(false); onChangeRuns(a, b); }} onCancel={() => setPicking(false)} />}
          {body}
        </div>
      </div>
    </>
  );
}

/**
 * InputDifferences diffs the two runs' recorded inputs (configuration first)
 * when any differ. Both runs have ended, so their inputs never change.
 */
function InputDifferences({ before, after }: { before: string; after: string }) {
  const [shown, setShown] = useState(false);
  const query = (id: string) => ({ queryKey: ['run-inputs', id], queryFn: () => api.runInputs(id), staleTime: Infinity, retry: false });
  const a = useQuery(query(before));
  const b = useQuery(query(after));
  if (a.error || b.error) return callout('warn', 'Could not read the recorded inputs', (a.error ?? b.error)!.message);
  if (!a.data || !b.data) return null;
  const diffs = differingInputs(a.data, b.data);
  if (!diffs.length) return null;
  const names = diffs.map((d) => d.name.toLowerCase()).join(', ');
  return (
    <section className="od-card" style={{ overflow: 'hidden' }} aria-label="Input differences">
      <div className="od-card__head">
        <h2 className="od-card__title" style={{ margin: 0 }}>Input differences</h2>
        <span className="od-subtle" style={{ fontSize: 12 }}>The runs differ in their {names}.</span>
        <span style={{ flex: 1 }} />
        <button type="button" className="od-btn od-btn--ghost od-btn--sm" aria-expanded={shown} onClick={() => setShown(!shown)}>
          <Icon name="file-diff" size={14} />
          <span>{shown ? 'Hide diff' : diffs[0].key === 'config' ? 'Configuration diff' : 'Input diff'}</span>
        </button>
      </div>
      {shown && diffs.map((d) => (
        <div key={d.key} className="od-card__body" aria-label={`${d.name} diff`}>
          <h3 className="t-body-strong" style={{ margin: '0 0 6px' }}>{d.name}</h3>
          <UnifiedDiff before={d.before} after={d.after} />
        </div>
      ))}
    </section>
  );
}

function runOption(r: Run): string {
  return `${runLabel(r.id)} · ${whenText(r.created)} · ${r.state}`;
}

/** RunPicker chooses the two finished runs to compare from the run list. */
function RunPicker({ runs, runId, compareId, onApply, onCancel }: { runs: Run[]; runId: string; compareId: string; onApply(a: string, b: string): void; onCancel(): void }) {
  const [a, setA] = useState(runId);
  const [b, setB] = useState(compareId);
  const options = runs.filter(finished);
  const valid = a !== b && options.some((r) => r.id === a) && options.some((r) => r.id === b);
  const select = (label: string, value: string, set: (v: string) => void) => (
    <label style={{ display: 'flex', flexDirection: 'column', gap: 4, fontSize: 12 }} className="od-muted">
      {label}
      <select className="od-select" value={value} onChange={(e) => set(e.target.value)} style={{ minWidth: 260 }}>
        {!options.some((r) => r.id === value) && <option value={value}>{runLabel(value)} (not available)</option>}
        {options.map((r) => <option key={r.id} value={r.id}>{runOption(r)}</option>)}
      </select>
    </label>
  );
  return (
    <section className="od-card" aria-label="Change runs">
      <div className="od-card__head"><h2 className="od-card__title" style={{ margin: 0 }}>Change runs</h2><span className="od-subtle" style={{ fontSize: 12 }}>The older run is always the baseline.</span></div>
      <div className="od-card__body" style={{ display: 'flex', alignItems: 'flex-end', gap: 12, flexWrap: 'wrap' }}>
        {select('Run', a, setA)}
        {select('Compare with', b, setB)}
        <button type="button" className="od-btn od-btn--primary" disabled={!valid} onClick={() => onApply(a, b)}><span>Compare these</span></button>
        <button type="button" className="od-btn od-btn--ghost" onClick={onCancel}><span>Cancel</span></button>
        <span style={{ flex: 1 }} />
        <Link to="/runs" className="od-btn od-btn--link"><span>All runs</span><Icon name="arrow-right" size={14} /></Link>
      </div>
      {compareCandidates(runs, a).length === 0 && <div className="od-card__body od-muted" style={{ fontSize: 12.5 }}>No other finished run to compare with.</div>}
    </section>
  );
}

function Comparison({ c, runs }: { c: RunComparison; runs: Run[] }) {
  const v = verdict(c);
  const [anyway, setAnyway] = useState(false);
  const sides = useRunDetails(runs.filter((r) => r.id === c.before.id || r.id === c.after.id));
  const conclusive = c.outcome !== 'inconclusive';
  const open = conclusive || anyway;
  const rows = outputRows(c);
  const label = { before: runLabel(c.before.id), after: runLabel(c.after.id) };
  return (
    <>
      <div className="od-cmp">
        <Side role="Baseline · earlier run" info={c.before} run={sides.runs.get(c.before.id)} outcomes={assertionOutcomes(c, 'before')} />
        <div className="od-cmp__mid"><Icon name="arrow-right" size={18} /></div>
        <Side role="Candidate · later run" info={c.after} run={sides.runs.get(c.after.id)} outcomes={assertionOutcomes(c, 'after')} />
      </div>

      <div className={`od-cmp-verdict${v.tone === 'no' ? ' od-cmp-verdict--no' : ''}`} role="status">
        <Icon name={v.tone === 'no' ? 'circle-x' : 'circle-check'} size={18} />
        <div style={{ flex: 1 }}>
          <div className="t-body-strong">{v.title} <span className="od-subtle" style={{ fontWeight: 400 }}>· outcome: {c.outcome}</span></div>
          <div className="od-muted" style={{ fontSize: 12.5 }}>{v.lead}</div>
          {v.reasons.length > 0 && (
            <ul style={{ margin: '6px 0 0', paddingLeft: 18, listStyle: 'disc', fontSize: 12.5, lineHeight: '19px' }}>
              {v.reasons.map((r, i) => <li key={(c.reasons ?? [])[i]?.code ?? i}>{r}.</li>)}
            </ul>
          )}
          {!c.inputs_identical && <div className="od-muted" style={{ fontSize: 12.5, marginTop: 6 }}>The captured inputs are not proven identical, so the runs cannot be compared; the exporter outputs below are for information only.</div>}
          {!conclusive && !anyway && (
            <div style={{ display: 'flex', gap: 6, marginTop: 10 }}>
              <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={() => setAnyway(true)}><span>Show differences anyway</span></button>
            </div>
          )}
        </div>
      </div>

      <InputDifferences before={c.before.id} after={c.after.id} />

      {open && <Summary c={c} />}

      <section className="od-card" style={{ overflow: 'hidden' }} aria-label="Outputs">
        <div className="od-card__head">
          <h2 className="od-card__title" style={{ margin: 0 }}>Outputs</h2>
          {!conclusive && <span className="od-chip od-chip--warn"><Icon name="triangle-alert" size={12} />Not conclusive</span>}
          <span className="od-subtle" style={{ fontSize: 12 }}>what each exporter received</span>
        </div>
        {rows.length ? (
          <table className="od-table">
            <thead><tr><th>Exporter</th><th>Signal</th><th className="num">Baseline {label.before}</th><th className="num">Candidate {label.after}</th><th>Change</th><th>Records</th><th aria-label="Open" /></tr></thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.key}>
                  <td className="mono">{r.exporter}</td>
                  <td>{r.signal}</td>
                  <td className="num">{r.before}</td>
                  <td className="num">{r.after}</td>
                  <td><span className={`od-delta od-delta--${r.delta.tone}`}>{r.delta.text}</span></td>
                  <td className="od-muted">{r.records}</td>
                  <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                    <Link to="/" search={{ run: c.before.id }} className="od-btn od-btn--link od-btn--sm"><span>Open in {label.before}</span></Link>
                    <Link to="/" search={{ run: c.after.id }} className="od-btn od-btn--link od-btn--sm"><span>Open in {label.after}</span></Link>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : <div className="od-card__body od-muted" style={{ fontSize: 12.5 }}>Neither run delivered any output.</div>}
      </section>

      {open ? <Details c={c} label={label} /> : (
        <section className="od-card" style={{ opacity: 0.6 }} aria-label="Record changes">
          <div className="od-card__head"><h2 className="od-card__title" style={{ margin: 0 }}>Record changes</h2><span className="od-chip od-chip--warn"><Icon name="triangle-alert" size={12} />Not conclusive</span></div>
          <div className="od-card__body od-muted" style={{ fontSize: 12.5 }}>Records, routes, errors and assertions are listed once you choose to show the differences anyway.</div>
        </section>
      )}
      {(c.notes ?? []).map((n, k) => <p key={k} className="od-muted" style={{ fontSize: 12.5, margin: 0 }}>{n}</p>)}
    </>
  );
}

/** Side is one identity card: role, the run, its three separate facts, and where and when it ran. */
function Side({ role, info, run, outcomes }: { role: string; info: ComparedRun; run?: Run; outcomes: string[] }) {
  const meta = [run && whenText(run.created), sourceText(run?.report), runtimeLabel(info.runtime), info.mode, inputCount(run?.report) !== undefined && `${inputCount(run?.report)} inputs`]
    .filter(Boolean).join(' · ');
  return (
    <div className="od-cmp__side">
      <span className="od-cmp__role">{role}</span>
      <div className="od-ident">
        <span className="od-ident__marker od-ident__marker--run"><Icon name="run-marker" size={14} />Run {runLabel(info.id)}</span>
        {run?.report?.scenario && <span className="od-ident__name">{run.report.scenario}</span>}
        <span style={{ flex: 1 }} />
        <Link to="/" search={{ run: info.id }} className="od-btn od-btn--link od-btn--sm"><span>Open run</span><Icon name="arrow-right" size={14} /></Link>
      </div>
      <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
        <FactChip fact={comparedExecution(info)} label="Execution" />
        <FactChip fact={comparedEvidence(info)} label="Evidence" />
        <FactChip fact={comparedAssertions(info, outcomes)} label="Assertions" />
      </div>
      {meta && <span className="od-subtle" style={{ fontSize: 12 }}>{meta}</span>}
    </div>
  );
}

function Summary({ c }: { c: RunComparison }) {
  const cards = summaryCards(c);
  const before = assertionOutcomes(c, 'before'), after = assertionOutcomes(c, 'after');
  const fails = (o: string[]) => o.filter((x) => x === 'fail').length;
  return (
    <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: 12 }}>
      {cards.map((s) => (
        <div key={s.id} className="od-card" style={{ padding: '12px 14px' }}>
          <div className="od-section-label">{s.label}</div>
          <div className="t-metric">{s.value} <span className="od-subtle" style={{ fontSize: 13 }}>{s.of}</span></div>
          {s.note && <div className="od-subtle" style={{ fontSize: 12 }}>{s.note}</div>}
        </div>
      ))}
      <div className="od-card" style={{ padding: '12px 14px' }}>
        <div className="od-section-label">Assertions</div>
        {before.length || after.length ? (
          <div style={{ display: 'flex', gap: 6, alignItems: 'center', marginTop: 4 }}>
            <span className={`od-chip od-chip--${fails(before) ? 'fail' : 'pass'}`}>{fails(before)} fail</span>
            <Icon name="arrow-right" size={14} />
            <span className={`od-chip od-chip--${fails(after) ? 'fail' : 'pass'}`}>{fails(after)} fail</span>
          </div>
        ) : <div className="od-subtle" style={{ fontSize: 12.5, marginTop: 4 }}>None in either run</div>}
      </div>
    </div>
  );
}

/** Details: per-record changes, routes, errors, assertions and ignored paths, as far as the platform listed them. */
function Details({ c, label }: { c: RunComparison; label: { before: string; after: string } }) {
  const items = compareItems(c);
  const routes = c.routes ?? [];
  const changedRoutes = routes.filter((r) => r.changed);
  const assertions = c.assertions ?? [];
  return (
    <>
      {c.records && (
        <section className="od-card" style={{ overflow: 'hidden' }} aria-label="Record changes">
          <div className="od-card__head">
            <h2 className="od-card__title" style={{ margin: 0 }}>Record changes</h2>
            <span className="od-subtle" style={{ fontSize: 12 }}>
              {c.records.aligned} aligned, {c.records.changed} changed, {c.records.unaligned} injected in one run only, {c.records.uncertain} could not be followed with confidence.
              {c.records.truncated && ' Only the first changed records are listed.'}
            </span>
          </div>
          <div className="od-card__body">
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
                    <dt>Baseline</dt><dd>{fateText(i.record.before)}</dd>
                    <dt>Candidate</dt><dd>{fateText(i.record.after)}</dd>
                  </dl>
                </div>
                <FollowBoth c={c} r={i.record} label={label} />
                {i.exporters.map((e) => <div key={e.exporter}>
                  <p className="rv-foot">At <code>{e.exporter}</code></p>
                  <FieldTable fields={e.fields} showAll={false} />
                </div>)}
                {i.record.changes_truncated && <p className="rv-foot">More field changes are not listed.</p>}
              </>} />
          </div>
        </section>
      )}

      {routes.length > 0 && (
        <section className="od-card" aria-label="Routes">
          <div className="od-card__head"><h2 className="od-card__title" style={{ margin: 0 }}>Routes</h2></div>
          <div className="od-card__body" style={{ fontSize: 12.5 }}>
            {changedRoutes.length ? (
              <ul style={{ margin: 0, paddingLeft: 18 }}>
                {changedRoutes.map((r) => <li key={`${r.connector}:${r.pipeline}`}><code>{r.connector}</code> → <code>{r.pipeline}</code>: {r.before} → {r.after} record(s)</li>)}
              </ul>
            ) : <span className="od-muted">No connector delivered a different number of records into any pipeline.</span>}
          </div>
        </section>
      )}

      <section className="od-card" aria-label="Assertions">
        <div className="od-card__head"><h2 className="od-card__title" style={{ margin: 0 }}>Assertions</h2></div>
        <div className="od-card__body" style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
          {assertions.length ? assertions.map((a) => {
            const was = assertionOutcomeFact(a.before), now = assertionOutcomeFact(a.after);
            return (
              <div key={a.name} style={{ display: 'grid', gridTemplateColumns: 'minmax(0, 1fr) 120px 24px 120px', gap: 10, alignItems: 'center', fontSize: 13 }}>
                <span>{a.name}</span>
                <FactChip fact={was} label="Baseline" />
                <Icon name="arrow-right" size={14} />
                <FactChip fact={now} label="Candidate" />
              </div>
            );
          }) : <span className="od-muted" style={{ fontSize: 12.5 }}>Neither run declared assertions.</span>}
        </div>
      </section>

      <section className="od-card" aria-label="Errors">
        <div className="od-card__head"><h2 className="od-card__title" style={{ margin: 0 }}>Errors</h2></div>
        <div className="od-card__body" style={{ fontSize: 12.5 }}>
          {(c.errors ?? []).length ? (
            <ul style={{ margin: 0, paddingLeft: 18 }}>
              {c.errors!.map((e, n) => <li key={n}>{e.source} {e.component && <code>{e.component}</code>}{e.pipeline && ` (${e.pipeline})`}: <code>{e.message}</code> {e.before} → {e.after} time(s)</li>)}
            </ul>
          ) : <span className="od-muted">No component error was recorded a different number of times in the two runs.</span>}
        </div>
      </section>

      {(c.ignored ?? []).length > 0 && (
        <section className="od-card" aria-label="Ignored paths">
          <div className="od-card__head"><h2 className="od-card__title" style={{ margin: 0 }}>Ignored paths</h2></div>
          <div className="od-card__body" style={{ fontSize: 12.5 }}>
            <ul style={{ margin: 0, paddingLeft: 18 }}>
              {c.ignored!.map((i) => <li key={i.rule}><code>{i.rule}</code>: {i.count} difference(s) counted, not treated as different</li>)}
            </ul>
          </div>
        </section>
      )}
    </>
  );
}

/** FollowBoth: the record's path through each run side by side, with a link to follow it in either run. */
function FollowBoth({ c, r, label }: { c: RunComparison; r: ComparisonRecord; label: { before: string; after: string } }) {
  const rows = followRows(r);
  return (
    <div className="rv-group">
      <div className="rv-group-head"><b>Follow in both</b>{r.diverges_at ? <span>diverges at <code>{r.diverges_at}</code></span> : <span>same path in each run</span>}</div>
      {rows.length > 0 && (
        <table className="od-follow-both">
          <thead><tr><th scope="col">Component</th><th scope="col">Baseline</th><th scope="col">Candidate</th></tr></thead>
          <tbody>
            {rows.map((row) => (
              <tr key={row.key} className={row.diverges ? 'is-diverges' : row.differs ? 'is-differs' : undefined}>
                <th scope="row"><code>{row.label}</code></th>
                <td>{row.before ?? <span className="od-subtle">not reached</span>}</td>
                <td>{row.after ?? <span className="od-subtle">not reached</span>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <p className="rv-foot">
        {r.before.lineage_id && <Link to="/" search={{ run: c.before.id, trace: r.before.lineage_id }}>Follow in {label.before}</Link>}
        {r.before.lineage_id && r.after.lineage_id && ' · '}
        {r.after.lineage_id && <Link to="/" search={{ run: c.after.id, trace: r.after.lineage_id }}>Follow in {label.after}</Link>}
      </p>
    </div>
  );
}
