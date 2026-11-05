import { useState } from 'react';
import { Link, useNavigate } from '@tanstack/react-router';
import { useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import type { CaptureSelection, Collector } from '../api/types';
import { CaptureControls, type CaptureView } from './CaptureControls';
import { selectionForPoint } from './selection';
import { groupingLabels, resolveGroupCapture, selectionForGroup, type CollectorGrouping, type GroupCaptureTargets } from './groupCapture';
import './collectors.css';

export function CollectorsPage({ embedded = false, initialView = 'new', onInputsAdded }: { embedded?: boolean; initialView?: 'new' | 'history'; onInputsAdded?: (count: number) => void } = {}) {
  const navigate = useNavigate();
  const added = onInputsAdded ?? (() => { void navigate({ to: '/', search: { editor: 'inputs' } }); });
  const [view, setView] = useState<CaptureView>(initialView); const [pages, setPages] = useState(['']); const [selected, setSelected] = useState<string>();
  const [draft, setDraft] = useState<CaptureSelection>(); const [search, setSearch] = useState(''); const [pointPages, setPointPages] = useState(['']);
  const [groupTargets, setGroupTargets] = useState<GroupCaptureTargets>();
  const [groupLoading, setGroupLoading] = useState(false); const [groupError, setGroupError] = useState('');
  const [grouping, setGrouping] = useState<CollectorGrouping>('cluster');
  const [collapsedGroups, setCollapsedGroups] = useState<Set<string>>(() => new Set());
  const collectors = useQuery({ queryKey: ['collectors', pages.at(-1)], queryFn: () => api.collectors(pages.at(-1)), refetchInterval: 5000, refetchIntervalInBackground: false });
  const collector = useQuery({ queryKey: ['collector', selected], queryFn: () => api.collector(selected!), enabled: !!selected, refetchInterval: 5000, refetchIntervalInBackground: false });
  const points = useQuery({ queryKey: ['capture-points', selected, pointPages.at(-1)], queryFn: () => api.capturePoints(selected!, pointPages.at(-1)), enabled: !!selected, refetchInterval: 5000, refetchIntervalInBackground: false });
  const items = collectors.data?.items ?? [];
  const filtered = items.filter(item => `${item.pod} ${item.cluster} ${item.namespace} ${item.collector_group}`.toLowerCase().includes(search.toLowerCase()));
  const grouped = new Map<string, Collector[]>();
  for (const item of filtered) {
    const value = grouping === 'none' ? '' : item[grouping] || '';
    const members = grouped.get(value) ?? []; members.push(item); grouped.set(value, members);
  }
  const groups = [...grouped].sort(([a], [b]) => !a ? 1 : !b ? -1 : a.localeCompare(b));
  function toggleGroup(key: string) {
    setCollapsedGroups(previous => { const next = new Set(previous); if (next.has(key)) next.delete(key); else next.add(key); return next; });
  }
  function choose(item: Collector) {
    if (groupLoading || item.id === selected && !groupTargets) return;
    setGroupTargets(undefined); setGroupError('');
    setSelected(item.id); setPointPages(['']);
    const empty = selectionForPoint(item.id, { pipeline: 'logs', point: 'ingress', signal: 'logs', sensitivity: 'redacted' });
    setDraft({ ...empty, capture_points: [], signals: [] });
  }
  async function chooseGroup(field: Exclude<CollectorGrouping, 'none'>, value: string) {
    setGroupLoading(true); setGroupError('');
    try {
      const targets = await resolveGroupCapture(field, value);
      setGroupTargets(targets); setSelected(undefined); setPointPages(['']); setDraft(selectionForGroup(targets));
    } catch (error) { setGroupError(error instanceof Error ? error.message : String(error)); }
    finally { setGroupLoading(false); }
  }
  function collectorRow(item: Collector) {
    return <button type="button" className={`capture-collector-row ${(groupTargets?.collectors.some(member => member.id === item.id) || selected === item.id) ? 'is-selected' : ''}`} key={item.id} aria-label={`Select collector ${item.pod}`} aria-pressed={!!groupTargets?.collectors.some(member => member.id === item.id) || selected === item.id} disabled={groupLoading} onClick={() => choose(item)}><span className="capture-collector-row-title"><strong>{item.pod}</strong><span className="capture-row-arrow">→</span></span><span>{[item.cluster, item.namespace].filter(Boolean).join(' / ') || 'Local collector'}</span><span className="capture-collector-row-meta"><span className={`capture-badge ${item.enabled ? 'is-good' : 'is-warning'}`}><span className="capture-status-dot" />{item.enabled ? 'connected' : 'capture disabled'}</span><small>{item.point_count} points</small></span><small>{item.collector_group}</small></button>;
  }
  const pointPaging = (pointPages.length > 1 || points.data?.next_cursor) && <nav aria-label="Capture-point pages"><button disabled={pointPages.length === 1} onClick={() => setPointPages(p => p.slice(0, -1))}>Previous points</button><span>Page {pointPages.length}</span><button disabled={!points.data?.next_cursor} onClick={() => { const next = points.data?.next_cursor; if (next) setPointPages(p => [...p, next]); }}>Next points</button></nav>;
  return <div className={embedded ? "capture-embedded" : "app collectors-app"}>
    {!embedded && <header className="capture-topbar"><Link to="/" className="capture-back">← Open debugger</Link><span className="capture-product">Linea <span>/</span> Live capture</span><span className="capture-connection"><span className="capture-status-dot" />{collectors.data ? collectors.data.enabled ? 'Connected to fleet' : 'Fleet not configured' : 'Checking connection…'}</span></header>}
    <main className="capture-page">{!embedded && <div className="capture-page-heading"><div><span className="capture-eyebrow">Live telemetry</span><h1>Collectors & captures</h1><p>Collect a sample from a running collector and use it to debug your pipeline.</p></div><span className="capture-discovery-summary"><span className="capture-status-dot" />{items.filter(item => item.enabled).length} capturable on this page</span></div>}
      <nav className="capture-tabs" aria-label="Live capture sections">{(embedded ? ([['history', 'Existing captures'], ['new', 'Start new capture']] as const) : ([['new', 'New capture'], ['history', 'Capture history']] as const)).map(([tab, label]) => <button key={tab} aria-current={view === tab ? 'page' : undefined} onClick={() => setView(tab)}>{label}</button>)}</nav>
      <div className={`capture-content ${view === 'new' ? 'is-new' : ''}`}>
        <aside className="capture-panel capture-collector-list" hidden={view !== 'new'} aria-labelledby="collectors-heading"><div className="capture-section-title"><div><span className="capture-eyebrow">1 · Select</span><h2 id="collectors-heading">Collector</h2></div><span className="capture-badge">{items.length}</span></div><label className="capture-search"><span className="capture-sr-only">Find a collector</span><input type="search" placeholder="Find a collector…" value={search} onChange={e => { setSearch(e.target.value); setCollapsedGroups(new Set()); }} /></label>
          {items.length > 0 && <><label className="capture-grouping">Group by<select value={grouping} onChange={e => { setGrouping(e.target.value as CollectorGrouping); setCollapsedGroups(new Set()); }}>{Object.entries(groupingLabels).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></label>{(pages.length > 1 || collectors.data?.next_cursor) && <p className="capture-grouping-help">Grouping and search apply to the current page. Capture group reviews matching collectors across all pages.</p>}</>}
          {groupLoading && <p role="status">Reviewing the whole group…</p>}
          {groupError && <p className="capture-notice is-error" role="alert">{groupError}</p>}
          {collectors.isPending && <p role="status">Loading collectors…</p>}
          {collectors.error && <div className="capture-notice" role="alert"><p>{collectors.error.message}</p><button onClick={() => { setPages(['']); void collectors.refetch(); }}>Restart Collector list</button></div>}
          {collectors.data && !collectors.data.enabled && <div className="capture-empty" role="status"><h3>Live capture is not set up</h3><p>Start the server with <code>--fleet-url</code> and <code>--fleet-token-file</code> to capture through fleet.</p></div>}
          {collectors.data?.enabled && !items.length && <div className="capture-empty" role="status"><h3>Waiting for a collector</h3><p>A collector appears automatically when it connects to fleet with the livecapture extension.</p><Link to="/fleet">Open Fleet agents</Link></div>}
          {items.length > 0 && !filtered.length && <p className="capture-empty">No collectors match this search on the current page.</p>}
          {groups.map(([value, members], index) => {
            const key = JSON.stringify([grouping, value]); const expanded = !collapsedGroups.has(key);
            const label = value || 'Not reported'; const listID = `collector-group-${index}`;
            return grouping === 'none' ? members.map(collectorRow) : <section className="capture-collector-group" key={key} aria-label={`${groupingLabels[grouping]}: ${label}`}>
              <button type="button" className="capture-group-heading" aria-expanded={expanded} aria-controls={listID} onClick={() => toggleGroup(key)}><span aria-hidden="true">{expanded ? '⌄' : '›'}</span><strong>{label}</strong>{members.some(item => item.id === selected || groupTargets?.collectors.some(member => member.id === item.id)) && <span className="capture-group-selected">Selected</span>}<span className="capture-badge" aria-label={`${members.length} collector${members.length === 1 ? '' : 's'}`}>{members.length}</span></button>
              <div id={listID} hidden={!expanded}><button type="button" className="capture-group-action" disabled={groupLoading} aria-label={`Capture group ${groupingLabels[grouping]}: ${label}`} onClick={() => void chooseGroup(grouping, value)}>Capture group <span aria-hidden="true">→</span></button>{members.map(collectorRow)}</div>
            </section>;
          })}
          {(pages.length > 1 || collectors.data?.next_cursor) && <nav aria-label="Collector pages"><button disabled={pages.length === 1} onClick={() => setPages(p => p.slice(0, -1))}>Previous Collectors</button><span>Page {pages.length}</span><button disabled={!collectors.data?.next_cursor} onClick={() => { const next = collectors.data?.next_cursor; if (next) setPages(p => [...p, next]); }}>Next Collectors</button></nav>}
          {collector.data && <details className="capture-disclosure"><summary>Fleet details</summary><dl className="capture-definition"><dt>Collector</dt><dd>{collector.data.collector_version || 'Unknown'}</dd><dt>Distribution</dt><dd>{collector.data.distribution_version || 'Unknown'}</dd><dt>Agent</dt><dd>{collector.data.agent_id || 'Unknown'}</dd><dt>Credential</dt><dd>{collector.data.credential_id}</dd><dt>Generation</dt><dd>{collector.data.generation}</dd><dt>Raw points</dt><dd>{collector.data.raw_point_count ? `${collector.data.raw_point_count} · need raw permission in fleet` : 'None'}</dd></dl></details>}
        </aside>
        <CaptureControls inputOnly={embedded} onInputsAdded={added} groupTargets={groupTargets} draft={draft} collector={collector.data} points={points.data?.items ?? []} view={view} onViewChange={setView} pointPaging={pointPaging} onKeepDraft={(id, group) => { setGroupTargets(group); setSelected(group ? undefined : id); setPointPages(['']); }} pointsLoading={groupLoading || !!selected && (points.isPending || collector.isPending)} pointsError={points.error?.message || collector.error?.message} />
      </div>
    </main>
  </div>;
}
