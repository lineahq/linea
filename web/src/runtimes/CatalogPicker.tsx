import { useState } from 'react';
import type { CollectorCatalog } from '../api/types';
import { kindCounts, pickerGroups, stabilityText, templateDiff } from './catalogPicks';
import { componentKey } from './runtimes';

/** CHANGES_SHOWN bounds the added and removed chips; the rest are counted. */
const CHANGES_SHOWN = 12;

/**
 * CatalogPicker picks catalog components of a verified release. The start's
 * components arrive preselected (`template`); the summary names what was added
 * or removed since, each change undone by a click, and the list filters by
 * search, kind and "Selected".
 */
export function CatalogPicker({ catalog, picked, onPicked, template, loading }: {
  catalog?: CollectorCatalog;
  picked: ReadonlySet<string>;
  onPicked(s: Set<string>): void;
  template?: { label: string; keys: readonly string[] };
  loading?: string;
}) {
  const [filter, setFilter] = useState('');
  const [kind, setKind] = useState('');
  const [selectedOnly, setSelectedOnly] = useState(false);
  const groups = pickerGroups(catalog, picked, { filter, kind, selectedOnly });
  const counts = kindCounts(catalog, picked);
  const diff = template ? templateDiff(picked, template.keys) : undefined;
  const changed = !!diff && (diff.added.length > 0 || diff.removed.length > 0);
  const base = new Set(template?.keys ?? []);
  const toggle = (key: string) => {
    const next = new Set(picked);
    if (next.has(key)) next.delete(key); else next.add(key);
    onPicked(next);
  };

  return (
    <div className="catalog-picker">
      <div className="catalog-picker__summary" aria-live="polite">
        <strong>{picked.size} {picked.size === 1 ? 'component' : 'components'} selected</strong>
        {template && (
          <span className="od-subtle">
            {template.label}: {template.keys.length}
            {diff?.added.length ? <> · <span className="catalog-picker__added">{diff.added.length} added</span></> : null}
            {diff?.removed.length ? <> · <span className="catalog-picker__removed">{diff.removed.length} removed</span></> : null}
          </span>
        )}
        <span className="rt-grow" />
        {template && changed && (
          <button type="button" className="od-btn od-btn--sm" onClick={() => onPicked(new Set(template.keys))}>Reset to {template.label}</button>
        )}
        {picked.size > 0 && <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={() => onPicked(new Set())}>Clear</button>}
      </div>
      {loading && <p className="od-subtle catalog-picker__status" role="status">{loading}</p>}
      {changed && (
        <div className="catalog-picker__changes" aria-label="Changes to the start">
          {diff!.added.slice(0, CHANGES_SHOWN).map((k) => (
            <button key={k} type="button" className="od-chip od-chip--accent" title="Added: click to remove" onClick={() => toggle(k)}>+ {k} ×</button>
          ))}
          {diff!.removed.slice(0, CHANGES_SHOWN).map((k) => (
            <button key={k} type="button" className="od-chip od-chip--outline catalog-picker__gone" title="Removed: click to add back" onClick={() => toggle(k)}>− {k} ↺</button>
          ))}
          {diff!.added.length + diff!.removed.length > 2 * CHANGES_SHOWN && <span className="od-subtle">and more; show “Selected” to review</span>}
        </div>
      )}
      <div className="catalog-picker__tools">
        <label className="od-search catalog-picker__search">
          <input type="search" placeholder="Search by type or module" value={filter} onChange={(e) => setFilter(e.target.value)} aria-label="Filter components" />
        </label>
        <div className="od-seg" role="group" aria-label="Show">
          <button type="button" aria-pressed={!selectedOnly} onClick={() => setSelectedOnly(false)}>All</button>
          <button type="button" aria-pressed={selectedOnly} onClick={() => setSelectedOnly(true)}>Selected ({picked.size})</button>
        </div>
      </div>
      <div className="catalog-picker__kinds" role="group" aria-label="Kind">
        <button type="button" aria-pressed={kind === ''} onClick={() => setKind('')}>All kinds</button>
        {counts.map((k) => (
          <button key={k.kind} type="button" aria-pressed={kind === k.kind} onClick={() => setKind(kind === k.kind ? '' : k.kind)}>
            {k.kind} <span className="catalog-picker__count">{k.picked}/{k.total}</span>
          </button>
        ))}
      </div>
      <div className="catalog-picker__list">
        {catalog && groups.length === 0 && (
          <p className="od-subtle">{selectedOnly ? 'Nothing selected here yet.' : 'No component matches.'}</p>
        )}
        {groups.map((g) => (
          <section key={g.kind} aria-label={g.kind}>
            <h5 className="catalog-picker__kind">{g.kind} <span className="od-subtle">{g.picked} of {g.total} selected</span></h5>
            <div className="catalog-picker__grid">
              {g.components.map((c) => {
                const key = componentKey(c);
                const on = picked.has(key);
                const stability = stabilityText(c);
                return (
                  <label key={key} title={c.gomod} className={`catalog-item${on ? ' is-on' : ''}${c.unsupported ? ' is-unsupported' : ''}`}>
                    <input type="checkbox" checked={on} disabled={!!c.unsupported && !on} onChange={() => toggle(key)} />
                    <span className="catalog-item__body">
                      <span className="catalog-item__type">
                        {c.type}
                        {template && on && !base.has(key) && <span className="catalog-item__tag">added</span>}
                      </span>
                      <span className="catalog-item__meta">
                        {c.class}{stability && ` · ${stability}`}{c.deprecated_type && ` · was ${c.deprecated_type}`}
                      </span>
                      {c.unsupported && <span className="catalog-item__why">Cannot run in the debugger: {c.unsupported}</span>}
                    </span>
                  </label>
                );
              })}
            </div>
          </section>
        ))}
      </div>
    </div>
  );
}
