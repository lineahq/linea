import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useQuery } from '@tanstack/react-query';
import { api } from '../../api/client';
import { Icon } from '../../ui/Icon';
import { anyLive, captureTitle, collectorsText, sessionsNewestFirst } from '../detailView';
import { clockTime, formatCount, relativeTime } from '../format';
import { capturePhase, chipClass } from '../phase';
import { CaptureHead } from './CaptureHead';
import { Permitted } from '../../ui/Permitted';

/** Captures: every capture session, newest first. Live ones refresh every two seconds. */
export function CapturesPage() {
  const [cursors, setCursors] = useState(['']);
  const cursor = cursors[cursors.length - 1];
  const sessions = useQuery({
    queryKey: ['captures', cursor],
    queryFn: () => api.captures(cursor),
    refetchInterval: (q) => (anyLive(q.state.data?.items) ? 2000 : 10000),
    refetchIntervalInBackground: false,
  });
  const items = sessionsNewestFirst(sessions.data?.items);
  const now = Date.now();
  return (
    <>
      <CaptureHead
        actions={
          <Permitted perm="capture.start"><Link to="/captures/new" className="od-btn od-btn--primary od-btn--sm">
            <Icon name="radio" size={14} />
            <span>New capture</span>
          </Link></Permitted>
        }
      />
      <div className="od-page" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        {sessions.isPending && <p role="status" className="od-muted">Loading captures…</p>}
        {sessions.error && (
          <div className="od-callout od-callout--fail" role="alert">
            <Icon name="circle-x" size={16} />
            <div className="od-callout__main">
              <div className="od-callout__title">Could not load captures</div>
              <div className="od-callout__body">{sessions.error.message}</div>
              <div className="od-callout__actions">
                <button type="button" className="od-btn od-btn--sm" onClick={() => { setCursors(['']); void sessions.refetch(); }}>Try again</button>
              </div>
            </div>
          </div>
        )}
        {sessions.data && !items.length && cursors.length === 1 && (
          <div className="od-card">
            <div className="od-empty">
              <div className="od-empty__glyph"><Icon name="radio" size={20} /></div>
              <div className="od-empty__title">No captures yet</div>
              <div className="od-empty__body">A capture collects a bounded sample of live telemetry from Collectors so you can debug your pipeline with it.</div>
              <div className="od-empty__actions">
                <Permitted perm="capture.start"><Link to="/captures/new" className="od-btn od-btn--primary">
                  <Icon name="radio" size={16} />
                  <span>New capture</span>
                </Link></Permitted>
              </div>
            </div>
          </div>
        )}
        {items.length > 0 && (
          <div className="od-card" style={{ overflow: 'hidden' }}>
            <table className="od-table">
              <thead>
                <tr>
                  <th>Capture</th>
                  <th>State</th>
                  <th>Collectors</th>
                  <th className="num">Records received</th>
                  <th>Started</th>
                </tr>
              </thead>
              <tbody>
                {items.map((s) => {
                  const phase = capturePhase(s);
                  return (
                    <tr key={s.id}>
                      <td className="mono">
                        <Link to="/captures/$captureId" params={{ captureId: s.id }}>{captureTitle(s)}</Link>
                      </td>
                      <td>
                        <span className={chipClass(phase.tone)}>
                          <Icon name={phase.icon} size={12} />
                          {phase.label}
                        </span>
                      </td>
                      <td>{collectorsText(s.target_count)}</td>
                      <td className="num">{formatCount(s.data.received_records)}</td>
                      <td className="od-subtle" title={s.created_at}>{relativeTime(s.created_at, now)} · {clockTime(s.created_at)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
            <div className="od-pager">
              <span>Page {cursors.length} · newest first</span>
              <span style={{ display: 'flex', gap: 4 }}>
                <button type="button" className="od-btn od-btn--ghost od-btn--sm" disabled={cursors.length === 1} onClick={() => setCursors((p) => p.slice(0, -1))}>Previous captures</button>
                <button
                  type="button"
                  className="od-btn od-btn--ghost od-btn--sm"
                  disabled={!sessions.data?.next_cursor}
                  onClick={() => { const next = sessions.data?.next_cursor; if (next) setCursors((p) => [...p, next]); }}
                >
                  Next captures
                </button>
              </span>
            </div>
          </div>
        )}
      </div>
    </>
  );
}
