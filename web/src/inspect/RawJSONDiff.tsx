import { useMemo, useState } from 'react';
import { RawJSON, SideBySide } from './DiffViews';
import { pretty } from './records';

interface Props { before: unknown; after?: unknown; afterMissingReason?: string }

/** Always discoverable; mount the complete, offline Monaco diff only on demand. */
export function RawJSONDiff(props: Props) {
  const [open, setOpen] = useState(false);
  return <details className="raw-json-diff" onToggle={(e) => setOpen(e.currentTarget.open)}>
    <summary>Raw JSON diff</summary>
    {open && <RecordedJSON {...props} />}
  </details>;
}

function RecordedJSON({ before, after, afterMissingReason }: Props) {
  const original = useMemo(() => pretty(before), [before]);
  const modified = useMemo(() => pretty(after), [after]);
  const comparable = before !== undefined && after !== undefined;
  return <div className="raw-json-content">
    <p className="muted small">{comparable
      ? 'Complete recorded snapshots · Before → After. Text alignment does not establish record identity.'
      : after === undefined ? afterMissingReason ?? 'No recorded After snapshot is available. Missing data is not treated as a removal.'
      : 'No recorded Before snapshot is available. Missing data is not treated as an addition.'}</p>
    {comparable ? <><div className="raw-json-labels"><strong>Before</strong><strong>After</strong></div><SideBySide before={original} after={modified} /></>
      : <RawJSON before={original} after={modified} />}
  </div>;
}
