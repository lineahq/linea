import type { Marker } from './markers';

export function DiagnosticsList({ markers, onSelect }: { markers: Marker[]; onSelect(m: Marker): void }) {
  if (markers.length === 0) return null;
  return (
    <ul className="diagnostics" aria-label="Diagnostics">
      {markers.map((m, i) => (
        <li key={i} className={`sev-${m.severity}`}>
          <button type="button" onClick={() => onSelect(m)}>
            <span className="badge">{m.severity}</span>
            <span className="loc">
              {m.location.startLine}:{m.location.startColumn}
            </span>
            <span className="msg">{m.message}</span>
          </button>
        </li>
      ))}
    </ul>
  );
}
