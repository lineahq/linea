import { useState } from 'react';
import { CommitInput } from '../scenario/fields';
import type { Header } from '../scenario/scenarioDoc';
import { overriddenDefaults } from './headers';

/**
 * HeadersEditor edits request headers as name = value rows: an input's own,
 * with the scenario's defaults it also sends, or the defaults themselves.
 * Any header name works; which ones matter is the configuration's choice
 * (a batch processor's metadata_keys, for one).
 */
export function HeadersEditor({
  entries,
  defaults,
  onChange,
  label,
}: {
  entries: Header[];
  /** The defaults this input also sends, when editing one input's headers. */
  defaults?: Header[];
  onChange(entries: Header[]): boolean;
  label: string;
}) {
  const [adding, setAdding] = useState(false);
  const replace = (i: number, entry?: Header) => onChange(entry ? entries.map((h, j) => (j === i ? entry : h)) : entries.filter((_, j) => j !== i));
  const shadowed = overriddenDefaults(entries, defaults ?? []);
  return (
    <div className="headers-editor" role="group" aria-label={label}>
      {entries.map(([k, v], i) => (
        <div key={`${i}:${k}`} className="form-line">
          <CommitInput className="header-name mono" value={k} label="Header name" placeholder="name" onCommit={(name) => replace(i, name.trim() ? [name.trim(), v] : undefined)} />
          <span className="muted">:</span>
          <CommitInput className="grow mono" value={v} label={`Value of ${k}`} placeholder="value" onCommit={(value) => replace(i, [k, value])} />
          <button type="button" className="icon" aria-label={`Remove header ${k}`} onClick={() => replace(i)}>
            ✕
          </button>
        </div>
      ))}
      {defaults?.map(([k, v]) => (
        <div key={`default:${k}`} className={`form-line header-default${shadowed.has(k.toLowerCase()) ? ' replaced' : ''}`}>
          <span className="header-name mono">{k}</span>
          <span className="muted">:</span>
          <span className="grow mono">{v}</span>
          <span className="muted small">{shadowed.has(k.toLowerCase()) ? 'replaced by this input' : 'from every input'}</span>
        </div>
      ))}
      {adding ? (
        <NewHeader
          onCommit={(name, value) => {
            setAdding(false);
            if (name.trim()) onChange([...entries, [name.trim(), value]]);
          }}
        />
      ) : (
        <button type="button" className="add-check text-link" onClick={() => setAdding(true)}>
          + Header
        </button>
      )}
    </div>
  );
}

function NewHeader({ onCommit }: { onCommit(name: string, value: string): void }) {
  const [name, setName] = useState('');
  const [value, setValue] = useState('');
  const commit = () => onCommit(name, value);
  return (
    <div
      className="form-line"
      onBlur={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget as Node | null)) commit();
      }}
    >
      <input className="header-name mono" autoFocus value={name} placeholder="name" aria-label="New header name" onChange={(e) => setName(e.target.value)} onKeyDown={(e) => e.key === 'Enter' && commit()} />
      <span className="muted">:</span>
      <input className="grow mono" value={value} placeholder="value" aria-label="New header value" onChange={(e) => setValue(e.target.value)} onKeyDown={(e) => e.key === 'Enter' && commit()} />
    </div>
  );
}
