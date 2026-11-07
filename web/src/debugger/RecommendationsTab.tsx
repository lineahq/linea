import { useMemo } from 'react';
import type { WorkspaceSearch } from '../router';
import type { RunView } from '../runs/useRun';
import { Icon } from '../ui/Icon';
import type { Segment } from './findings';
import { recommendationViews } from './recommendations';
import { runLabel } from './runFacts';
import { VerifyRecommendation } from './VerifyRecommendation';

function Text({ parts }: { parts: Segment[] }) {
  return <>{parts.map((p, i) => (typeof p === 'string' ? p : <code key={i}>{p.code}</code>))}</>;
}

/**
 * RecommendationsTab is the Recommendations view of the Investigate left
 * pane: a finished run's `report.recommendations` in report order, worded by
 * `recommendations.ts`. It adds, ranks or hides none, and never applies a
 * change to the draft; a processor order can be verified on the run's input.
 */
export function RecommendationsTab({ view, setSearch }: { view: RunView; setSearch(patch: Partial<WorkspaceSearch>): void }) {
  const r = view.run;
  const recs = useMemo(() => (view.live || !r ? [] : recommendationViews(r.report, runLabel(r.id))), [r, view.live]);
  // The candidate runs the same input, so the original run's time is the estimate.
  const expectedMs = r?.started && r.finished ? Date.parse(r.finished) - Date.parse(r.started) : undefined;
  const open = (node: string) => setSearch({ node, input: undefined, record: undefined, tab: undefined, view: undefined });
  if (!r || view.live) {
    return <div className="od-muted" style={{ padding: 14 }} role="status">Recommendations appear when the run has finished.</div>;
  }
  if (!recs.length) {
    return <div className="od-muted" style={{ padding: 14 }} role="status">No recommendations: this run's evidence suggests no change.</div>;
  }
  return (
    <section style={{ padding: 14, display: 'flex', flexDirection: 'column', gap: 8 }} aria-label="Recommendations">
      <div className="od-muted" style={{ fontSize: 12 }}>Suggestions from this run's evidence. Nothing here changes your configuration; check each against your own traffic.</div>
      {recs.map((x) => (
        <div key={x.id} className="od-card" style={{ padding: '10px 12px', display: 'flex', flexDirection: 'column', gap: 6 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
            <span className={`od-chip od-chip--plain${x.kind.tone === 'warn' ? ' od-chip--warn' : ''}`}>{x.kind.label}</span>
            <span className="t-body-strong">{x.title}</span>
            {x.pipeline && <span className="od-muted" style={{ fontSize: 12 }}>in <code>{x.pipeline}</code></span>}
          </div>
          {x.effect && <div className={`od-effect od-effect--${x.effect.tone}`} style={{ fontSize: 12.5 }}>{x.effect.text}</div>}
          <div style={{ fontSize: 12.5 }}>{x.summary}</div>
          {x.change && <div style={{ fontSize: 12.5 }}><span className="od-muted">Suggested change: </span><Text parts={x.change} /></div>}
          {x.saves && (
            <div className="od-muted" style={{ fontSize: 12.5, display: 'flex', flexDirection: 'column', gap: 4 }}>
              <span>{x.saves}{x.share && <>: {x.share.text}</>}</span>
              {x.share && (
                <div className="od-progress" role="img" aria-label={x.share.text} style={{ maxWidth: 240 }}>
                  <span style={{ width: `${Math.max(x.share.fraction * 100, 1)}%` }} />
                </div>
              )}
            </div>
          )}
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
          {x.verifiable
            ? <VerifyRecommendation key={`${r.id}:${x.id}`} runId={r.id} index={x.index} provenance={x.provenance} expectedMs={expectedMs} />
            : <span className={`od-prov od-prov--${x.provenance.tone}`} title={x.rule}><Icon name="eye" size={12} />{x.provenance.text}</span>}
        </div>
      ))}
    </section>
  );
}
