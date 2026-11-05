import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../../api/client';
import { runtimeComponents } from '../catalogPicks';

/** SEARCH_FROM is the component count from which the list offers a search. */
const SEARCH_FROM = 12;

/**
 * RuntimeComponents lists the components a runtime was built with, by kind,
 * read from its manifest and named by its release's catalog when the release
 * is verified. Components the catalog lacks are custom ones.
 */
export function RuntimeComponents({ manifest, version, verified }: { manifest: string; version: string; verified: boolean }) {
  const [filter, setFilter] = useState('');
  const catalog = useQuery({
    queryKey: ['collector-catalog', version],
    queryFn: () => api.collectorCatalog(version),
    enabled: verified && !!version,
    staleTime: Infinity,
  });
  // A verified release names its components once its catalog is read.
  if (catalog.isLoading) return <p className="od-subtle rt-note" role="status">Reading its components…</p>;
  const groups = runtimeComponents(manifest, catalog.data);
  const total = groups.reduce((n, g) => n + g.items.length, 0);
  if (!total) return null;
  const f = filter.trim().toLowerCase();
  const shown = groups
    .map((g) => ({ ...g, items: g.items.filter((c) => !f || c.name.toLowerCase().includes(f) || c.module.toLowerCase().includes(f)) }))
    .filter((g) => g.items.length);
  return (
    <section className="rt-components" aria-label="Components">
      <div className="od-section-label">Components · {total} in its manifest</div>
      {total >= SEARCH_FROM && (
        <label className="od-search">
          <input type="search" placeholder="Search components" value={filter} onChange={(e) => setFilter(e.target.value)} aria-label="Search runtime components" />
        </label>
      )}
      {shown.length === 0 && <p className="od-subtle rt-note">No component matches.</p>}
      {shown.map((g) => (
        <details key={g.kind} open className="rt-components__kind">
          <summary><span>{g.kind}</span><span className="od-mono od-subtle">{g.items.length}</span></summary>
          <div className="rt-chips">
            {g.items.map((c) => (
              <span key={c.module} title={c.module} className={`od-chip od-chip--plain od-mono${c.inCatalog === false ? ' od-chip--accent' : ''}`}>
                {c.name}{c.inCatalog === false && <span className="od-subtle"> custom</span>}
              </span>
            ))}
          </div>
        </details>
      ))}
    </section>
  );
}

/** BuiltInComponents lists the built-in runtime's components from its manifest. */
export function BuiltInComponents({ version }: { version: string }) {
  const standard = useQuery({
    queryKey: ['manifest-start', 'built-in', version],
    queryFn: () => api.composeManifest({ version, preset: 'built-in' }),
    staleTime: Infinity,
  });
  if (!standard.data) return null;
  return <RuntimeComponents manifest={standard.data.manifest} version={version} verified />;
}
