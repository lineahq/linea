import { useState } from 'react';
import { Icon } from '../ui/Icon';
import { confirmText, dirtyDrafts, type Draft } from './drafts';
import { getWorkspace } from './store';

/** A replacement waiting for the user's answer. */
interface Pending {
  dirty: Draft[];
  what: string;
  apply(): void;
}

/**
 * useReplaceDrafts is the one way a view replaces drafts: `request` applies
 * at once when none of the named drafts holds unsaved edits, and otherwise
 * holds the replacement until the user answers the inline confirmation,
 * which renders where the caller puts `confirm`. `after` runs once the
 * replacement is applied, whether or not it was asked. `compact` renders the
 * one-line form for menus and toolbars.
 */
export function useReplaceDrafts({ after, compact = false }: { after?: () => void; compact?: boolean } = {}) {
  const [pending, setPending] = useState<Pending>();
  const request = (drafts: Draft[], what: string, apply: () => void, opts: { skipAsk?: boolean } = {}) => {
    const dirty = dirtyDrafts(getWorkspace(), drafts);
    if (dirty.length && !opts.skipAsk) {
      setPending({ dirty, what, apply });
      return;
    }
    apply();
    after?.();
  };
  const confirm = pending && (
    <ReplaceConfirm
      pending={pending}
      compact={compact}
      onCancel={() => setPending(undefined)}
      onReplace={() => {
        pending.apply();
        setPending(undefined);
        after?.();
      }}
    />
  );
  return { request, confirm, asking: !!pending, cancel: () => setPending(undefined) };
}

/** ReplaceConfirm asks before unsaved work is replaced; keeping it is the default. */
function ReplaceConfirm({ pending, compact, onReplace, onCancel }: { pending: Pending; compact: boolean; onReplace(): void; onCancel(): void }) {
  const keep = 'Keep mine';
  if (compact) {
    return (
      <div className="od-confirm" role="alertdialog" aria-label="Replace unsaved work">
        <Icon name="triangle-alert" size={16} />
        <div className="od-confirm__text">
          {confirmText(pending.dirty)} {pending.what} replaces {pending.dirty.length > 1 ? 'them' : 'it'}.
        </div>
        <button type="button" className="od-btn od-btn--sm" onClick={onReplace}>
          Replace
        </button>
        <button type="button" className="od-btn od-btn--sm od-btn--primary" autoFocus onClick={onCancel}>
          {keep}
        </button>
      </div>
    );
  }
  return (
    <div className="od-callout od-callout--warn" role="alertdialog" aria-label="Replace unsaved work">
      <Icon name="triangle-alert" />
      <div className="od-callout__main">
        <div className="od-callout__title">{confirmText(pending.dirty)}</div>
        <div className="od-callout__body">{pending.what} replaces {pending.dirty.length > 1 ? 'them' : 'it'}. Save or download it first to keep it.</div>
        <div className="od-callout__actions">
          <button type="button" className="od-btn od-btn--sm" onClick={onReplace}>
            Replace
          </button>
          <button type="button" className="od-btn od-btn--sm od-btn--primary" autoFocus onClick={onCancel}>
            {keep}
          </button>
        </div>
      </div>
    </div>
  );
}
