import type { Declaration, Run } from '../api/types';
import type { RunView } from './useRun';
import { limitations } from './limitations';
import { standIns } from './standins';
import { endpointText, simulatedEndpoints } from '../inspect/extensions';
import { deletedRun, runLoadText } from './deletion';
import { reportRuntime, UNVERIFIED } from '../runtimes/runtimes';

export function stateLabel(view: RunView): { text: string; cls: string } {
  const r = view.run;
  if (!r) {
    if (deletedRun(view.error)) return { text: 'Deleted', cls: 'warn' };
    return view.error ? { text: 'Unavailable', cls: 'bad' } : { text: 'Loading…', cls: 'idle' };
  }
  if (view.live) return view.events.length ? { text: 'Running', cls: 'idle' } : { text: 'Queued', cls: 'idle' };
  if (r.state === 'error') return { text: 'Error', cls: 'bad' };
  if (r.state === 'cancelled') return { text: 'Cancelled', cls: 'warn' };
  const status = r.report?.result?.status;
  if (status === 'completed') {
    const failed = r.report?.assertions && r.report.assertions.outcome !== 'pass';
    return failed ? { text: `Completed · assertions ${r.report!.assertions!.outcome}`, cls: 'warn' } : { text: 'Completed', cls: 'good' };
  }
  return { text: status ?? r.state, cls: status === 'failed' ? 'bad' : 'warn' };
}

function duration(r: Run): string | undefined {
  if (!r.started || !r.finished) return undefined;
  const ms = new Date(r.finished).getTime() - new Date(r.started).getTime();
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`;
}

/** RunSummary shows a run's status, inputs, outputs, assertions and limitations. */
export function RunSummary({ view }: { view: RunView }) {
  const r = view.run;
  if (!r) return <div className={`placeholder${view.error ? ' error' : ''}`}>{view.error ? runLoadText(view.error) : 'Loading run…'}</div>;
  const rep = r.report;
  const res = rep?.result;
  const label = stateLabel(view);
  const limits = limitations(r);
  const warnings = limits.filter((l) => l.level === 'warning');
  const errors = limits.filter((l) => l.level === 'error');
  const stand = standIns(rep);
  const endpoints = simulatedEndpoints(rep);
  const standLines = [...stand.components, ...stand.references];
  const runtime = reportRuntime(rep?.runtime);
  return (
    <div className="details run-summary">
      <dl className="facts">
        <dt>Run</dt>
        <dd>
          <code>{r.id}</code> <span className={`status status-${label.cls}`}>{label.text}</span>
        </dd>
        {runtime && (
          <>
            <dt>Runtime</dt>
            <dd>
              {runtime.label}
              {runtime.unverified && <> <span className="badge unverified" title={UNVERIFIED}>unverified</span></>}
              {runtime.custom.length > 0 && <ul className="custom-modules">{runtime.custom.map((m) => <li key={m}><code>{m}</code></li>)}</ul>}
              {runtime.providers.length > 0 && <ul className="custom-modules">{runtime.providers.map((p) => <li key={p}>provider <code>{p}</code></li>)}</ul>}
              {runtime.missing.length > 0 && <p className="missing-features">This runtime lacks {runtime.missing.join(', ')}; that evidence is absent.</p>}
            </dd>
          </>
        )}
        {rep?.scenario && (
          <>
            <dt>Scenario</dt>
            <dd>{rep.scenario}</dd>
          </>
        )}
        {duration(r) && (
          <>
            <dt>Duration</dt>
            <dd>{duration(r)}</dd>
          </>
        )}
        {res?.settle && (
          <>
            <dt>Settle</dt>
            <dd title={res.settle.reason}>
              {res.settle.duration} <span className="muted">· {res.settle.reason === 'execution.settle' ? 'declared' : res.settle.reason}</span>
            </dd>
          </>
        )}
        {view.live && (
          <>
            <dt>Events</dt>
            <dd>{view.events.length}</dd>
          </>
        )}
      </dl>

      {errors.map((l, i) => <p key={i} className="error" role="alert">{l.text}</p>)}
      {standLines.length > 0 && (
        <section className="stand-ins" aria-label="Stand-ins">
          <h4>Stand-ins</h4>
          <p className="note">These ran in place of the real thing. Checks on data that passed a stand-in are inconclusive, never a pass.</p>
          <ul className="limitations">{standLines.map((l, i) => <li key={i} className="lim-warning"><code>{l.subject}</code> {l.text}</li>)}</ul>
        </section>
      )}
      {endpoints.length > 0 && (
        <section className="simulated-endpoints" aria-label="Simulated endpoints">
          <h4>Simulated endpoints</h4>
          <p className="note">Custom components ran for real with their client endpoints pointed at loopback: OTLP sinks, or the service of the provider named; the configured endpoints were never contacted.</p>
          <ul className="limitations">{endpoints.map((l) => <li key={`${l.component} ${l.path}`}><code>{l.component}</code> <code>{l.path}</code> ({l.protocol}, configured <code>{l.configured}</code>) {endpointText(l)}</li>)}</ul>
        </section>
      )}
      {warnings.length > 0 && <p className="note lim-warning">{warnings[0].text}{warnings.length > 1 ? ` Plus ${warnings.length - 1} more warning(s); review Evidence limits below.` : ''}</p>}

      {res && (
        <>
        <div className="overview-cards">
          <div><strong>{(res.inputs ?? []).filter((i) => i.injected).reduce((n, i) => n + i.counts.items, 0)}</strong><span>Injected items</span></div>
          <div><strong>{(res.outputs ?? []).reduce((n, o) => n + o.counts.items, 0)}</strong><span>Captured items</span></div>
          <div><strong>{res.outputs?.length ?? 0}</strong><span>Exporter / signal outputs</span></div>
        </div>
        <p className="muted small">Captured totals include fan-out copies and connector-derived signals; they are not a dropped-record count.</p>
        {!res.outputs?.length && <p className="note">Nothing reached an exporter{view.live ? ' yet' : ''}.</p>}
        <details className="detail-disclosure"><summary>Input / output details <span className="muted">· counts by receiver, exporter and signal</span></summary>
        <div className="columns">
          <div>
            <h4>Inputs</h4>
            <table className="grid">
              <thead>
                <tr>
                  <th>Receiver</th>
                  <th>Signal</th>
                  <th>Source</th>
                  <th>Items</th>
                  <th>Injected</th>
                </tr>
              </thead>
              <tbody>
                {(res.inputs ?? []).map((i, n) => (
                  <tr key={n}>
                    <td>{i.receiver}</td>
                    <td>{i.signal}</td>
                    <td>
                      {i.mock ? <span className="mode mode-dependency_mocked">{i.mock} mock</span> : 'scenario'}
                      {i.client_address && <span className="muted"> from {i.client_address}</span>}
                      {i.after && <span className="muted"> after {i.after}</span>}
                    </td>
                    <td>{i.counts.items}</td>
                    <td>{i.injected ? 'yes' : (i.error ?? 'no')}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div>
            <h4>Outputs</h4>
            <table className="grid">
              <thead>
                <tr>
                  <th>Exporter</th>
                  <th>Signal</th>
                  <th>Items</th>
                  <th>Batches</th>
                </tr>
              </thead>
              <tbody>
                {(res.outputs ?? []).map((o, n) => (
                  <tr key={n}>
                    <td>{o.exporter}</td>
                    <td>{o.signal}</td>
                    <td>{o.counts.items}</td>
                    <td>{o.batches}</td>
                  </tr>
                ))}
                {!res.outputs?.length && (
                  <tr>
                    <td colSpan={4} className="muted">
                      Nothing reached an exporter.
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>
        </div></details></>
      )}

      {res?.mocks?.length ? <details className="detail-disclosure"><summary>Mocked dependencies <span className="muted">· {res.mocks.length} component(s)</span></summary><MockedDependencies mocks={res.mocks} /></details> : null}

      {!view.live && limits.length > 0 && <details className="detail-disclosure evidence-limits"><summary>Evidence limits <span className="muted">· {errors.length} errors, {warnings.length} warnings, {limits.filter((l) => l.level === 'info').length} notes</span></summary>
        <ul className="limitations">{limits.map((l, i) => <li key={i} className={`lim-${l.level}`}>{l.text}</li>)}</ul>
      </details>}

      {rep?.assertions && (
        <details className="detail-disclosure" open={rep.assertions.outcome !== 'pass'}>
          <summary>Checks: {rep.assertions.outcome} <span className="muted">· {rep.assertions.results?.length ?? 0} assertions</span></summary>
          <ul className="assertions">
            {(rep.assertions.results ?? []).map((a, i) => (
              <li key={i} className={`outcome-${a.outcome}`}>
                <span className="badge">{a.outcome}</span> {a.name} <span className="muted">{a.target}</span>
                {(a.checks ?? [])
                  .filter((c) => c.outcome !== 'pass')
                  .map((c, j) => (
                    <div key={j} className="check">
                      {c.check}
                      {c.key ? ` ${c.key}` : ''}: expected {c.expected}
                      {c.observed !== undefined ? `, observed ${c.observed}` : ''}
                      {c.message ? ` — ${c.message}` : ''}
                    </div>
                  ))}
              </li>
            ))}
          </ul>
        </details>
      )}
    </div>
  );
}

/**
 * MockedDependencies lists the components that ran for real against a mocked
 * dependency, with the fixtures each one used.
 */
function MockedDependencies({ mocks }: { mocks: Declaration[] }) {
  return (
    <>
      <h4>Mocked dependencies</h4>
      <table className="grid mocks">
        <thead>
          <tr>
            <th>Component</th>
            <th>Mock</th>
            <th>Supplied</th>
          </tr>
        </thead>
        <tbody>
          {mocks.map((d) => (
            <tr key={d.component}>
              <td>
                {d.component}{' '}
                {d.warnings?.length ? (
                  <span className="lim-warning" title={d.warnings.join('\n')}>
                    ⚠ {d.warnings.length}
                  </span>
                ) : null}
              </td>
              <td>
                <span className={`mode mode-${d.mode}`}>{d.provider}</span>
              </td>
              <td>
                {d.supplied?.length ? (
                  <ul className="supplied">
                    {d.supplied.map((s) => (
                      <li key={s}>
                        <code>{s}</code>
                      </li>
                    ))}
                  </ul>
                ) : (
                  <span className="muted">nothing</span>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  );
}
