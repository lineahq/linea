import { useEffect, useMemo, useRef, useState, type CSSProperties } from 'react';
import { Link } from '@tanstack/react-router';
import { useQuery } from '@tanstack/react-query';
import { Icon } from '../ui/Icon';
import { runListQuery } from '../runs/runList';
import { firstPageRuns, hasMoreRuns, runCountText } from '../runs/runPages';
import { FactChip, FactPlaceholder } from './FactChip';
import { factsText, isFinished, matchesQuery, runFacts, runLabel, type RunFacts } from './runFacts';

/** How many of the newest runs the drawer lists; "All runs" has the rest. */
export const DRAWER_RUNS = 10;

/** Elements carrying this attribute (the button that opens the drawer) are not "outside": they toggle it themselves. */
export const RUNS_TRIGGER_ATTR = 'data-runs-trigger';

interface Props {
  open: boolean;
  onClose(): void;
  /** The run the debugger shows now: marked "Viewing", and the one a pick compares with. */
  currentRunId?: string;
  /** Start in the mode that picks a run to compare with the current one. */
  picking?: boolean;
}

/**
 * RunsDrawer lists the newest runs beside the debugger. Choosing a run opens
 * it in Investigate (`/?run=<id>`); in the compare mode it opens the
 * comparison (`/?run=<current>&compare=<picked>`). Escape or a click outside
 * closes it, focus moves into it on open and returns to where it was on close.
 *
 * Each row's runtime, scenario, evidence and assertions come from the run
 * list's summary of its report, so opening the drawer reads no run. Execution
 * state, evidence completeness and assertion outcome are separate chips.
 */
export function RunsDrawer({ open, onClose, currentRunId, picking: startPicking = false }: Props) {
  const ref = useRef<HTMLElement>(null);
  const runs = useQuery({ ...runListQuery, enabled: open });
  const [query, setQuery] = useState('');
  const [picking, setPicking] = useState(startPicking);
  useEffect(() => { if (open) setPicking(startPicking); }, [open, startPicking]);

  // Move focus in on open and back on close.
  useEffect(() => {
    if (!open) return;
    const before = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    ref.current?.focus();
    return () => { if (before?.isConnected) before.focus(); };
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const key = (e: KeyboardEvent) => { if (e.key === 'Escape') { e.preventDefault(); onClose(); } };
    const outside = (e: PointerEvent) => {
      const t = e.target;
      if (!(t instanceof Element) || ref.current?.contains(t) || t.closest(`[${RUNS_TRIGGER_ATTR}]`)) return;
      onClose();
    };
    document.addEventListener('keydown', key);
    document.addEventListener('pointerdown', outside);
    return () => { document.removeEventListener('keydown', key); document.removeEventListener('pointerdown', outside); };
  }, [open, onClose]);

  const all = useMemo(() => firstPageRuns(runs.data), [runs.data]);
  const recent = useMemo(() => all.slice(0, DRAWER_RUNS), [all]);
  const rows = recent.map((r) => runFacts(r));
  const shown = rows.filter((f) => matchesQuery(factsText(f), query));
  const current = all.find((r) => r.id === currentRunId);

  if (!open) return null;
  return (
    <aside ref={ref} tabIndex={-1} className="od-drawer" aria-label="Runs" style={{ position: 'fixed', top: 'var(--size-header)', outline: 'none' }}>
      <div className="od-drawer__head">
        <span className="t-heading">Runs</span>
        <span className="od-subtle" style={{ fontSize: 12 }}>newest first</span>
        <span style={{ flex: 1 }} />
        <button type="button" className="od-btn od-btn--ghost od-btn--icon" aria-label="Close runs" onClick={onClose}>
          <Icon name="x" size={16} />
        </button>
      </div>
      <div style={{ padding: '10px 16px', display: 'flex', gap: 8, borderBottom: '1px solid var(--border-subtle)' }}>
        <div className="od-search" style={{ flex: 1 }}>
          <Icon name="search" size={14} />
          <input aria-label="Filter recent runs" placeholder="Filter by scenario, runtime, result" value={query} onChange={(e) => setQuery(e.target.value)} />
        </div>
      </div>
      {picking && currentRunId ? (
        <div style={{ padding: '8px 16px', display: 'flex', alignItems: 'center', gap: 8, background: 'var(--accent-subtle)', borderBottom: '1px solid var(--accent-border)', fontSize: 12.5 }}>
          <Icon name="git-compare" size={14} />
          <span>Pick a run to compare with <strong>{runLabel(currentRunId)}</strong></span>
          <span style={{ flex: 1 }} />
          <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={() => setPicking(false)}><span>Cancel</span></button>
        </div>
      ) : current && isFinished(current) ? (
        <div style={{ padding: '6px 16px', display: 'flex', borderBottom: '1px solid var(--border-subtle)' }}>
          <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={() => setPicking(true)}>
            <Icon name="git-compare" size={14} />
            <span>Compare with {runLabel(current.id)}…</span>
          </button>
        </div>
      ) : null}
      <div style={{ flex: 1, overflow: 'auto' }}>
        {runs.error ? <div className="od-callout od-callout--fail" role="alert" style={{ margin: 12 }}><Icon name="octagon-alert" size={16} /><div className="od-callout__main"><div className="od-callout__title">Could not load the runs</div><div className="od-callout__body">{runs.error.message}</div></div></div>
          : !runs.data ? <div className="od-empty" role="status"><div className="od-empty__body">Loading runs…</div></div>
          : !all.length ? <div className="od-empty"><div className="od-empty__title">No runs yet</div><div className="od-empty__body">Run an input or a scenario; its run appears here.</div></div>
          : !shown.length ? <div className="od-empty" role="status"><div className="od-empty__body">No recent run matches “{query}”.</div></div>
          : shown.map((f) => <DrawerRow key={f.id} facts={f} viewing={f.id === currentRunId} picking={picking && !!currentRunId} currentRunId={currentRunId}
              finished={isFinished(all.find((r) => r.id === f.id)!)} onClose={onClose} />)}
      </div>
      <div className="od-drawer__foot">
        <span className="od-subtle" style={{ fontSize: 12 }}>
          {all.length > recent.length || hasMoreRuns(runs.data) ? `Newest ${recent.length} of ${runCountText(runs.data)} runs` : `${all.length} run${all.length === 1 ? '' : 's'}`}
        </span>
        <span style={{ flex: 1 }} />
        <Link to="/runs" className="od-btn od-btn--sm" onClick={onClose}>
          <span>All runs</span>
          <Icon name="arrow-right" size={14} />
        </Link>
      </div>
    </aside>
  );
}

const ROW: CSSProperties = { display: 'flex', flexDirection: 'column', gap: 6, padding: '10px 16px', borderBottom: '1px solid var(--border-subtle)', color: 'var(--fg)', textDecoration: 'none' };

function DrawerRow({ facts: f, viewing, picking, currentRunId, finished, onClose }: { facts: RunFacts; viewing: boolean; picking: boolean; currentRunId?: string; finished: boolean; onClose(): void }) {
  const style = viewing ? { ...ROW, background: 'var(--bg-selected)', boxShadow: 'inset 2px 0 0 var(--accent)' } : ROW;
  const meta = [f.when, f.scenario, f.runtime].filter(Boolean).join(' · ');
  const body = (
    <>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        <span className="t-code-strong">{f.label}</span>
        <span className="od-subtle" style={{ fontSize: 12, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{meta}</span>
        <span style={{ flex: 1 }} />
        {viewing && <span className="od-chip od-chip--evidence od-chip--plain">Viewing</span>}
      </div>
      <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
        <FactChip fact={f.execution} label="Execution" />
        {f.evidence ? <FactChip fact={f.evidence} label="Evidence" /> : <FactPlaceholder />}
        {f.assertions ? <FactChip fact={f.assertions} label="Assertions" /> : <FactPlaceholder />}
      </div>
    </>
  );
  if (picking && currentRunId) {
    // Only another finished run can be compared with the current one.
    if (viewing || !finished) {
      return <div style={{ ...style, opacity: 0.55 }} aria-disabled="true" title={viewing ? 'This is the run to compare' : 'Compare once the run has finished'}>{body}</div>;
    }
    return <Link to="/" search={{ run: currentRunId, compare: f.id }} style={style} onClick={onClose}>{body}</Link>;
  }
  return <Link to="/" search={{ run: f.id }} style={style} aria-current={viewing ? 'true' : undefined} onClick={onClose}>{body}</Link>;
}
