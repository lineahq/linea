import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, type FleetAgentQuery } from '../../api/client';
import type { FleetAgent } from '../../api/types';
import { availabilityChip, isCapturable, locationText } from '../../capture/collectorView';
import { Icon } from '../../ui/Icon';
import { agentName, agentPlace, configChip, healthChip, relTime, shortHex } from '../fleetView';
import { ChipView, FleetGate, FleetHead, Pager, Problem, useCursors } from './FleetHead';
import { Permitted } from '../../ui/Permitted';
import { useDenial } from '../../ui/session';

type Tri = '' | 'true' | 'false';
const tri = (v: Tri) => (v === '' ? undefined : v === 'true');

/**
 * AgentsPage is fleet's one list of Collectors. Each agent shows its
 * health and config state; agents whose Collectors run the livecapture
 * extension are marked, and capture starts from them.
 */
export function AgentsPage() {
  const pages = useCursors();
  const [connected, setConnected] = useState<Tri>('');
  const [healthy, setHealthy] = useState<Tri>('');
  const [selectorText, setSelectorText] = useState('');
  const [selector, setSelector] = useState('');
  const [focus, setFocus] = useState<string>();
  const q: FleetAgentQuery = { cursor: pages.cursor, connected: tri(connected), healthy: tri(healthy), selector };
  const agents = useQuery({ queryKey: ['fleet-agents', q], queryFn: () => api.fleetAgents(q), refetchInterval: 5000, refetchIntervalInBackground: false });
  const items = agents.data?.items ?? [];

  return (
    <>
      <FleetHead />
      <FleetGate>
        <div style={{ display: 'flex', flex: 1, minHeight: 0 }}>
          <div className="od-page" style={{ flex: 1, display: 'flex', flexDirection: 'column', gap: 12 }}>
            <form
              style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}
              onSubmit={(e) => { e.preventDefault(); pages.reset(); setSelector(selectorText.trim()); }}
            >
              <div className="od-search" style={{ width: 320 }}>
                <Icon name="search" size={14} />
                <input aria-label="Agent selector" placeholder="Selector, such as service.name=gateway" value={selectorText} onChange={(e) => setSelectorText(e.target.value)} />
              </div>
              <select className="od-select" aria-label="Connection filter" value={connected} onChange={(e) => { pages.reset(); setConnected(e.target.value as Tri); }}>
                <option value="">Connection: any</option>
                <option value="true">Connected</option>
                <option value="false">Offline</option>
              </select>
              <select className="od-select" aria-label="Health filter" value={healthy} onChange={(e) => { pages.reset(); setHealthy(e.target.value as Tri); }}>
                <option value="">Health: any</option>
                <option value="true">Healthy</option>
                <option value="false">Unhealthy</option>
              </select>
              <button type="submit" className="od-btn od-btn--sm">Apply selector</button>
            </form>
            {agents.error && <Problem title="Could not load agents" error={agents.error} retry={() => { pages.reset(); void agents.refetch(); }} />}
            {agents.isPending && <p role="status" className="od-muted">Loading agents…</p>}
            {agents.data && (
              <div className="od-card" style={{ overflow: 'hidden' }}>
                <table className="od-table">
                  <thead>
                    <tr><th>Agent</th><th>Runs in</th><th>Health</th><th>Config</th><th>Live capture</th><th>Last seen</th></tr>
                  </thead>
                  <tbody>
                    {items.map((a) => <Row key={a.id} a={a} focused={focus === a.id} onFocus={() => setFocus(a.id)} />)}
                    {!items.length && (
                      <tr><td colSpan={6} className="od-subtle" style={{ textAlign: 'center', padding: 24 }}>
                        {selector || connected || healthy ? 'No agent matches these filters.' : 'No agent has connected to fleet yet.'}
                      </td></tr>
                    )}
                  </tbody>
                </table>
                <Pager c={pages} next={agents.data.next_cursor} noun="agents" />
              </div>
            )}
          </div>
          {focus && <AgentPane id={focus} onClose={() => setFocus(undefined)} />}
        </div>
      </FleetGate>
    </>
  );
}

function Row({ a, focused, onFocus }: { a: FleetAgent; focused: boolean; onFocus: () => void }) {
  const linked = a.collector_ids?.length ?? 0;
  return (
    <tr aria-selected={focused}>
      <td>
        <button type="button" className="od-btn od-btn--link od-btn--sm" aria-label={`Show details of ${agentName(a)}`} onClick={onFocus}>{agentName(a)}</button>
        <div className="mono od-subtle" style={{ fontSize: 11 }}>{shortHex(a.id)}</div>
      </td>
      <td className="mono">{agentPlace(a) || '—'}</td>
      <td><ChipView chip={healthChip(a)} /></td>
      <td><ChipView chip={configChip(a.remote_config)} /></td>
      <td>{linked ? <span className="od-chip od-chip--pass"><Icon name="radio" size={12} />{linked} Collector{linked === 1 ? '' : 's'}</span> : <span className="od-subtle">—</span>}</td>
      <td className="od-subtle">{a.connected ? 'now' : relTime(a.last_seen_at ?? a.disconnected_at)}</td>
    </tr>
  );
}

/** AgentPane is one agent: health, components, config state and the Collectors capture can use. */
function AgentPane({ id, onClose }: { id: string; onClose: () => void }) {
  const qc = useQueryClient();
  const detail = useQuery({ queryKey: ['fleet-agent', id], queryFn: () => api.fleetAgent(id), refetchInterval: 5000, refetchIntervalInBackground: false });
  const rolloutDenied = useDenial('fleet.rollout');
  const retry = useMutation({
    mutationFn: () => api.retryFleetAgentConfig(id),
    onSuccess: () => { void qc.invalidateQueries({ queryKey: ['fleet-agent', id] }); void qc.invalidateQueries({ queryKey: ['fleet-agents'] }); },
  });
  const a = detail.data;
  const cfg = a?.config;
  const unhealthy = (a?.components ?? []).filter((c) => c.healthy === false);
  return (
    <aside className="od-pane" style={{ width: 400, flex: 'none', borderLeft: '1px solid var(--border)' }} aria-label="Agent details">
      <div className="od-pane__head">
        <span className="od-pane__title">Agent</span>
        <span style={{ flex: 1 }} />
        <button type="button" className="od-btn od-btn--ghost od-btn--sm od-btn--icon" aria-label="Close details" onClick={onClose}><Icon name="x" size={14} /></button>
      </div>
      <div className="od-pane__body" style={{ padding: 14, display: 'flex', flexDirection: 'column', gap: 14 }}>
        {detail.isPending && <p role="status" className="od-muted">Loading agent…</p>}
        {detail.error && <Problem title="Could not load this agent" error={detail.error} retry={() => void detail.refetch()} />}
        {a && (
          <>
            <div>
              <div className="t-code-strong">{agentName(a)}</div>
              <div className="mono od-muted" style={{ fontSize: 11.5 }}>{a.id}</div>
            </div>
            <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
              <ChipView chip={healthChip(a)} />
              <ChipView chip={configChip(a.remote_config)} />
              {a.identity_conflict && <span className="od-chip od-chip--fail"><Icon name="triangle-alert" size={12} />Identity conflict</span>}
            </div>
            {a.health?.error && <p className="od-callout od-callout--warn" style={{ margin: 0 }}>{a.health.error}</p>}
            <dl className="od-kv">
              {Object.entries(a.attributes ?? {}).sort(([x], [y]) => x.localeCompare(y)).map(([k, v]) => (
                <div key={k} style={{ display: 'contents' }}><dt className="mono">{k}</dt><dd><code>{v}</code></dd></div>
              ))}
              <dt>First seen</dt><dd>{relTime(a.first_seen_at)}</dd>
              <dt>Connected</dt><dd>{a.connected ? `since ${relTime(a.connected_at)}` : `no, last seen ${relTime(a.last_seen_at ?? a.disconnected_at)}`}</dd>
            </dl>

            <div className="od-hsep" />
            <div className="od-section-label">Configuration</div>
            {!cfg && <p className="od-muted" style={{ margin: 0, fontSize: 12.5 }}>Fleet keeps no configuration state for this agent.</p>}
            {cfg && (
              <>
                <dl className="od-kv">
                  <dt>In sync</dt><dd>{cfg.in_sync ? 'Yes' : 'No'}</dd>
                  <dt>Accepts remote config</dt><dd>{cfg.accepts_remote_config ? 'Yes' : 'No'}{cfg.accepts_remote_config && !cfg.credential_remote_config && ' · its credential may not receive config'}</dd>
                  {cfg.desired?.unmanaged && <><dt>Desired</dt><dd>Unmanaged; no assignment matches</dd></>}
                  {(cfg.desired?.layers ?? []).map((l) => (
                    <div key={`${l.name}/${l.sha256}`} style={{ display: 'contents' }}>
                      <dt>Layer</dt>
                      <dd>
                        {l.config_id ? <Link to="/fleet/configs/$configId" params={{ configId: l.config_id }} className="mono">{l.name}</Link> : <code>{l.name}</code>}
                        {l.version !== undefined && <span className="od-subtle"> · v{l.version}</span>}
                      </dd>
                    </div>
                  ))}
                  {!!cfg.missing_components?.length && <><dt>Missing components</dt><dd className="mono">{cfg.missing_components.join(', ')}</dd></>}
                </dl>
                {(cfg.error || cfg.reported?.error) && <p className="od-callout od-callout--fail" style={{ margin: 0 }}>{cfg.error || cfg.reported?.error}</p>}
                <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                  <button type="button" className="od-btn od-btn--sm" disabled={retry.isPending || !a.connected || !!rolloutDenied} title={rolloutDenied} onClick={() => retry.mutate()}>
                    <Icon name="rotate-ccw" size={14} /><span>{retry.isPending ? 'Retrying…' : 'Retry config'}</span>
                  </button>
                  <Link to="/fleet/bisect" search={{ agent: a.id }} className="od-btn od-btn--sm od-btn--ghost">
                    <span>Find the version that broke it</span><Icon name="arrow-right" size={14} />
                  </Link>
                </div>
                {retry.error && <Problem title="Could not retry" error={retry.error} />}
                {retry.data && <p role="status" className="od-muted" style={{ margin: 0, fontSize: 12.5 }}>Fleet sent the config again{retry.data.in_sync ? '; the agent is in sync.' : '.'}</p>}
              </>
            )}

            {!!a.components?.length && (
              <>
                <div className="od-hsep" />
                <div className="od-section-label">Components · {a.components.length}{unhealthy.length ? ` · ${unhealthy.length} unhealthy` : ''}</div>
                <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
                  {a.components.map((c) => (
                    <div key={c.id} style={{ display: 'flex', gap: 8, alignItems: 'center', fontSize: 12.5 }}>
                      <Icon name={c.healthy === false ? 'circle-x' : 'circle-check'} size={12} />
                      <code>{c.id}</code>
                      <span style={{ flex: 1 }} />
                      {c.error ? <span className="od-subtle" title={c.error}>{c.status || 'error'}</span> : <span className="od-subtle">{c.status}</span>}
                    </div>
                  ))}
                </div>
              </>
            )}

            <div className="od-hsep" />
            <div className="od-section-label">Live capture</div>
            {a.capture_linked ? <LinkedCollectors ids={a.collector_ids ?? []} /> : (
              <p className="od-muted" style={{ margin: 0, fontSize: 12.5 }}>None of this agent&apos;s Collectors runs the livecapture extension, so it cannot be captured from.</p>
            )}
          </>
        )}
      </div>
    </aside>
  );
}

/** LinkedCollectors are the agent's Collectors that run the livecapture extension. */
function LinkedCollectors({ ids }: { ids: string[] }) {
  const collectors = useQuery({ queryKey: ['collectors-of', ids], queryFn: () => Promise.all(ids.map((id) => api.collector(id))), refetchInterval: 10000, refetchIntervalInBackground: false });
  const usable = (collectors.data ?? []).filter(isCapturable).map((c) => c.id);
  return (
    <>
      {collectors.error && <Problem title="Could not load the Collectors" error={collectors.error} />}
      {(collectors.data ?? []).map((c) => (
        <div key={c.id} style={{ display: 'flex', gap: 8, alignItems: 'center', fontSize: 12.5, flexWrap: 'wrap' }}>
          <code>{c.pod}</code>
          <span className="od-subtle">{locationText(c)}</span>
          <span style={{ flex: 1 }} />
          <span className="od-subtle">{c.point_count} points</span>
          <ChipView chip={availabilityChip(c)} />
        </div>
      ))}
      {usable.length > 0 && (
        <Permitted perm="capture.start"><Link to="/captures/new" search={{ collectors: usable }} className="od-btn od-btn--primary od-btn--sm" style={{ alignSelf: 'flex-start' }}>
          <Icon name="radio" size={14} /><span>Configure capture</span>
        </Link></Permitted>
      )}
    </>
  );
}
