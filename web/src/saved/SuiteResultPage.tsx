import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Link, useNavigate, useParams } from '@tanstack/react-router';
import { api } from '../api/client';
import { Icon } from '../ui/Icon';
import { Crumbs } from '../ui/PageHead';
import { shortId, whenText } from './format';
import { chipClass, firstFailing, junitHref, memberRows, rerunBlocker, suiteFacets, type Badge } from './suiteView';
import { isMissing, useSuiteReport, useSuiteStatus } from './useSuite';
import './saved.css';
import { useDenial } from '../ui/session';

function Chip({ badge, label }: { badge: Badge; label?: string }) {
  return (
    <span className={chipClass(badge.tone)}>
      <Icon name={badge.icon} size={12} />
      {label ?? badge.label}
    </span>
  );
}

/**
 * SuiteResultPage is a suite's result: its overall outcome, each member's
 * execution and assertions, and the assertions that failed or could not be
 * decided, each leading into the member's run in the debugger. It follows
 * the suite until every member has finished. A finished suite reruns from its
 * members' recorded inputs and downloads as JUnit XML.
 */
export function SuiteResultPage() {
  const { suiteId = '' } = useParams({ strict: false });
  const status = useSuiteStatus(suiteId);
  const done = status.data?.done ?? false;
  const report = useSuiteReport(suiteId, done);
  const qc = useQueryClient();
  const navigate = useNavigate();
  const runDenied = useDenial('run.submit');
  const rerun = useMutation({
    mutationFn: () => api.rerunSuite(suiteId),
    onSuccess: (suite) => {
      void qc.invalidateQueries({ queryKey: ['suites'] });
      void navigate({ to: '/saved/suites/$suiteId', params: { suiteId: suite.id } });
    },
  });

  const crumb = (what: string) => (
    <header className="od-wbhead">
      <Crumbs items={[{ label: 'Saved work', to: '/saved/suites' }, { label: what }]} />
    </header>
  );

  if (status.isPending) {
    return (
      <>
        {crumb('Suite result')}
        <div className="od-page"><div className="od-empty" role="status"><div className="od-empty__title">Loading the suite…</div></div></div>
      </>
    );
  }
  if (status.error) {
    const missing = isMissing(status.error);
    return (
      <>
        {crumb('Suite result')}
        <div className="od-page">
          <div className="od-card">
            <div className="od-empty" role="alert">
              <div className="od-empty__title">{missing ? 'This suite does not exist' : 'The suite could not be read'}</div>
              <div className="od-empty__body">{missing ? 'It was deleted, or the server no longer keeps it.' : status.error.message}</div>
              <div className="od-empty__actions"><Link to="/saved/suites" className="od-btn">Back to suites</Link></div>
            </div>
          </div>
        </div>
      </>
    );
  }

  const s = status.data;
  const facets = suiteFacets(s, report.data);
  const rows = memberRows(s, report.data);
  const failing = firstFailing(rows);
  const blocker = rerunBlocker(s);

  return (
    <>
      <header className="od-wbhead">
        <Crumbs items={[{ label: 'Saved work', to: '/saved/suites' }, { label: <span title={s.created}>Suite result · {whenText(s.created).toLowerCase()}</span> }]} />
        <span className="saved-grow" />
        {done && (
          <a href={junitHref(s.id)} download={`${s.id}.junit.xml`} className="od-btn" title="The suite's assertions as JUnit XML, one testcase per assertion">
            <Icon name="download" size={14} /><span>JUnit</span>
          </a>
        )}
        <button type="button" className="od-btn" disabled={!!blocker || rerun.isPending || !!runDenied} title={runDenied ?? blocker ?? 'Rerun every member from the inputs it recorded, on the same runtime'} onClick={() => rerun.mutate()}>
          <Icon name={rerun.isPending ? 'loader-circle' : 'rotate-ccw'} size={14} /><span>{rerun.isPending ? 'Rerunning…' : 'Rerun suite'}</span>
        </button>
        {failing?.runId && (
          <Link to="/" search={{ run: failing.runId }} className="od-btn">
            <span>Open failing member</span><Icon name="arrow-right" size={14} />
          </Link>
        )}
      </header>
      <div className="od-page">
        <div className="saved-result">
          <div className="od-facets" role="group" aria-label="Suite summary">
            {facets.map((f) => (
              <div className="od-facet" key={f.name}>
                <span className="od-facet__name">{f.name}</span>
                <span className={`od-facet__value od-facet__value--${f.tone}`}><Icon name={f.icon} size={14} />{f.label}</span>
                <span className="od-facet__detail">{f.detail}</span>
              </div>
            ))}
          </div>

          {!done && (
            <div className="od-callout od-callout--info" role="status">
              <Icon name="loader-circle" size={16} />
              <div className="od-callout__main">
                <div className="od-callout__title">The suite is still running</div>
                <div className="od-callout__body">This page follows it and fills in the assertions when the last member finishes. Leaving does not stop it.</div>
              </div>
            </div>
          )}
          {rerun.error && (
            <div className="od-callout od-callout--fail" role="alert">
              <Icon name="circle-x" size={16} />
              <div className="od-callout__main">
                <div className="od-callout__title">The suite could not be rerun</div>
                <div className="od-callout__body">{rerun.error instanceof Error ? rerun.error.message : String(rerun.error)}</div>
              </div>
            </div>
          )}
          {done && report.error && (
            <div className="od-callout od-callout--warn" role="status">
              <Icon name="triangle-alert" size={16} />
              <div className="od-callout__main"><div className="od-callout__body">The assertion details could not be read ({report.error.message}); member outcomes are shown.</div></div>
            </div>
          )}

          <section className="od-card" style={{ overflow: 'hidden' }}>
            <div className="od-card__head">
              <h2 className="od-card__title" style={{ margin: 0 }}>Members</h2>
              <span className="od-subtle" style={{ fontSize: 12 }}>configuration <code>{s.config}</code></span>
            </div>
            <table className="od-table">
              <thead>
                <tr><th>Scenario</th><th>Run</th><th>Execution</th><th>Assertions</th><th><span className="sr-only">Actions</span></th></tr>
              </thead>
              <tbody>
                {rows.map((r, i) => (
                  <tr key={i} aria-selected={r === failing}>
                    <td className="mono">{r.label}</td>
                    <td className="mono" title={r.runId}>{r.runId ? shortId(r.runId) : '—'}</td>
                    <td><Chip badge={r.state} /></td>
                    <td><Chip badge={r.outcome} label={r.assertions || r.outcome.label} /></td>
                    <td style={{ textAlign: 'right' }}>
                      {r.runId && (
                        <Link to="/" search={{ run: r.runId }} className="od-btn od-btn--link od-btn--sm">
                          <span>Investigate {shortId(r.runId)}</span><Icon name="arrow-right" size={14} />
                        </Link>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            {rows.filter((r) => r.attention.length > 0 || r.message).map((r, i) => (
              <div className="saved-attention" key={i}>
                <div className="od-section-label">{r.label} · {r.attention.length > 0 ? 'failing assertions' : 'why it has no result'}</div>
                {r.message && <div className="od-assert od-assert--fail"><Icon name="circle-x" size={16} /><div><div className="od-assert__text">{r.message}</div></div><div /></div>}
                {r.attention.map((a, j) => (
                  <div key={j} className={`od-assert od-assert--${a.tone === 'warn' ? 'inconclusive' : 'fail'}`}>
                    <Icon name={a.tone === 'warn' ? 'triangle-alert' : 'circle-x'} size={16} />
                    <div>
                      <div className="od-assert__text">{a.name} <code>{a.target}</code></div>
                      {a.evidence && <div className="od-assert__evidence">{a.evidence}</div>}
                    </div>
                    <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
                      {r.runId && (
                        <Link to="/" search={{ run: r.runId }} className="od-btn od-btn--link od-btn--sm">
                          <span>Open in run {shortId(r.runId)}</span><Icon name="arrow-right" size={14} />
                        </Link>
                      )}
                    </div>
                  </div>
                ))}
              </div>
            ))}
          </section>
        </div>
      </div>
    </>
  );
}
