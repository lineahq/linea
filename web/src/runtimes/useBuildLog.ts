import { useEffect, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';
import type { RuntimeBuild } from '../api/types';
import { follow, NOT_FOLLOWED, parseBuildEvent, type BuildReply, type Followed } from './buildFollow';
import { nextLogFrom } from './runtimes';

/** RETRY_MS is how long the stream waits before it is opened again after it failed for good. */
export const RETRY_MS = 2000;

/**
 * useBuildLog follows a build. It reads the build once (its log tail), and
 * while the build has not ended follows it over server-sent events like a
 * run's (EventSource resumes after a dropped connection, with the log line to
 * continue from as the event id). If the browser gives up on the stream, the
 * build is read again, which says whether it is gone, and the stream is
 * reopened from the first line not read. When the build ends the runtime and
 * build lists are refreshed. Leaving, or following another build, closes the
 * stream; it never cancels the build.
 */
export function useBuildLog(id: string | undefined): Followed & { error: unknown } {
  const qc = useQueryClient();
  const [state, setState] = useState<Followed>(NOT_FOLLOWED);
  const [error, setError] = useState<unknown>();
  useEffect(() => {
    setState(NOT_FOLLOWED);
    setError(undefined);
    if (!id) return;
    let stopped = false;
    let source: EventSource | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let current = NOT_FOLLOWED;
    const apply = (reply: BuildReply, data: RuntimeBuild) => {
      current = follow(current, reply, data);
      setState(current);
      if (current.ended) {
        void qc.invalidateQueries({ queryKey: ['runtimes'] });
        void qc.invalidateQueries({ queryKey: ['runtime-builds'] });
      }
    };
    const open = () => {
      source = new EventSource(api.buildEventsURL(id, nextLogFrom(current.log)));
      const on = (reply: BuildReply) => (m: Event) => {
        const b = parseBuildEvent((m as MessageEvent<string>).data);
        if (!b || stopped) return;
        apply(reply, b);
        if (reply === 'end') source?.close();
      };
      source.addEventListener('build', on('build'));
      source.addEventListener('end', on('end'));
      source.addEventListener('error', () => {
        // The browser retries a dropped connection itself; it only closes the
        // stream for a refusal (the build is gone, or not the caller's).
        if (stopped || source?.readyState !== EventSource.CLOSED) return;
        source.close();
        timer = setTimeout(() => void read(), RETRY_MS);
      });
    };
    const read = async () => {
      try {
        const b = await api.runtimeBuild(id, nextLogFrom(current.log));
        if (stopped) return;
        setError(undefined);
        apply('read', b);
        if (!current.ended) open();
      } catch (e) {
        if (!stopped) setError(e);
      }
    };
    void read();
    return () => {
      stopped = true;
      source?.close();
      clearTimeout(timer);
    };
  }, [id, qc]);
  return { ...state, error };
}
