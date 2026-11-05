import { useEffect, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';
import type { End, Event, Run } from '../api/types';
import { runActive } from '../api/runState';

export interface RunView {
  run?: Run;
  /** Events so far: streamed while the run is live, from the report once done. */
  events: Event[];
  live: boolean;
  end?: End;
  error?: Error | null;
}

const finished = (r?: Run) => !!r && !runActive(r.state);

/**
 * useRun follows a run. While it is queued or running, events arrive over
 * SSE (EventSource resumes with Last-Event-ID after a dropped connection)
 * and are batched per animation frame. A run requeued after its worker was
 * lost executes again: "restart" drops the events of the earlier attempt. When the stream ends, the run is
 * refetched with its report, which is then the single source of truth.
 */
export function useRun(id?: string): RunView {
  const qc = useQueryClient();
  const query = useQuery({
    queryKey: ['run', id],
    queryFn: () => api.run(id!),
    enabled: !!id,
    staleTime: (q) => (finished(q.state.data) ? Infinity : 0),
  });
  const run = query.data;
  const [live, setLive] = useState<{ id: string; events: Event[]; end?: End }>({ id: '', events: [] });

  const streaming = !!id && !!run && !finished(run);
  useEffect(() => {
    if (!streaming || !id) return;
    setLive({ id, events: [] });
    const source = new EventSource(api.eventsURL(id));
    let pending: Event[] = [];
    let frame = 0;
    const flush = () => {
      frame = 0;
      const batch = pending;
      pending = [];
      setLive((l) => (l.id === id ? { ...l, events: [...l.events, ...batch] } : l));
    };
    source.addEventListener('event', (m) => {
      pending.push(JSON.parse((m as MessageEvent<string>).data) as Event);
      if (!frame) frame = requestAnimationFrame(flush);
    });
    source.addEventListener('restart', () => {
      pending = [];
      if (frame) cancelAnimationFrame(frame);
      frame = 0;
      setLive((l) => (l.id === id ? { ...l, events: [] } : l));
    });
    source.addEventListener('end', (m) => {
      source.close();
      if (frame) cancelAnimationFrame(frame);
      flush();
      const end = JSON.parse((m as MessageEvent<string>).data) as End;
      setLive((l) => (l.id === id ? { ...l, end } : l));
      void qc.invalidateQueries({ queryKey: ['run', id] });
      void qc.invalidateQueries({ queryKey: ['runs'] });
    });
    return () => {
      source.close();
      if (frame) cancelAnimationFrame(frame);
    };
  }, [streaming, id, qc]);

  const reportEvents = run?.report?.result?.events;
  return {
    run,
    events: reportEvents ?? (live.id === id ? live.events : []),
    live: streaming,
    end: live.id === id ? live.end : undefined,
    error: query.error,
  };
}
