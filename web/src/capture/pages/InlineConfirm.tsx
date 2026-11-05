import { useEffect, useRef, type ReactNode } from 'react';
import { Icon } from '../../ui/Icon';

/**
 * InlineConfirm asks before a consequential action, in the page, where the
 * action was taken (no modal). Focus starts on the safe choice.
 */
export function InlineConfirm({
  children,
  confirmLabel,
  cancelLabel = 'Cancel',
  danger = false,
  busy = false,
  onConfirm,
  onCancel,
}: {
  children: ReactNode;
  confirmLabel: string;
  cancelLabel?: string;
  danger?: boolean;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const cancel = useRef<HTMLButtonElement>(null);
  useEffect(() => cancel.current?.focus(), []);
  return (
    <div className="od-confirm" role="alertdialog" aria-label={confirmLabel}>
      <Icon name="triangle-alert" size={16} />
      <div className="od-confirm__text">{children}</div>
      <button type="button" ref={cancel} className="od-btn od-btn--sm" disabled={busy} onClick={onCancel}>
        {cancelLabel}
      </button>
      <button type="button" className={`od-btn od-btn--sm ${danger ? 'od-btn--danger' : 'od-btn--primary'}`} disabled={busy} onClick={onConfirm}>
        {busy ? 'Working…' : confirmLabel}
      </button>
    </div>
  );
}
