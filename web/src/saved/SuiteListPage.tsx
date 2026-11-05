import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useQueries, useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import { Icon } from '../ui/Icon';
import { paginate } from './collection';
import { plural, whenText } from './format';
import { SavedHead } from './SavedHead';
import { chipClass, suiteResultBadge } from './suiteView';
import { suiteStatusQuery } from './useSuite';

/**
 * SuiteListPage lists the retained suites, newest first. The list holds
 * admission metadata only, so each visible row reads its suite's status for
 * the result (at most one page of 25 at a time).
 */
export function SuiteListPage() {
  const suites = useQuery({ queryKey: ['suites'], queryFn: api.suites, retry: false });
  const [page, setPage] = useState(1);
  const view = paginate(suites.data ?? [], page);
  const statuses = useQueries({ queries: view.rows.map((s) => suiteStatusQuery(s.id)) });

  return (
    <>
      <SavedHead />
      <div className="od-page">
        <div className="saved-stack" style={{ maxWidth: 1000 }}>
          {suites.isPending && <div className="od-empty" role="status"><div className="od-empty__title">Loading suites…</div></div>}
          {suites.error && (
            <div className="od-callout od-callout--fail" role="alert">
              <Icon name="circle-x" size={16} />
              <div className="od-callout__main">
                <div className="od-callout__title">Suites could not be read</div>
                <div className="od-callout__body">{suites.error.message}</div>
                <div className="od-callout__actions"><button type="button" className="od-btn od-btn--sm" onClick={() => void suites.refetch()}>Try again</button></div>
              </div>
            </div>
          )}
          {suites.data && suites.data.length === 0 && (
            <div className="od-card" style={{ borderStyle: 'dashed' }}>
              <div className="od-empty">
                <div className="od-empty__glyph"><Icon name="bookmark" size={18} /></div>
                <div className="od-empty__title">No suites yet</div>
                <div className="od-empty__body">A suite is assembled from a selection on the Tests filter of Saved work, and runs once. Its result stays here while the server keeps it.</div>
                <div className="od-empty__actions"><Link to="/saved" className="od-btn"><span>Go to Saved work</span><Icon name="arrow-right" size={14} /></Link></div>
              </div>
            </div>
          )}
          {suites.data && suites.data.length > 0 && (
            <div className="od-card" style={{ overflow: 'hidden' }}>
              <table className="od-table">
                <thead>
                  <tr><th>When</th><th>Members</th><th>Configuration</th><th>Result</th><th><span className="sr-only">Actions</span></th></tr>
                </thead>
                <tbody>
                  {view.rows.map((s, i) => {
                    const status = statuses[i]?.data;
                    const badge = suiteResultBadge(s, status);
                    const unadmitted = (s.members ?? []).filter((m) => m.error).length;
                    return (
                      <tr key={s.id}>
                        <td className="od-subtle" title={s.created}>{whenText(s.created)}</td>
                        <td className="num" style={{ textAlign: 'left' }}>
                          {plural(s.members?.length ?? 0, 'scenario')}
                          {unadmitted > 0 && <span className="od-subtle"> · {unadmitted} not started</span>}
                        </td>
                        <td className="mono">{s.config}</td>
                        <td>
                          <span className={chipClass(badge.tone)}><Icon name={badge.icon} size={12} />{badge.label}</span>
                        </td>
                        <td style={{ textAlign: 'right' }}>
                          <Link to="/saved/suites/$suiteId" params={{ suiteId: s.id }} className="od-btn od-btn--link od-btn--sm" aria-label={`Open suite from ${whenText(s.created)}`}>
                            <span>Open</span><Icon name="arrow-right" size={14} />
                          </Link>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
              <div className="od-pager">
                <span>{view.from}–{view.to} of {plural(view.total, 'suite')}</span>
                <span className="saved-pager-actions">
                  <button type="button" className="od-btn od-btn--sm" disabled={view.page <= 1} onClick={() => setPage(view.page - 1)}>
                    <Icon name="chevron-left" size={14} /><span>Previous</span>
                  </button>
                  <button type="button" className="od-btn od-btn--sm" disabled={view.page >= view.pages} onClick={() => setPage(view.page + 1)}>
                    <span>Next</span><Icon name="chevron-right" size={14} />
                  </button>
                </span>
              </div>
            </div>
          )}
        </div>
      </div>
    </>
  );
}
