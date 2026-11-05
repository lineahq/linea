import { useContext, type ReactNode } from 'react';
import { ColumnsContext } from './RecordList';
import type { SignalRow } from './signals';

export type GlyphTone = 'keep' | 'drop' | 'mod' | 'route' | 'none';

interface Props {
  open: boolean;
  selected?: boolean;
  /** Dim a record that no longer exists (dropped, not counted). */
  dim?: boolean;
  panelId: string;
  onToggle(): void;
  glyph: string;
  glyphTone: GlyphTone;
  badge?: string;
  badgeTone?: SignalRow['tone'];
  /** Monospace name shown before the title, e.g. a metric name. */
  metric?: string;
  title: string;
  service?: string;
  /** One sentence that answers the view's question. */
  why?: ReactNode;
  pills?: ReactNode;
  dataRecord?: string;
}

/**
 * RecordRow is the one row every telemetry list uses. The whole row is the
 * control: it opens the evidence for the record and nothing else in the list
 * is clickable.
 */
export function RecordRow(p: Props) {
  // In a list with column headings the badge and the service are cells of their
  // own, under the heading that says what they are; otherwise they sit inline.
  const columns = useContext(ColumnsContext);
  const badge = p.badge && <span className={`rv-badge rv-badge-${p.badgeTone ?? 'neutral'}`} title={columns?.badge}>{p.badge}</span>;
  return (
    <button type="button" className={`rv-row${columns ? ' rv-row-tabular' : ''}${p.dim ? ' rv-dim' : ''}${p.selected ? ' rv-selected' : ''}`} aria-expanded={p.open}
      aria-controls={p.open ? p.panelId : undefined} aria-current={p.selected ? 'true' : undefined} data-record={p.dataRecord} onClick={p.onToggle}>
      <span className={`rv-glyph rv-glyph-${p.glyphTone}`} aria-hidden="true">{p.glyph}</span>
      {columns?.badge && <span className="rv-cell rv-cell-badge">{badge}</span>}
      <span className="rv-main">
        <span className="rv-title-line">
          {!columns?.badge && badge}
          {p.metric && <span className="rv-metric">{p.metric}</span>}
          {p.title && <span className="rv-title">{p.title}</span>}
          {!columns?.service && p.service && <span className="rv-service">{p.service}</span>}
        </span>
        {p.why && <span className="rv-why">{p.why}</span>}
      </span>
      {columns?.service && <span className="rv-cell rv-service" title={p.service}>{p.service}</span>}
      {(!columns || columns.result) && <span className="rv-pills">{p.pills}</span>}
      <span className="rv-chevron" aria-hidden="true">›</span>
    </button>
  );
}

export function Pill({ tone, children }: { tone?: 'keep' | 'drop' | 'mod' | 'route'; children: ReactNode }) {
  return <span className={`rv-pill${tone ? ` rv-pill-${tone}` : ''}`}>{children}</span>;
}
