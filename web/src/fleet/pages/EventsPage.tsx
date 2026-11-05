import { useEffect, useRef, useState } from 'react';
import { api } from '../../api/client';
import type { FleetEvent } from '../../api/types';
import { chipClass } from '../../capture/phase';
import { Icon } from '../../ui/Icon';
import { eventActor, eventText, relTime, shortHex } from '../fleetView';
import { FleetGate, FleetHead, Problem } from './FleetHead';

/** The feed keeps the newest events it has read; older ones fall off. */
const KEEP = 500;
/** One read's page; serve's default page budget allows 100. */
const PAGE = 100;

/**
 * EventsPage follows fleet's event stream: it reads from the cursor
 * fleet returned last, every few seconds while the page is visible, and
 * shows the newest first. Event details stay in fleet; the feed names the
 * kind, the subject and the actor: the debugger user a change was made for
 * when fleet recorded one, else the credential.
 */
export function EventsPage() {
  const [events, setEvents] = useState<FleetEvent[]>([]);
  const [error, setError] = useState<unknown>();
  const [paused, setPaused] = useState(false);
  const cursor = useRef('');
  const [, tick] = useState(0);

  useEffect(() => {
    if (paused) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    const read = async () => {
      try {
        // Drain what is waiting, a page at a time, then wait for more.
        for (let i = 0; i < 10 && !stopped; i++) {
          const page = await api.fleetEvents(cursor.current, PAGE);
          if (stopped) return;
          cursor.current = page.next_cursor;
          const got = page.items ?? [];
          if (got.length) setEvents((prev) => [...got.slice().reverse(), ...prev].slice(0, KEEP));
          if (got.length < PAGE) break;
        }
        setError(undefined);
      } catch (e) {
        if (!stopped) setError(e);
      }
      if (!stopped) timer = setTimeout(() => { if (document.visibilityState === 'visible') void read(); else timer = setTimeout(read, 5000); }, 5000);
    };
    void read();
    const clock = setInterval(() => tick((n) => n + 1), 30000);
    return () => { stopped = true; clearTimeout(timer); clearInterval(clock); };
  }, [paused]);

  const action = (
    <button type="button" className="od-btn od-btn--sm" aria-pressed={paused} onClick={() => setPaused((p) => !p)}>
      <Icon name={paused ? 'play' : 'circle-pause'} size={14} /><span>{paused ? 'Resume feed' : 'Pause feed'}</span>
    </button>
  );
  return (
    <>
      <FleetHead actions={action} />
      <FleetGate>
        <div className="od-page" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          {error !== undefined && <Problem title="Could not read fleet's events" error={error} />}
          <div className="od-card" style={{ overflow: 'hidden' }}>
            <table className="od-table">
              <thead><tr><th>Event</th><th>Subject</th><th>Actor</th><th>When</th></tr></thead>
              <tbody>
                {events.map((e) => {
                  const t = eventText(e);
                  const who = eventActor(e);
                  return (
                    <tr key={e.id}>
                      <td><span className={chipClass(t.tone)}><Icon name={t.icon} size={12} />{t.label}</span></td>
                      <td className="mono" title={e.subject_id}>{shortHex(e.subject_id)}</td>
                      <td className={e.acting_for ? undefined : 'mono od-subtle'} title={who.title}>{who.label}</td>
                      <td className="od-subtle" title={e.occurred_at}>{relTime(e.occurred_at)}</td>
                    </tr>
                  );
                })}
                {!events.length && <tr><td colSpan={4} className="od-subtle" style={{ textAlign: 'center', padding: 24 }}>{paused ? 'The feed is paused.' : 'Waiting for fleet events…'}</td></tr>}
              </tbody>
            </table>
            <div className="od-pager"><span>{events.length} event{events.length === 1 ? '' : 's'} shown, newest first{events.length >= KEEP ? `; older ones are dropped after ${KEEP}` : ''}</span></div>
          </div>
        </div>
      </FleetGate>
    </>
  );
}
