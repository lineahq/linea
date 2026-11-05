import { Link } from '@tanstack/react-router';
import type { Graph } from '../api/types';
import type { LayoutNode } from '../graph/layout';
import { Icon } from '../ui/Icon';
import { howItRuns } from './componentConfig';

/**
 * HowItRunsView is the Prepare view of a selected component: how the next
 * run will execute it (real code, mocked environment, captured output, or a
 * stand-in), which pipelines it sits in and whether the selected runtime has
 * it. It states execution modes only; nothing here was observed.
 */
export function HowItRunsView({ node, graph }: { node: LayoutNode; graph?: Graph }) {
  const c = node.component;
  const h = howItRuns(c, graph);
  return (
    <div style={{ display: 'grid', gridTemplateColumns: 'minmax(0, 1fr) minmax(0, 1fr)', gap: 20, padding: '14px 16px' }}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
        {h.warn ? (
          <div className="od-callout od-callout--warn" role="status">
            <Icon name="triangle-alert" size={16} />
            <div className="od-callout__main">
              <div className="od-callout__title">{h.title}</div>
              <div className="od-callout__body">{h.body}</div>
              <div className="od-callout__actions">
                <Link to="/runtimes" className="od-btn od-btn--sm"><span>Choose or add a runtime that has it</span></Link>
              </div>
            </div>
          </div>
        ) : (
          <>
            <div className="t-subheading">{h.title}</div>
            <p className="od-muted" style={{ margin: 0, fontSize: 12.5, lineHeight: '18px' }}>{h.body}</p>
          </>
        )}
        {h.facts.length > 0 && (
          <dl className="od-kv">
            {h.facts.map(([k, v]) => (
              <span key={k} style={{ display: 'contents' }}><dt>{k}</dt><dd><code>{v}</code></dd></span>
            ))}
          </dl>
        )}
      </div>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
        <div className="od-section-label">Pipelines</div>
        {h.pipelines.length === 0 && <span className="od-muted" style={{ fontSize: 12.5 }}>Not used by any pipeline.</span>}
        {h.pipelines.map((p) => (
          <div className="od-path" key={p.id}>
            <span className="od-path__step">{p.id}</span>
            {p.position && (<><span className="od-path__sep">·</span><span className="od-muted" style={{ fontFamily: 'var(--font-sans)' }}>{p.position}</span></>)}
          </div>
        ))}
        <div className="od-section-label" style={{ marginTop: 6 }}>In this runtime</div>
        <div style={{ fontSize: 12.5, display: 'flex', alignItems: 'center', gap: 6 }}>
          <Icon name={h.available.ok ? 'circle-check' : 'triangle-alert'} size={14} />
          <span>{h.available.text}</span>
        </div>
      </div>
    </div>
  );
}
