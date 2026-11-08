import { useMemo } from 'react';
import type { RunView } from '../runs/useRun';
import { runLoadText } from '../runs/deletion';
import { Icon } from '../ui/Icon';
import { assertionRows } from './overview';
import { RecordedChecks } from './RecordedChecks';

/**
 * AssertionsTab is the whole-run Assertions view: each assertion the run
 * checked with its outcome and evidence, then the checks recorded beside the
 * run later. A run that declared none says behavior was not checked.
 */
export function AssertionsTab({ view }: { view: RunView }) {
  const r = view.run;
  const rep = r?.report;
  const rows = useMemo(() => assertionRows(rep?.assertions), [rep]);
  if (!r) {
    return (
      <div className="od-muted" style={{ padding: 14 }} role={view.error ? 'alert' : 'status'}>
        {view.error ? runLoadText(view.error) : 'Loading run…'}
      </div>
    );
  }
  if (view.live) return <div className="od-muted" style={{ padding: 14 }} role="status">Assertions are available when the run finishes.</div>;
  return (
    <div style={{ padding: 14, display: 'flex', flexDirection: 'column', gap: 16 }}>
      {rows.length > 0 && (
        <section style={{ display: 'flex', flexDirection: 'column', gap: 8 }} aria-label="Assertions">
          <h2 className="od-section-label" style={{ margin: 0 }}>Assertions</h2>
          {rows.map((a, i) => (
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
      {!rows.length && rep && rep.result?.status === 'completed' && (
        <div className="od-muted" style={{ fontSize: 12 }}>No assertions: behavior was not checked against expectations.</div>
      )}
      {r.state === 'done' && <RecordedChecks runId={r.id} />}
    </div>
  );
}
