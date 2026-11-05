import { useEffect, useRef, type ReactNode } from 'react';

export function CaptureDialog({ title, children, onCancel, onConfirm, confirmLabel, cancelLabel = 'Cancel', busy = false, danger = false }: {
  title: string; children: ReactNode; onCancel: () => void; onConfirm: () => void; confirmLabel: string; cancelLabel?: string; busy?: boolean; danger?: boolean;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const dialog = ref.current!;
    const nativeModal = typeof dialog.showModal === 'function';
    if (nativeModal) dialog.showModal(); else dialog.setAttribute('open', '');
    return () => { if (nativeModal && dialog.open) dialog.close(); };
  }, []);
  return <dialog className="capture-dialog" ref={ref} role="alertdialog" aria-label={title} onCancel={event => { event.preventDefault(); if (!busy) onCancel(); }}>
    <h3>{title}</h3><div className="capture-dialog-content">{children}</div><div className="capture-actions"><button disabled={busy} onClick={onCancel}>{cancelLabel}</button><button className={danger ? 'capture-danger' : 'primary'} disabled={busy} onClick={onConfirm}>{busy ? 'Working…' : confirmLabel}</button></div>
  </dialog>;
}
