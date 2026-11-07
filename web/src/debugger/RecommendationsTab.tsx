import { useMemo } from 'react';
import type { WorkspaceSearch } from '../router';
import type { RunView } from '../runs/useRun';
import { Icon } from '../ui/Icon';
import type { Segment } from './findings';
import { recommendationGroups, recommendationViews, type RecommendationView } from './recommendations';
import { runLabel } from './runFacts';
import { VerifyRecommendation } from './VerifyRecommendation';

function Text({ parts }: { parts: Segment[] }) {
  return <>{parts.map((p, i) => (typeof p === 'string' ? p : <code key={i}>{p.code}</code>))}</>;
}

/**
 * RecommendationsTab is the Recommendations view of the Investigate left
 * pane: a finished run's `report.recommendations`, worded by
 * `recommendations.ts` and grouped by how far they hold: for any input, for
 * this input, or not tested by it; report order within each group. It adds or
 * hides none, and never applies a change to the draft; a change can be
 * verified on the run's input.
 */
export function RecommendationsTab({ view, setSearch }: { view: RunView; setSearch(patch: Partial<WorkspaceSearch>): void }) {
  const r = view.run;
  const recs = useMemo(() => (view.live || !r ? [] : recommendationViews(r.report, runLabel(r.id))), [r, view.live]);
  const groups = useMemo(() => recommendationGroups(recs), [recs]);
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
    <section style={{ padding: 14, display: 'flex', flexDirection: 'column', gap: 14 }} aria-label="Recommendations">
      <div className="od-muted" style={{ fontSize: 12 }}>Suggestions from this run's evidence. Nothing here changes your configuration.</div>
      {groups.map((g) => (
        <section key={g.id} aria-label={g.title} style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
          <div>
            <h3 className="t-body-strong" style={{ margin: 0, fontSize: 13 }}>{g.title} <span className="od-muted" style={{ fontWeight: 400 }}>({g.views.length})</span></h3>
            <div className="od-muted" style={{ fontSize: 12 }}>{g.hint}</div>
          </div>
          {g.views.map((x) => <Card key={x.id} x={x} runId={r.id} expectedMs={expectedMs} open={open} />)}
        </section>
      ))}
    </section>
  );
}

/** Card is one recommendation: its kind, effect, summary, change and evidence. */
function Card({ x, runId, expectedMs, open }: { x: RecommendationView; runId: string; expectedMs?: number; open(node: string): void }) {
  return (
    <div className="od-card" style={{ padding: '10px 12px', display: 'flex', flexDirection: 'column', gap: 6 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
        <span className={`od-chip od-chip--plain${x.kind.tone === 'warn' ? ' od-chip--warn' : ''}`}>{x.kind.label}</span>
        <span className="t-body-strong">{x.title}</span>
        {x.pipeline && <span className="od-muted" style={{ fontSize: 12 }}>in <code>{x.pipeline}</code></span>}
        {x.effect && (
          <span className={`od-chip od-chip--plain od-chip--${x.effect.tone === 'neutral' ? 'outline' : x.effect.tone}`} title={x.effect.text} aria-label={x.effect.text}>
            {x.effect.label}
          </span>
        )}
      </div>
      <div style={{ fontSize: 12.5 }}>{x.summary}</div>
      {x.untested && <div style={{ fontSize: 12.5 }}><span className="od-muted">{x.untested.label}: </span><code>{x.untested.text}</code></div>}
      {x.change && <div style={{ fontSize: 12.5 }} title={x.changeTitle}><span className="od-muted">Change: </span><Text parts={x.change} /></div>}
      {x.saves && (
        <div className="od-muted" style={{ fontSize: 12.5, display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
          <span title={x.share?.title}>{x.saves}{x.share && <> · {x.share.text}</>}</span>
          {x.share && (
            <div className="od-progress" role="img" aria-label={x.share.title} style={{ width: 120 }}>
              <span style={{ width: `${Math.max(x.share.fraction * 100, 1)}%` }} />
            </div>
          )}
        </div>
      )}
      {x.blockers.map((b, i) => <div key={i} className="lim-warning" style={{ fontSize: 12 }}>⚠ {b}</div>)}
      {x.coverage.map((c, i) => <div key={i} className="od-muted" style={{ fontSize: 12 }}>{c}</div>)}
      <div style={{ display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap' }}>
        {x.actions.map((a) => (
          <button key={a.node} type="button" className="od-btn od-btn--link od-btn--sm" onClick={() => open(a.node)}>
            <span>{a.label}</span><Icon name="arrow-right" size={14} />
          </button>
        ))}
        {!x.verifiable && <span className={`od-prov od-prov--${x.provenance.tone}`} title={x.rule}><Icon name="eye" size={12} />{x.provenance.text}</span>}
      </div>
      {x.verifiable && <VerifyRecommendation key={`${runId}:${x.id}`} runId={runId} index={x.index} provenance={x.provenance} expectedMs={expectedMs} checksOnly={x.checksOnly} />}
    </div>
  );
}
