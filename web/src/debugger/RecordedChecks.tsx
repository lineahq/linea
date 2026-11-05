import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import { Icon } from '../ui/Icon';
import { evaluationRows, type EvaluationRow } from './evaluations';
import { assertionRows } from './overview';
import { whenText } from './runFacts';

/**
 * RecordedChecks lists the assertion checks recorded beside a finished run,
 * such as an agent's over MCP, each opening to its results. Nothing is shown
 * when there are none. The scenario text a check evaluated is never returned.
 */
export function RecordedChecks({ runId }: { runId: string }) {
  const list = useQuery({ queryKey: ['evaluations', runId], queryFn: () => api.runEvaluations(runId), retry: false });
  const rows = evaluationRows(list.data);
  if (list.isError) {
    return <div className="od-muted" style={{ fontSize: 12 }} role="status">Recorded checks could not be read: {list.error.message}</div>;
  }
  if (!rows.length) return null;
  return (
    <section style={{ display: 'flex', flexDirection: 'column', gap: 8 }} aria-label="Recorded checks">
      <h2 className="od-section-label" style={{ margin: 0 }}>Recorded checks</h2>
      <div className="od-muted" style={{ fontSize: 12 }}>Assertions evaluated later against this run’s recorded evidence, without running again.</div>
      {rows.map((r) => <CheckRow key={r.id} runId={runId} row={r} />)}
    </section>
  );
}

function CheckRow({ runId, row }: { runId: string; row: EvaluationRow }) {
  const [open, setOpen] = useState(false);
  const detail = useQuery({ queryKey: ['evaluation', runId, row.id], queryFn: () => api.runEvaluation(runId, row.id), enabled: open, staleTime: Infinity, retry: false });
  const results = assertionRows(detail.data?.result);
  return (
    <details className="detail-disclosure" open={open} onToggle={(e) => setOpen(e.currentTarget.open)}>
      <summary>
        <span className={`od-assert--${row.tone}`} style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
          <Icon name={row.icon} size={14} />
          {row.outcome}
        </span>
        <span className="od-muted"> · {whenText(row.created)} · by {row.by}</span>
      </summary>
      {detail.isPending && <div className="od-muted" style={{ fontSize: 12 }}>Loading…</div>}
      {detail.isError && <div className="od-field__error" role="alert">{detail.error.message}</div>}
      {detail.data && !results.length && <div className="od-muted" style={{ fontSize: 12 }}>The check had no assertions.</div>}
      {results.map((a, i) => (
        <div key={i} className={`od-assert od-assert--${a.outcome === 'error' ? 'fail' : a.outcome}`}>
          <Icon name={a.icon} size={16} />
          <div>
            <div className="od-assert__text">{a.name} <code>{a.target}</code></div>
            {a.evidence.map((e, j) => <div key={j} className="od-assert__evidence">{e}</div>)}
            {!a.evidence.length && <div className="od-assert__evidence">{a.outcome}</div>}
          </div>
        </div>
      ))}
    </details>
  );
}
