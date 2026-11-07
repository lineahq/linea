import { useMemo } from 'react';
import type { WorkspaceSearch } from '../router';
import type { RunView } from '../runs/useRun';
import { runLoadText } from '../runs/deletion';
import { RunInputsPanel } from '../runs/RunInputs';
import { reportRuntime, UNVERIFIED } from '../runtimes/runtimes';
import { endpointText, simulatedEndpoints } from '../inspect/extensions';
import { Icon } from '../ui/Icon';
import { assertionRows, coverage, environmentRows, facets, hasStandIns, problems } from './overview';
import { keyFindings, type FindingAction, type Segment } from './findings';
import { recommendationViews } from './recommendations';
import { runLabel } from './runFacts';
import { stopNode } from './recordList';
import { keepsRunLog } from './runLog';
import { RunLogPanel } from './RunLogPanel';
import { RecordedChecks } from './RecordedChecks';

/** Text renders finding text: plain pieces and pieces shown as code. */
function Text({ parts }: { parts: Segment[] }) {
  return <>{parts.map((p, i) => (typeof p === 'string' ? p : <code key={i}>{p.code}</code>))}</>;
}

const MODE_ICON = { mocked: 'mode-mocked', captured: 'mode-captured', simulated: 'mode-simulated', standin: 'mode-standin', unsupported: 'ban' } as const;
const section = { display: 'flex', flexDirection: 'column', gap: 8 } as const;
const label = { margin: 0 } as const;

/**
 * RunOverview is the Overview tab of the Investigate left pane. It orders
 * the run as outcome, evidence scope, findings, then detail: three separate
 * facts, findings derived from the report with their provenance, the
 * assertions and the checks recorded later, what did not run for real
 * (always open), and the sources. A
 * failed or partial run also shows its Collector log after the errors.
 */
export function RunOverview({ view, setSearch }: { view: RunView; setSearch(patch: Partial<WorkspaceSearch>): void }) {
  const r = view.run;
  const rep = r?.report;
  const res = rep?.result;
  const findings = useMemo(() => (view.live ? [] : keyFindings(rep)), [rep, view.live]);
  const rows = useMemo(() => coverage(rep, view.events), [rep, view.events]);
  const recs = useMemo(() => (view.live || !r ? [] : recommendationViews(rep, runLabel(r.id))), [rep, r, view.live]);
  if (!r) {
    return (
      <div className="od-muted" style={{ padding: 14 }} role={view.error ? 'alert' : 'status'}>
        {view.error ? runLoadText(view.error) : 'Loading run…'}
      </div>
    );
  }
  const runName = runLabel(r.id);
  const p = problems(r);
  const asr = assertionRows(rep?.assertions);
  const env = environmentRows(rep);
  const endpoints = simulatedEndpoints(rep);
  const runtime = reportRuntime(rep?.runtime);
  const incomplete = !view.live && rows.some((x) => x.state !== 'recorded');
  const open = (node: string) => setSearch({ node, input: undefined, record: undefined, tab: undefined, view: undefined });
  const act = (a: FindingAction) => {
    if (a.kind === 'open') return open(a.node);
    const l = rep?.lineage?.find((x) => x.id === a.trace);
    const node = l && stopNode(l, view.events);
    setSearch({ trace: a.trace, ...(node ? { node } : {}), input: undefined, record: undefined });
  };
  const f = facets(r, view.events);

  return (
    <div style={{ padding: 14, display: 'flex', flexDirection: 'column', gap: 16 }}>
      <div className="od-facets" role="group" aria-label="Run outcome">
        {f.map((x) => (
          <div className="od-facet" key={x.name} title={x.fact.title}>
            <span className="od-facet__name">{x.name}</span>
            <span className={`od-facet__value od-facet__value--${x.fact.tone}`}>
              <Icon name={x.fact.icon} size={14} />
              {x.fact.text}
            </span>
            {x.detail && <span className="od-facet__detail">{x.detail}</span>}
          </div>
        ))}
        {view.live && (
          <div className="od-facet">
            <span className="od-facet__name">Events</span>
            <span className="od-facet__value od-facet__value--neutral">{view.events.length}</span>
          </div>
        )}
      </div>

      {p.errors.map((l, i) => (
        <div key={i} className="od-callout od-callout--fail" role="alert">
          <Icon name="circle-x" size={16} />
          <div className="od-callout__main">
            <div className="od-callout__title">Run error</div>
            <div className="od-callout__body">{l.text}</div>
          </div>
        </div>
      ))}
      {!view.live && keepsRunLog(r) && <RunLogPanel key={r.id} runId={r.id} />}
      {!view.live && p.warnings.length > 0 && (
        <div className="od-callout od-callout--warn" role="status" aria-label="Warnings">
          <Icon name="triangle-alert" size={16} />
          <div className="od-callout__main">
            <div className="od-callout__title">{p.warnings.length === 1 ? 'Evidence limit' : `${p.warnings.length} evidence limits`}</div>
            {p.warnings.map((l, i) => <div key={i} className="od-callout__body">{l.text}</div>)}
          </div>
        </div>
      )}

      {findings.length > 0 && (
        <section style={section} aria-label="Key findings">
          <h2 className="od-section-label" style={label}>Key findings</h2>
          {findings.map((x) => (
            <div key={x.id} className="od-card" style={{ padding: '10px 12px', display: 'flex', flexDirection: 'column', gap: 6 }}>
              <div style={{ display: 'flex', gap: 8 }}>
                <Icon name={x.icon} size={16} />
                <div>
                  <div className="t-body-strong"><Text parts={x.title} /></div>
                  {x.detail && <div className="od-muted" style={{ fontSize: 12.5 }}><Text parts={x.detail} /></div>}
                </div>
              </div>
              {x.actions.length > 0 && (
                <div style={{ display: 'flex', gap: 6, paddingLeft: 24, flexWrap: 'wrap' }}>
                  {x.actions.map((a, i) => a.kind === 'follow' ? (
                    <button key={i} type="button" className="od-btn od-btn--sm" onClick={() => act(a)}>
                      <Icon name="crosshair" size={14} /><span>{a.label}</span>
                    </button>
                  ) : (
                    <button key={i} type="button" className="od-btn od-btn--link od-btn--sm" onClick={() => act(a)}>
                      <span>{a.label}</span><Icon name="arrow-right" size={14} />
                    </button>
                  ))}
                </div>
              )}
              <span className="od-prov od-prov--observed"><Icon name="eye" size={12} />Observed in run {runName}</span>
            </div>
          ))}
        </section>
      )}

      {recs.length > 0 && (
        <section style={section} aria-label="Recommendations">
          <h2 className="od-section-label" style={label}>Recommendations</h2>
          <div className="od-muted" style={{ fontSize: 12 }}>Suggestions from this run's evidence. Nothing here changes your configuration; check each against your own traffic.</div>
          {recs.map((x) => (
            <div key={x.id} className="od-card" style={{ padding: '10px 12px', display: 'flex', flexDirection: 'column', gap: 6 }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
                <span className={`od-chip od-chip--plain${x.kind.tone === 'warn' ? ' od-chip--warn' : ''}`}>{x.kind.label}</span>
                <span className="t-body-strong">{x.title}</span>
                {x.pipeline && <span className="od-muted" style={{ fontSize: 12 }}>in <code>{x.pipeline}</code></span>}
              </div>
              <div style={{ fontSize: 12.5 }}>{x.summary}</div>
              {x.change && <div style={{ fontSize: 12.5 }}><span className="od-muted">Suggested change: </span><Text parts={x.change} /></div>}
              {x.saves && <div className="od-muted" style={{ fontSize: 12.5 }}>{x.saves}</div>}
              {x.blockers.map((b, i) => <div key={i} className="lim-warning" style={{ fontSize: 12 }}>⚠ {b}</div>)}
              {x.coverage.map((c, i) => <div key={i} className="od-muted" style={{ fontSize: 12 }}>{c}</div>)}
              {x.actions.length > 0 && (
                <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
                  {x.actions.map((a) => (
                    <button key={a.node} type="button" className="od-btn od-btn--link od-btn--sm" onClick={() => open(a.node)}>
                      <span>{a.label}</span><Icon name="arrow-right" size={14} />
                    </button>
                  ))}
                </div>
              )}
              <span className={`od-prov od-prov--${x.provenance.tone}`} title={x.rule}><Icon name="eye" size={12} />{x.provenance.text}</span>
            </div>
          ))}
        </section>
      )}

      {asr.length > 0 && (
        <section style={section} aria-label="Assertions">
          <h2 className="od-section-label" style={label}>Assertions</h2>
          {asr.map((a, i) => (
            <div key={i} className={`od-assert od-assert--${a.outcome === 'error' ? 'fail' : a.outcome}`}>
              <Icon name={a.icon} size={16} />
              <div>
                <div className="od-assert__text">{a.name} <code>{a.target}</code></div>
                {a.evidence.map((e, j) => <div key={j} className="od-assert__evidence">{e}</div>)}
                {!a.evidence.length && <div className="od-assert__evidence">{a.outcome}</div>}
              </div>
            </div>
          ))}
        </section>
      )}
      {!asr.length && rep && !view.live && res?.status === 'completed' && (
        <div className="od-muted" style={{ fontSize: 12 }}>No assertions: behavior was not checked against expectations.</div>
      )}
      {!view.live && r.state === 'done' && <RecordedChecks runId={r.id} />}

      {incomplete && (
        <section style={section} aria-label="What was recorded">
          <h2 className="od-section-label" style={label}>What was recorded</h2>
          <div className="od-card" style={{ overflow: 'hidden' }}>
            <table className="od-table od-table--dense">
              <tbody>
                {rows.map((x, i) => (
                  <tr key={i}>
                    <td style={x.state === 'recorded' ? undefined : { color: 'var(--warn)' }}>
                      <Icon name={x.state === 'recorded' ? 'circle-check' : 'circle-dashed'} size={14} />
                    </td>
                    <td className="mono">{x.id}</td>
                    <td className="od-muted">{x.detail}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div className="od-muted" style={{ fontSize: 12 }}>Missing output is not a drop: nothing was recorded there.</div>
        </section>
      )}

      {rep && (
        <section style={section} aria-label="Stand-ins">
          <h2 className="od-section-label" style={label}>Mocks, stand-ins and missing references</h2>
          {env.length > 0 && (
            <div className="od-card" style={{ overflow: 'hidden' }}>
              {env.map((x, i) => (
                <div key={i} style={{ display: 'flex', flexDirection: 'column', gap: 3, padding: '8px 12px', borderBottom: i < env.length - 1 ? '1px solid var(--border-subtle)' : undefined }}>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                    <span className={`od-mode od-mode--${x.mode}`}><Icon name={MODE_ICON[x.mode]} size={12} />{x.label}</span>
                    <code style={{ fontSize: 12 }}>{x.subject}</code>
                  </div>
                  <span className="od-muted" style={{ fontSize: 12 }}>{x.detail}</span>
                  {x.warnings.map((w, j) => <span key={j} className="lim-warning" style={{ fontSize: 12 }}>⚠ {w}</span>)}
                </div>
              ))}
            </div>
          )}
          <div className="od-muted" style={{ fontSize: 12 }}>
            {hasStandIns(rep) ? 'Checks on data that passed a stand-in are inconclusive, never a pass.' : 'No stand-in components. No missing references.'}
          </div>
        </section>
      )}

      {endpoints.length > 0 && (
        <section style={{ ...section, gap: 6 }} aria-label="Simulated endpoints">
          <h2 className="od-section-label" style={label}>Simulated endpoints</h2>
          <div className="od-muted" style={{ fontSize: 12 }}>
            Custom components ran for real with their client endpoints pointed at loopback: OTLP sinks, or the service of the provider named; the configured endpoints were never contacted.
          </div>
          <ul className="limitations">
            {endpoints.map((l) => (
              <li key={`${l.component} ${l.path}`}>
                <code>{l.component}</code> <code>{l.path}</code> ({l.protocol}, configured <code>{l.configured}</code>) {endpointText(l)}
              </li>
            ))}
          </ul>
        </section>
      )}

      {res && (res.inputs?.length || res.outputs?.length) ? (
        <details className="detail-disclosure">
          <summary>Inputs and outputs <span className="od-muted">· counts by receiver, exporter and signal</span></summary>
          <table className="od-table od-table--dense">
            <thead><tr><th>Receiver</th><th>Signal</th><th>Source</th><th className="num">Items</th><th>Injected</th></tr></thead>
            <tbody>
              {(res.inputs ?? []).map((i, n) => (
                <tr key={n}>
                  <td className="mono">{i.receiver}</td>
                  <td>{i.signal}</td>
                  <td>
                    {i.mock ? `${i.mock} mock` : 'scenario'}
                    {i.client_address && <span className="od-muted"> from {i.client_address}</span>}
                    {i.after && <span className="od-muted"> after {i.after}</span>}
                  </td>
                  <td className="num">{i.counts.items}</td>
                  <td>{i.injected ? 'yes' : (i.error ?? 'no')}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <table className="od-table od-table--dense">
            <thead><tr><th>Exporter</th><th>Signal</th><th className="num">Items</th><th className="num">Batches</th></tr></thead>
            <tbody>
              {(res.outputs ?? []).map((o, n) => (
                <tr key={n}><td className="mono">{o.exporter}</td><td>{o.signal}</td><td className="num">{o.counts.items}</td><td className="num">{o.batches}</td></tr>
              ))}
              {!res.outputs?.length && <tr><td colSpan={4} className="od-muted">Nothing reached an exporter{view.live ? ' yet' : ''}.</td></tr>}
            </tbody>
          </table>
          <p className="od-muted" style={{ fontSize: 12 }}>Captured totals include fan-out copies and connector-derived signals; they are not a dropped-record count.</p>
        </details>
      ) : null}

      {!view.live && p.notes.length > 0 && (
        <details className="detail-disclosure evidence-limits">
          <summary>Notes <span className="od-muted">· {p.notes.length}</span></summary>
          <ul className="limitations">{p.notes.map((l, i) => <li key={i} className="lim-info">{l.text}</li>)}</ul>
        </details>
      )}

      <RunInputsPanel
        key={r.id}
        runId={r.id}
        rows={
          <>
            {runtime && (
              <>
                <dt>Runtime</dt>
                <dd>
                  {runtime.label}
                  {runtime.unverified && <> <span className="od-chip od-chip--warn" title={UNVERIFIED}>unverified</span></>}
                  {runtime.custom.map((m) => <div key={m}>custom module <code>{m}</code></div>)}
                  {runtime.providers.map((x) => <div key={x}>provider <code>{x}</code></div>)}
                  {runtime.missing.length > 0 && <div className="od-muted">This runtime lacks {runtime.missing.join(', ')}; that evidence is absent.</div>}
                </dd>
              </>
            )}
            {res?.settle && (
              <>
                <dt>Settle</dt>
                <dd title={res.settle.reason}>
                  {res.settle.duration} <span className="od-muted">· {res.settle.reason === 'execution.settle' ? 'declared' : res.settle.reason}</span>
                </dd>
              </>
            )}
          </>
        }
      />
    </div>
  );
}
