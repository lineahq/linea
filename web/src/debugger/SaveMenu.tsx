import { useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import { Icon } from '../ui/Icon';
import { isDirty, kindDraft, kindText, MAX_LABEL, newLabel, saveLabel, type DraftKind } from '../workspace/drafts';
import type { WorkspaceState } from '../workspace/store';
import { useSavedDraft } from '../workspace/useSavedDraft';
import { ExportForCI } from './ExportForCI';
import { useDismiss } from './useDismiss';
import { useDenial } from '../ui/session';

const KIND_NAMES: Record<DraftKind, string> = { configuration: 'configuration', scenario: 'scenario' };

/**
 * SaveMenu is the header's Save: save the open configuration and scenario
 * as revisions on the server (the originals are never modified),
 * and Export for CI, the one download. When the server's saved work cannot
 * be listed, it offers neither and says why.
 */
export function SaveMenu({ ws }: { ws: WorkspaceState }) {
  const [open, setOpen] = useState(false);
  const host = useRef<HTMLDivElement>(null);
  useDismiss(open, host, () => setOpen(false));
  const available = useQuery({ queryKey: ['artifacts', 'configuration'], queryFn: () => api.artifacts('configuration'), retry: false });
  const scenario = ws.scenarioYAML.trim() !== '';
  const dirty = isDirty(ws, 'config') || isDirty(ws, 'scenario');
  return (
    <div ref={host} style={{ position: 'relative' }}>
      <button type="button" className="od-btn" aria-haspopup="dialog" aria-expanded={open} onClick={() => setOpen(!open)}>
        <Icon name="save" />
        <span>Save</span>
        <Icon name="chevron-down" size={14} />
        {dirty && <span className="od-dirty" title="Unsaved changes" />}
      </button>
      {open && (
        <div className="od-menu od-wbhead__menu" style={{ top: 'calc(100% + 4px)', right: 0, width: 380, padding: 0 }} role="dialog" aria-label="Save">
          <div style={{ padding: '12px 14px', display: 'flex', flexDirection: 'column', gap: 10 }}>
            <div className="t-subheading">Save</div>
            {available.isPending ? null : available.data ? (
              <>
                <SaveSection ws={ws} kind="configuration" />
                {scenario && <SaveSection ws={ws} kind="scenario" />}
              </>
            ) : (
              <div className="od-callout od-callout--info" role="status">
                <Icon name="info" />
                <div className="od-callout__main">
                  <div className="od-callout__title">Saved work is unavailable</div>
                  <div className="od-callout__body">
                    This server could not list its saved work. Check that the team server's PostgreSQL and object storage are reachable to save and export for CI.
                  </div>
                </div>
              </div>
            )}
          </div>
          <div className="od-menu__sep" style={{ margin: 0 }} />
          <ExportForCI ws={ws} />
        </div>
      )}
    </div>
  );
}

/**
 * SaveSection saves one draft under the name in its Name field: a new
 * revision of its saved source (a changed name renames it), or as new saved
 * work. The field starts with the current name and follows the chosen mode
 * until it is edited.
 */
function SaveSection({ ws, kind }: { ws: WorkspaceState; kind: DraftKind }) {
  const denied = useDenial('artifact.write');
  const { list, busy, message, conflict, setConflict, save } = useSavedDraft(kind);
  const [asNew, setAsNew] = useState(false);
  const [typed, setTyped] = useState<string>();
  const source = ws.saved?.[kind];
  const meta = source ? list.data?.items?.find((m) => m.id === source.artifactId) : undefined;
  const revises = !!source && !asNew;
  const text = kindText(ws, kind);
  const current = meta?.label || newLabel(ws, kind);
  const fallback = revises ? current : newLabel(ws, kind);
  const label = saveLabel(typed, fallback);
  const renames = revises && !!meta && label !== meta.label;
  const radio = `save-${kind}`;
  const field = `save-${kind}-name`;
  return (
    <fieldset style={{ border: 0, margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 8, minWidth: 0 }} disabled={busy}>
      <legend className="od-section-label" style={{ padding: 0, marginBottom: 6 }}>{KIND_NAMES[kind]}</legend>
      {source && (
        <label className={`od-radio-card${revises ? ' od-radio-card--on' : ''}`}>
          <input type="radio" name={radio} checked={revises} onChange={() => setAsNew(false)} />
          <span>
            <span className="t-body-strong">New revision of <code>{current}</code></span>
            <span className="od-muted" style={{ display: 'block', fontSize: 12 }}>
              {meta ? `Saved work · revision ${meta.generation + 1}. ` : ''}Earlier revisions stay unchanged.
            </span>
          </span>
        </label>
      )}
      <label className={`od-radio-card${!revises ? ' od-radio-card--on' : ''}`}>
        <input type="radio" name={radio} checked={!revises} onChange={() => setAsNew(true)} disabled={!source} />
        <span>
          <span className="t-body-strong">Save as new saved work</span>
          <span className="od-muted" style={{ display: 'block', fontSize: 12 }}>
            It starts its own revision history
          </span>
        </span>
      </label>
      <label htmlFor={field} style={{ display: 'flex', flexDirection: 'column', gap: 4, fontSize: 12 }}>
        <span className="od-muted">Name</span>
        <input id={field} className="od-input" type="text" value={typed ?? fallback} maxLength={MAX_LABEL} spellCheck={false}
          onChange={(e) => setTyped(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter' && text.trim() && !busy && !denied) void save(!revises, label); }} />
        {typed?.trim() && label !== typed.trim() && <span className="od-subtle">Saves as “{label}”</span>}
      </label>
      <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
        <button type="button" className="od-btn od-btn--primary od-btn--sm" disabled={!text.trim() || busy || !!denied} title={denied} onClick={() => void save(!revises, label)}>
          <Icon name="save" size={14} />
          <span>{revises && meta ? `Save revision ${meta.generation + 1}` : revises ? 'Save revision' : 'Save'}</span>
        </button>
        {renames ? <span className="od-subtle" style={{ fontSize: 12 }}>Renames it from “{meta.label}”</span>
          : !isDirty(ws, kindDraft(kind)) && source && <span className="od-subtle" style={{ fontSize: 12 }}>No changes since it was saved</span>}
      </div>
      {conflict && (
        <div className="od-callout od-callout--warn" role="alertdialog" aria-label="Saved work changed">
          <Icon name="triangle-alert" />
          <div className="od-callout__main">
            <div className="od-callout__title">“{conflict.label}” changed since you opened it</div>
            <div className="od-callout__body">
              Its head is revision {conflict.generation}, saved {new Date(conflict.updated).toLocaleString()}. Your changes are kept.
            </div>
            <div className="od-callout__actions">
              <button type="button" className="od-btn od-btn--sm" onClick={() => void save(true, saveLabel(typed, newLabel(ws, kind)))}>Save as new</button>
              <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={() => setConflict(undefined)}>Cancel</button>
            </div>
          </div>
        </div>
      )}
      {message && <span className="od-muted" style={{ fontSize: 12 }} role="status">{message}</span>}
    </fieldset>
  );
}
