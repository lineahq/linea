import { useState } from 'react';

/** CommitInput is a text input that commits on blur or Enter, not per keystroke. */
export function CommitInput({
  value,
  onCommit,
  placeholder,
  className,
  label,
  title,
}: {
  value: string;
  onCommit(v: string): void;
  placeholder?: string;
  className?: string;
  label?: string;
  title?: string;
}) {
  const [draft, setDraft] = useState(value);
  return (
    <input
      className={className}
      value={draft}
      placeholder={placeholder}
      aria-label={label}
      title={title}
      onChange={(e) => setDraft(e.target.value)}
      onKeyDown={(e) => e.key === 'Enter' && e.currentTarget.blur()}
      onBlur={() => draft !== value && onCommit(draft)}
    />
  );
}

/** TextBlock edits a block of text; it commits on blur. */
export function TextBlock({ label, text, onCommit, rows }: { label: string; text: string; onCommit(v: string): void; rows?: number }) {
  const [draft, setDraft] = useState(text);
  return (
    <textarea
      className="mono conditions"
      rows={rows ?? Math.min(12, Math.max(2, draft.split('\n').length))}
      value={draft}
      aria-label={label}
      placeholder={label}
      spellCheck={false}
      onChange={(e) => setDraft(e.target.value)}
      onBlur={() => draft !== text && onCommit(draft)}
    />
  );
}

/** Section is a titled part of a form, with optional actions beside the title. */
export function Section({
  title,
  status,
  actions,
  children,
  hint,
}: {
  title: React.ReactNode;
  status?: React.ReactNode;
  actions?: React.ReactNode;
  hint?: React.ReactNode;
  children?: React.ReactNode;
}) {
  return (
    <section className="form-section">
      <header>
        <h3>{title}</h3>
        {status}
        {actions && <div className="section-actions">{actions}</div>}
      </header>
      {hint && <p className="hint">{hint}</p>}
      {children}
    </section>
  );
}

export interface MenuItem {
  label: React.ReactNode;
  /** A second line that says what the item does. */
  detail?: React.ReactNode;
  onSelect(): void;
  disabled?: boolean;
  /** Starts a new group under this heading. */
  group?: string;
}

/**
 * Menu is a button that opens a list of actions. It closes on a choice,
 * Escape or a click outside.
 */
export function Menu({
  label,
  items,
  className,
  title,
  align = 'left',
  disabled,
}: {
  label: React.ReactNode;
  items: MenuItem[];
  className?: string;
  title?: string;
  align?: 'left' | 'right';
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  return (
    <span className="menu" onKeyDown={(e) => e.key === 'Escape' && setOpen(false)}>
      <button type="button" className={className} aria-haspopup="menu" aria-expanded={open} title={title} disabled={disabled} onClick={() => setOpen(!open)}>
        {label}
      </button>
      {open && (
        <>
          <div className="backdrop" onClick={() => setOpen(false)} />
          <div className={`popover menu-list ${align}`} role="menu">
            {items.map((it, i) => (
              <div key={i}>
                {it.group && <div className="menu-group">{it.group}</div>}
                <button
                  type="button"
                  role="menuitem"
                  disabled={it.disabled}
                  autoFocus={i === 0}
                  onClick={() => {
                    setOpen(false);
                    it.onSelect();
                  }}
                >
                  <span>{it.label}</span>
                  {it.detail && <span className="muted small">{it.detail}</span>}
                </button>
              </div>
            ))}
          </div>
        </>
      )}
    </span>
  );
}
