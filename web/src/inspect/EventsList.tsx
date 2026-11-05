import { useMemo, useRef, useState } from 'react';
import { useVirtualizer } from '@tanstack/react-virtual';
import type { Event } from '../api/types';
import { eventStep, type Step } from './navigation';

const ROW = 22;

/** EventsList is the run's event stream, virtualized and filterable; an event opens its stage. */
export function EventsList({ events, onStep }: { events: Event[]; onStep(s: Step): void }) {
  const [filter, setFilter] = useState('');
  const shown = useMemo(() => {
    const f = filter.trim().toLowerCase();
    if (!f) return events;
    return events.filter((e) => `${e.kind} ${e.component ?? ''} ${e.pipeline ?? ''} ${e.signal ?? ''} ${e.decision ? decisionText(e) : ''}`.toLowerCase().includes(f));
  }, [events, filter]);
  const scroller = useRef<HTMLDivElement>(null);
  const v = useVirtualizer({ count: shown.length, getScrollElement: () => scroller.current, estimateSize: () => ROW, overscan: 20 });
  return (
    <div className="events">
      <div className="events-head">
        <input type="search" placeholder="Filter events" value={filter} onChange={(e) => setFilter(e.target.value)} aria-label="Filter events" />
        <span className="muted">
          {shown.length} of {events.length}
        </span>
      </div>
      <div className="events-list mono" ref={scroller}>
        <div style={{ height: v.getTotalSize(), position: 'relative' }}>
          {v.getVirtualItems().map((item) => {
            const e = shown[item.index];
            const step = eventStep(e);
            return (
              <div
                key={e.seq}
                className={`event-row${step ? ' clickable' : ''}${e.error ? ' has-error' : ''}`}
                style={{ transform: `translateY(${item.start}px)`, height: ROW }}
                onClick={() => step && onStep(step)}
                title={e.error ?? step?.label}
              >
                <span className="seq">{e.seq}</span>
                <span className="kind">{e.kind}</span>
                <span>{e.component ?? ''}</span>
                <span className="muted">{e.pipeline ?? ''}</span>
                <span className="muted">{e.signal ?? ''}</span>
                <span>{e.decision ? decisionText(e) : e.counts ? `${e.counts.items} items` : ''}</span>
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}

/** decisionText summarizes a sampling decision: a repeated one (a late span of a not-sampled trace) has no snapshot. */
export function decisionText(e: Event): string {
  const d = e.decision!;
  const text = `trace ${d.trace_id.slice(0, 8)} ${d.outcome.replace('_', ' ')}${d.policy ? ` by ${d.policy}` : ''}`;
  return e.snapshot ? `${text} · ${e.counts?.items ?? 0} spans` : `${text} again (late span)`;
}
