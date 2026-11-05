import { useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import { PageHead } from '../ui/PageHead';
import { Icon } from '../ui/Icon';
import { Pager, Problem, useCursors } from '../fleet/pages/FleetHead';
import { whenText } from '../saved/format';
import { auditQuery, NO_FILTERS, outcomeView, resourcesText, scopeText, type AuditFilters } from './auditView';

/**
 * AuditPage reads the team server's audit log, newest first:
 * one row per operation, filtered by principal, operation and outcome on
 * the server. It needs audit.read; the rail hides it without.
 */
export function AuditPage() {
  const [draft, setDraft] = useState<AuditFilters>(NO_FILTERS);
  const [filters, setFilters] = useState<AuditFilters>(NO_FILTERS);
  const pages = useCursors();
  const events = useQuery({
    queryKey: ['audit', filters, pages.cursor],
    queryFn: () => api.auditEvents(auditQuery(filters, pages.cursor)),
    placeholderData: keepPreviousData,
    retry: false,
  });
  const apply = (f: AuditFilters) => {
    setFilters(f);
    pages.reset();
  };
  const rows = events.data?.events ?? [];
  const filtered = filters.principal || filters.operation || filters.outcome;
  return (
    <>
      <PageHead title="Audit log" />
      <div className="od-page">
        <div className="saved-stack" style={{ maxWidth: 1200 }}>
          <form
            className="audit-filters"
            onSubmit={(e) => {
              e.preventDefault();
              apply(draft);
            }}
          >
            <div className="od-field">
              <label htmlFor="audit-principal">Principal</label>
              <input id="audit-principal" className="od-input" placeholder="user:alice" value={draft.principal} onChange={(e) => setDraft({ ...draft, principal: e.target.value })} />
            </div>
            <div className="od-field">
              <label htmlFor="audit-operation">Operation</label>
              <input id="audit-operation" className="od-input" placeholder="SubmitRun" value={draft.operation} onChange={(e) => setDraft({ ...draft, operation: e.target.value })} />
            </div>
            <div className="od-field">
              <label htmlFor="audit-outcome">Outcome</label>
              <select id="audit-outcome" className="od-select" value={draft.outcome} onChange={(e) => setDraft({ ...draft, outcome: e.target.value as AuditFilters['outcome'] })}>
                <option value="">Any</option>
                <option value="allowed">Allowed</option>
                <option value="denied">Denied</option>
              </select>
            </div>
            <button type="submit" className="od-btn">Filter</button>
            {filtered && (
              <button
                type="button"
                className="od-btn od-btn--ghost"
                onClick={() => {
                  setDraft(NO_FILTERS);
                  apply(NO_FILTERS);
                }}
              >
                Clear
              </button>
            )}
          </form>
          {events.isPending && <div className="od-empty" role="status"><div className="od-empty__title">Loading the audit log…</div></div>}
          {events.error && <Problem title="The audit log could not be read" error={events.error} retry={() => void events.refetch()} />}
          {events.data && rows.length === 0 && (
            <div className="od-card" style={{ borderStyle: 'dashed' }}>
              <div className="od-empty">
                <div className="od-empty__glyph"><Icon name="history" size={18} /></div>
                <div className="od-empty__title">No events</div>
                <div className="od-empty__body">{filtered ? 'No operation matches these filters.' : 'No operation has been recorded yet.'}</div>
              </div>
            </div>
          )}
          {rows.length > 0 && (
            <div className="od-card" style={{ overflow: 'auto' }}>
              <table className="od-table">
                <thead>
                  <tr><th>When</th><th>Principal</th><th>Client</th><th>Operation</th><th>Permission</th><th>Scope</th><th>Resources</th><th>Outcome</th></tr>
                </thead>
                <tbody>
                  {rows.map((e) => {
                    const outcome = outcomeView(e);
                    return (
                      <tr key={e.seq}>
                        <td className="od-subtle" title={e.time}>{whenText(e.time)}</td>
                        <td className="mono" title={e.principal_kind}>{e.principal}</td>
                        <td>{e.client}</td>
                        <td className="mono" title={e.request_id ? `Request ${e.request_id}` : undefined}>{e.operation}</td>
                        <td className="mono">{e.permission || '—'}</td>
                        <td>{scopeText(e.scope)}</td>
                        <td className="mono" title={(e.resources ?? []).join('\n') || undefined}>{resourcesText(e.resources)}</td>
                        <td><span className={`od-chip od-chip--plain od-chip--${outcome.tone}`}>{outcome.label}</span></td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
              <Pager c={pages} next={events.data?.next_cursor} noun="events" />
            </div>
          )}
        </div>
      </div>
    </>
  );
}
