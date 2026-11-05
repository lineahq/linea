import { useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import { runLogView } from './runLog';

/**
 * RunLogPanel shows a failed or partial run's Collector log tail,
 * environment values already redacted by the server. The caller renders it
 * only for a run that keeps one (keepsRunLog).
 */
export function RunLogPanel({ runId }: { runId: string }) {
  const q = useQuery({ queryKey: ['run-log', runId], queryFn: () => api.runLog(runId), staleTime: Infinity, retry: false });
  if (q.isPending) return null;
  if (q.isError) {
    return <div className="od-muted" style={{ fontSize: 12 }} role="status">The Collector log could not be read: {q.error.message}</div>;
  }
  const v = runLogView(q.data);
  if (!v) return null;
  return (
    <details className="detail-disclosure" open={v.lines > 0}>
      <summary>Collector log <span className="od-muted">· {v.lines} {v.lines === 1 ? 'line' : 'lines'}, environment values redacted</span></summary>
      {v.note && <p className="od-muted" style={{ fontSize: 12 }}>{v.note}</p>}
      {v.text && (
        <pre className="od-code" style={{ margin: 0, padding: '8px 12px', maxHeight: 320, overflow: 'auto', fontSize: 11.5 }} tabIndex={0} aria-label="Collector log">
          {v.text}
        </pre>
      )}
    </details>
  );
}
