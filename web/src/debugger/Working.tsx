import { useEffect, useState } from 'react';
import { elapsedText } from './runFacts';

/**
 * Working is the status of a server request that answers only at the end,
 * such as a comparison: a spinner, what is being done, and the time it has
 * taken so far, so a long wait never looks like a stalled page.
 */
export function Working({ text, detail }: { text: string; detail?: string }) {
  const [start] = useState(() => Date.now());
  const [now, setNow] = useState(start);
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, []);
  return (
    <div className="od-callout od-callout--info" role="status" aria-label={text}>
      <span className="spinner" aria-hidden="true" style={{ marginTop: 3, flex: 'none' }} />
      <div className="od-callout__main">
        <div className="od-callout__body">{text}: {elapsedText(now - start)}</div>
        {detail && <div className="od-callout__body od-muted">{detail}</div>}
      </div>
    </div>
  );
}
