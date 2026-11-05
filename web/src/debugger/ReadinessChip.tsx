import { useRef, useState } from 'react';
import { Icon, type IconName } from '../ui/Icon';
import type { Problem, ProblemFix, Readiness } from './state';
import { useDismiss } from './useDismiss';

const TONE_ICON: Record<Readiness['tone'], IconName> = {
  ok: 'circle-check',
  warn: 'triangle-alert',
  blocked: 'circle-x',
  running: 'loader-circle',
};

/**
 * ReadinessChip says whether the draft can run. With problems it opens the
 * list that names each one and where its remedy is (Prepare-Blocked): the
 * draft is kept, nothing is lost by looking.
 */
export function ReadinessChip({ readiness, onFix }: { readiness: Readiness; onFix(fix: ProblemFix): void }) {
  const [open, setOpen] = useState(false);
  const host = useRef<HTMLDivElement>(null);
  useDismiss(open, host, () => setOpen(false));
  const { tone, label, problems } = readiness;
  const chip = (
    <span className={`od-ready${tone === 'running' ? '' : ` od-ready--${tone}`}`} style={tone === 'running' ? { color: 'var(--accent-fg)' } : undefined}>
      <Icon name={TONE_ICON[tone]} size={14} />
      {label}
    </span>
  );
  if (!problems.length) {
    return (
      <span role="status" aria-live="polite">
        {chip}
      </span>
    );
  }
  const blocked = tone === 'blocked';
  return (
    <div ref={host} style={{ position: 'relative' }}>
      <button
        type="button"
        className="od-btn od-btn--sm od-btn--ghost"
        style={blocked ? { borderColor: 'var(--fail-border)', background: 'var(--fail-bg)', color: 'var(--fail)' } : { color: 'inherit' }}
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
      >
        <span role="status" aria-live="polite" style={{ display: 'contents' }}>{chip}</span>
        <Icon name="chevron-down" size={14} />
      </button>
      {open && (
        <div className="od-menu od-wbhead__menu" style={{ top: 'calc(100% + 4px)', right: 0, width: 470, padding: 0 }} role="dialog" aria-label="Problems that block running">
          <div style={{ padding: '10px 14px', borderBottom: '1px solid var(--border-subtle)', display: 'flex', alignItems: 'center', gap: 8 }}>
            <span className="t-subheading">{label}</span>
            <span style={{ flex: 1 }} />
            <span className="od-subtle" style={{ fontSize: 12 }}>your changes are kept</span>
          </div>
          <div style={{ padding: 10, display: 'flex', flexDirection: 'column', gap: 8 }}>
            {problems.map((p, i) => (
              <ProblemRow key={i} problem={p} onFix={(fix) => { setOpen(false); onFix(fix); }} />
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

function ProblemRow({ problem, onFix }: { problem: Problem; onFix(fix: ProblemFix): void }) {
  return (
    <div className={`od-callout od-callout--${problem.blocking ? 'fail' : 'warn'}`} role="status">
      <Icon name={problem.blocking ? 'circle-x' : 'triangle-alert'} />
      <div className="od-callout__main">
        <div className="od-callout__title">{problem.text}</div>
        {problem.fix && (
          <div className="od-callout__actions">
            <button type="button" className="od-btn od-btn--sm" onClick={() => onFix(problem.fix!)}>
              <span>{problem.fix.label}</span>
            </button>
          </div>
        )}
      </div>
    </div>
  );
}
