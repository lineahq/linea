import { useRef, useState } from 'react';
import type { Sample } from '../api/types';
import { SAMPLE_DRAFTS, samplePatch, useSamples } from '../samples/SamplePicker';
import { Icon } from '../ui/Icon';
import { getWorkspace, updateWorkspace } from '../workspace/store';
import { useReplaceDrafts } from '../workspace/useReplaceDrafts';
import { useDismiss } from './useDismiss';

/**
 * SamplesMenu loads a sample as the configuration and scenario drafts. Unsaved
 * drafts are replaced only after an inline confirmation.
 */
export function SamplesMenu({ onLoaded }: { onLoaded(): void }) {
  const [open, setOpen] = useState(false);
  const host = useRef<HTMLDivElement>(null);
  const replacer = useReplaceDrafts({
    compact: true,
    after: () => {
      setOpen(false);
      onLoaded();
    },
  });
  const close = () => {
    setOpen(false);
    replacer.cancel();
  };
  useDismiss(open, host, close);
  const samples = useSamples();
  const pick = (s: Sample) => {
    const ws = getWorkspace();
    const same = ws.configYAML === s.config_yaml && ws.scenarioYAML === s.scenario_yaml;
    replacer.request(SAMPLE_DRAFTS, `The example “${s.title}”`, () => updateWorkspace(samplePatch(getWorkspace(), s)), { skipAsk: same });
  };
  return (
    <div ref={host} style={{ position: 'relative' }}>
      <button type="button" className="od-btn od-btn--ghost od-btn--sm" aria-haspopup="menu" aria-expanded={open} onClick={() => (open ? close() : setOpen(true))}>
        <Icon name="file-text" size={14} />
        <span>Samples</span>
        <Icon name="chevron-down" size={14} />
      </button>
      {open && (
        <div className="od-menu" style={{ top: 'calc(100% + 4px)', left: 0, width: 360, maxHeight: 420, overflow: 'auto' }} role="menu" aria-label="Samples">
          {replacer.asking ? (
            replacer.confirm
          ) : (
            <>
              <div className="od-menu__label">Start from a sample</div>
              {samples.error && <div className="od-field__error" style={{ padding: '6px 8px' }} role="alert">Samples: {samples.error.message}</div>}
              {!samples.data && !samples.error && <div className="od-muted" style={{ padding: '6px 8px', fontSize: 12.5 }}>Loading samples…</div>}
              {samples.data?.map((s) => (
                <button key={s.id} type="button" role="menuitem" className="od-menu__item" style={{ alignItems: 'flex-start', flexDirection: 'column', gap: 2 }} onClick={() => pick(s)}>
                  <span className="t-body-strong">
                    {s.title}
                    {s.expect === 'fail' && <span className="od-chip od-chip--warn" style={{ marginLeft: 6 }}>has a bug</span>}
                  </span>
                  <span className="od-muted" style={{ fontSize: 12 }}>{s.description}</span>
                </button>
              ))}
            </>
          )}
        </div>
      )}
    </div>
  );
}
