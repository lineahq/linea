import { useEffect, useRef } from 'react';
import { CollectorsPage } from './CollectorsPage';
import './collectors.css';

/** Collection and selection share the same platform operations as /collectors.
 * The debugger draft stays mounted while the user chooses or starts a capture. */
export function CaptureInputPicker({ onClose, onAdded }: { onClose(): void; onAdded(count: number): void }) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const dialog = ref.current!;
    if (typeof dialog.showModal === 'function') dialog.showModal(); else dialog.setAttribute('open', '');
    return () => { if (typeof dialog.close === 'function' && dialog.open) dialog.close(); };
  }, []);
  return <dialog ref={ref} className="capture-input-picker" aria-label="Add input from capture" onClick={event => { if ((event.target as Element).closest('a')?.getAttribute('href')?.startsWith('/')) onClose(); }} onCancel={event => { event.preventDefault(); onClose(); }}>
    <header><div><h2>Add input from capture</h2><p>Choose a capture or start one, select records, then add them to the scenario you are editing.</p></div><button type="button" onClick={onClose} aria-label="Close capture picker">Close</button></header>
    <CollectorsPage embedded initialView="history" onInputsAdded={onAdded} />
    <footer>Closing this window keeps your scenario. A running capture continues until stopped or its duration ends.</footer>
  </dialog>;
}
