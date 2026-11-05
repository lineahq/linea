import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../api/client';
import type { RuntimeStorage } from '../../api/types';
import { Icon } from '../../ui/Icon';
import { formatSize, IMAGE_STATES, problemOf, PRUNE_CACHES_WARNING, pruneQuestion, prunedText, pruneSummary, shortImage } from '../runtimes';
import { RuntimesHead } from './RuntimesHead';
import {
  cleanupRows, cleanupScope, DEFAULT_SELECTION, diskUse, pruneRequest, type CleanupId, type CleanupSelection,
} from './storage';
import './runtimes.css';
import { useDenial } from '../../ui/session';

const SWATCH = { images: 'var(--accent)', caches: 'var(--mode-captured)' } as const;

/**
 * StoragePage reports what runtimes take on Docker's disk and removes what is
 * no longer needed, only after an inline confirmation. Runs, captures
 * and saved work are never touched by it. The measurement takes Docker a few
 * seconds, so the page says it is measuring and offers to measure again.
 */
export function StoragePage() {
  const qc = useQueryClient();
  const storage = useQuery({ queryKey: ['runtime-storage'], queryFn: api.runtimeStorage, retry: false, refetchOnWindowFocus: false });
  const [sel, setSel] = useState<CleanupSelection>(DEFAULT_SELECTION);
  const [confirming, setConfirming] = useState(false);
  const prune = useMutation({
    mutationFn: () => api.pruneRuntimes(pruneRequest(sel)),
    onSuccess: () => {
      setConfirming(false);
      void qc.invalidateQueries({ queryKey: ['runtime-storage'] });
      void qc.invalidateQueries({ queryKey: ['runtimes'] });
    },
  });
  const st = storage.data;
  const failure = storage.error ? problemOf(storage.error) : undefined;
  const choose = (next: CleanupSelection) => { setSel(next); setConfirming(false); };

  return (
    <>
      <RuntimesHead />
      <div className="od-page">
        <div className="rt-storage">
          {storage.isFetching && !st && <p className="od-muted" role="status">Measuring what runtimes take on Docker's disk…</p>}
          {failure && (
            <div className="od-callout od-callout--warn" role="alert">
              <Icon name="triangle-alert" size={16} />
              <div className="od-callout__main">
                <span className="od-callout__title">Storage cannot be measured</span>
                <div className="od-callout__body">{failure.message}</div>
              </div>
              <div className="od-callout__actions">
                <button type="button" className="od-btn od-btn--sm" onClick={() => void storage.refetch()}>Try again</button>
              </div>
            </div>
          )}
          {st && (
            <>
              <DiskUse st={st} measuring={storage.isFetching} onMeasure={() => void storage.refetch()} />
              <Cleanup
                st={st} sel={sel} onSelect={choose} confirming={confirming} onConfirming={(c) => { prune.reset(); setConfirming(c); }}
                pending={prune.isPending} onPrune={() => prune.mutate()}
              />
              {prune.error && (
                <div className="od-callout od-callout--fail" role="alert">
                  <Icon name="circle-x" size={16} />
                  <div className="od-callout__main"><span className="od-callout__title">Clean-up failed</span><div className="od-callout__body">{problemOf(prune.error).message}</div></div>
                </div>
              )}
              {prune.data && (
                <div className="od-callout od-callout--pass" role="status">
                  <Icon name="circle-check" size={16} />
                  <div className="od-callout__main">
                    <span className="od-callout__title">{pruneSummary(prune.data)}</span>
                    {(prune.data.kept ?? []).map((i, n) => <div key={n} className="od-callout__body">Kept {prunedText(i)}</div>)}
                  </div>
                </div>
              )}
              <Images st={st} />
            </>
          )}
        </div>
      </div>
    </>
  );
}

function DiskUse({ st, measuring, onMeasure }: { st: RuntimeStorage; measuring: boolean; onMeasure(): void }) {
  const use = diskUse(st);
  return (
    <section className="od-card" aria-label="Disk use">
      <div className="od-card__head">
        <h2 className="od-card__title">Disk use</h2>
        <span className="rt-grow" />
        <span className="od-mono rt-note">{formatSize(use.total)} used by runtimes</span>
        <button type="button" className="od-btn od-btn--ghost od-btn--sm" disabled={measuring} onClick={onMeasure}>
          <Icon name="rotate-ccw" size={14} /><span>{measuring ? 'Measuring…' : 'Measure again'}</span>
        </button>
      </div>
      <div className="od-card__body rt-diag">
        {use.total > 0 ? (
          <>
            <div className="od-meter" role="img" aria-label={use.summary}>
              {use.segments.filter((s) => s.bytes > 0).map((s) => <span key={s.id} style={{ width: `${s.percent}%`, background: SWATCH[s.id] }} />)}
            </div>
            <div className="rt-legend">
              {use.segments.map((s) => (
                <span key={s.id}><span className="rt-swatch" style={{ background: SWATCH[s.id] }} />{s.label} {formatSize(s.bytes)}</span>
              ))}
            </div>
          </>
        ) : (
          <p className="od-muted rt-note">No runtime images or build caches are stored.</p>
        )}
        <p className="od-muted rt-note">
          Run evidence and captures are stored by the debugger itself and are not part of this measurement. <Link to="/runs/storage">Run history storage</Link> shows what it holds and prunes it.
        </p>
      </div>
    </section>
  );
}

function Cleanup({ st, sel, onSelect, confirming, onConfirming, pending, onPrune }: {
  st: RuntimeStorage; sel: CleanupSelection; onSelect(s: CleanupSelection): void;
  confirming: boolean; onConfirming(c: boolean): void; pending: boolean; onPrune(): void;
}) {
  const manageDenied = useDenial('runtime.manage');
  const rows = cleanupRows(st);
  const scope = cleanupScope(rows, sel);
  const checked = (id: CleanupId) => id === 'containers' || (id === 'replaced' ? sel.replaced : id === 'stale' ? sel.stale : sel.caches);
  const toggle = (id: CleanupId) => {
    if (id === 'replaced') onSelect({ ...sel, replaced: !sel.replaced });
    else if (id === 'stale') onSelect({ ...sel, stale: !sel.stale });
    else if (id === 'caches') onSelect({ ...sel, caches: !sel.caches });
  };
  if (rows.length === 0) {
    return (
      <section className="od-card" aria-label="Clean up">
        <div className="od-card__head"><h2 className="od-card__title">Clean up</h2></div>
        <div className="od-card__body od-muted rt-note">Nothing can be removed: every runtime image is current or in use, and no build cache or stopped container is left.</div>
      </section>
    );
  }
  return (
    <section className="od-card" style={{ overflow: 'hidden' }} aria-label="Clean up">
      <div className="od-card__head">
        <h2 className="od-card__title">Clean up</h2>
        <span className="od-subtle rt-note">choose what to remove · nothing in use is offered</span>
      </div>
      <table className="od-table">
        <thead><tr><th className="cb" /><th>Item</th><th>Why it can go</th><th className="num">Frees</th></tr></thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.id} aria-selected={checked(r.id)}>
              <td className="cb">
                <input
                  type="checkbox" checked={checked(r.id)} disabled={r.fixed} aria-label={r.title}
                  onChange={() => toggle(r.id)}
                />
              </td>
              <td>{r.title}</td>
              <td className="od-subtle">{r.why}</td>
              <td className="num">{r.frees === undefined ? '—' : formatSize(r.frees)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {sel.caches && rows.some((r) => r.id === 'caches') && (
        <div className="od-card__body" style={{ borderTop: '1px solid var(--border-subtle)' }}>
          <div className="od-callout od-callout--warn"><Icon name="triangle-alert" size={16} /><div className="od-callout__main"><div className="od-callout__body">{PRUNE_CACHES_WARNING}</div></div></div>
        </div>
      )}
      {confirming ? (
        <div className="od-card__body" style={{ borderTop: '1px solid var(--border-subtle)' }}>
          <div className="od-confirm" role="alertdialog" aria-label="Clean up runtime storage">
            <Icon name="triangle-alert" size={16} />
            <span className="od-confirm__text">{pruneQuestion(scope)}</span>
            <button type="button" className="od-btn od-btn--sm" onClick={() => onConfirming(false)}>Keep</button>
            <button type="button" className="od-btn od-btn--danger od-btn--sm" disabled={pending} onClick={onPrune}>{pending ? 'Cleaning up…' : 'Clean up'}</button>
          </div>
        </div>
      ) : (
        <div className="od-pager">
          <span>Selected frees <strong>{formatSize(scope.bytes)}</strong> · runs and saved work are not touched</span>
          <button type="button" className="od-btn od-btn--primary od-btn--sm" disabled={scope.empty || !!manageDenied} title={manageDenied} onClick={() => onConfirming(true)}>
            <Icon name="trash-2" size={14} /><span>Clean up {formatSize(scope.bytes)}</span>
          </button>
        </div>
      )}
    </section>
  );
}

/** Images lists every stored runtime image and cache, with each image's state. */
function Images({ st }: { st: RuntimeStorage }) {
  const images = st.runtimes ?? [];
  const caches = st.caches ?? [];
  const leftovers = st.leftovers ?? [];
  if (images.length + caches.length + leftovers.length === 0) return null;
  return (
    <section className="od-card" style={{ overflow: 'hidden' }} aria-label="Images and caches">
      <div className="od-card__head"><h2 className="od-card__title">Images and caches</h2></div>
      <table className="od-table od-table--dense">
        <thead><tr><th>Runtime or cache</th><th>Collector</th><th>State</th><th className="num">Size</th><th>Image</th></tr></thead>
        <tbody>
          {images.map((r) => (
            <tr key={r.image_id}>
              <td className="mono">{r.name}</td>
              <td className="mono">{r.collector_version}</td>
              <td title={IMAGE_STATES[r.state]?.note}>{IMAGE_STATES[r.state]?.label ?? r.state}{r.in_use && <span className="od-subtle"> · in use</span>}</td>
              <td className="num">{formatSize(r.size)}</td>
              <td className="mono">{shortImage(r.image_id)}</td>
            </tr>
          ))}
          {caches.map((v) => (
            <tr key={v.name}>
              <td className="mono">{v.name}</td>
              <td />
              <td>build cache</td>
              <td className="num">{formatSize(v.size)}</td>
              <td />
            </tr>
          ))}
        </tbody>
      </table>
      {leftovers.length > 0 && (
        <div className="od-card__body rt-diag" style={{ borderTop: '1px solid var(--border-subtle)' }}>
          {leftovers.map((c) => (
            <span key={c.name} className="od-muted rt-note">Container <code>{c.name}</code> ({c.state}){c.running ? '; running, so it is kept' : ''}</span>
          ))}
        </div>
      )}
    </section>
  );
}
