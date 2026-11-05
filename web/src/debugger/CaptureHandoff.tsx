import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import { takeCaptureHandoff, type CaptureHandoff } from '../capture/handoff';
import { UseRecordsDrawer } from '../capture/pages/UseRecordsDrawer';
import { Icon } from '../ui/Icon';
import { handoffRecordCount } from './handoffView';

/**
 * useCaptureHandoff takes the records a capture page handed to the Debugger
 * (capture/handoff.ts) once, when the Debugger mounts. The selection then
 * lives in this component's state until it is added or dismissed, so opening
 * a configuration from the start state keeps it.
 */
export function useCaptureHandoff() {
  const [handoff, setHandoff] = useState<CaptureHandoff | undefined>(() => takeCaptureHandoff());
  return { handoff, clear: () => setHandoff(undefined) };
}

/**
 * CaptureHandoffBar says which captured records are waiting and adds them to
 * the current draft through the capture page's "Use records" drawer, once a
 * configuration is loaded.
 */
export function CaptureHandoffBar({ handoff, hasConfig, onDone }: { handoff: CaptureHandoff; hasConfig: boolean; onDone(): void }) {
  const [open, setOpen] = useState(false);
  const session = useQuery({ queryKey: ['capture', handoff.captureId], queryFn: () => api.capture(handoff.captureId), retry: false });
  const count = handoffRecordCount(handoff);
  const records = `${count} captured record${count === 1 ? '' : 's'}`;
  return (
    <>
      <div className={`od-callout ${session.error ? 'od-callout--fail' : 'od-callout--info'} od-callout--bar`} role="status">
        <Icon name="file-plus" />
        <div className="od-callout__main">
          <span className="od-callout__title">{records} from capture {handoff.captureId} are waiting</span>
          <span>
            {session.error
              ? `The capture can no longer be read (${session.error.message}). It may have been deleted or expired.`
              : hasConfig
                ? 'Choose a receiver for each stream to add them to the open scenario as inputs.'
                : 'Open or write a configuration first; the records stay waiting in this tab.'}
          </span>
        </div>
        {hasConfig && session.data && (
          <button type="button" className="od-btn od-btn--primary od-btn--sm" onClick={() => setOpen(true)}>
            <span>Add as inputs</span>
          </button>
        )}
        <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={onDone}>
          <span>Dismiss</span>
        </button>
      </div>
      {open && session.data && (
        <UseRecordsDrawer
          session={session.data}
          groups={handoff.streams.map((s) => ({ stream: s.stream, sequences: s.sequences, watermark: s.watermark, receiver: s.receiver ?? '' }))}
          handedOff
          onClose={() => setOpen(false)}
          onAdded={() => {
            setOpen(false);
            onDone();
          }}
        />
      )}
    </>
  );
}
