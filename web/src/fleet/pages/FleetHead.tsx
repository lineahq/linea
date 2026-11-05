import { useState, type ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../../api/client';
import { chipClass } from '../../capture/phase';
import { Icon } from '../../ui/Icon';
import { PageHead, type SubNavItem } from '../../ui/PageHead';
import { problemText, type Chip } from '../fleetView';

/** useFleetOverview is fleet's summary, shared by every Fleet page. */
export function useFleetOverview() {
  return useQuery({ queryKey: ['fleet-overview'], queryFn: () => api.fleetOverview(), refetchInterval: 10000, refetchIntervalInBackground: false });
}

/**
 * FleetHead is the header every Fleet page shares: the title, the section
 * navigation and the page's own actions. Fleet owns everything shown
 * here; the debugger reads and changes it through its fleet connection.
 */
export function FleetHead({ actions }: { actions?: ReactNode }) {
  const overview = useFleetOverview();
  const o = overview.data;
  const nav: SubNavItem[] = [
    { to: '/fleet', label: 'Agents', exact: true, count: o?.enabled ? o.total : undefined },
    { to: '/fleet/groups', label: 'Groups' },
    { to: '/fleet/configs', label: 'Configs' },
    { to: '/fleet/assignments', label: 'Assignments' },
    { to: '/fleet/rollouts', label: 'Rollouts' },
    { to: '/fleet/events', label: 'Events' },
    { to: '/fleet/bisect', label: 'Config bisect' },
  ];
  return <PageHead title="Fleet" nav={nav} actions={actions} />;
}

/** FleetGate shows its children once fleet is connected, and says how to connect it otherwise. */
export function FleetGate({ configs = false, children }: { configs?: boolean; children: ReactNode }) {
  const overview = useFleetOverview();
  if (overview.isPending) return <div className="od-page"><p role="status" className="od-muted">Connecting to fleet…</p></div>;
  if (overview.error) return <div className="od-page"><Problem title="Could not reach fleet" error={overview.error} retry={() => void overview.refetch()} /></div>;
  if (!overview.data.enabled) return <NotConnected />;
  if (configs && !overview.data.config_management) {
    return (
      <div className="od-page">
        <div className="od-card">
          <div className="od-empty" role="status">
            <div className="od-empty__glyph"><Icon name="file-code" size={20} /></div>
            <div className="od-empty__title">Fleet manages no configuration</div>
            <div className="od-empty__body">This fleet runs without config management. Start fleet with config management to keep config versions, assign them and roll them out.</div>
          </div>
        </div>
      </div>
    );
  }
  return <>{children}</>;
}

function NotConnected() {
  return (
    <div className="od-page" style={{ display: 'flex', justifyContent: 'center', paddingTop: 64 }}>
      <div className="od-card" style={{ maxWidth: 640 }}>
        <div className="od-empty" role="status">
          <div className="od-empty__glyph"><Icon name="server" size={20} /></div>
          <div className="od-empty__title">Fleet is not connected</div>
          <div className="od-empty__body">
            Start the server with <code>--fleet-url</code> and <code>--fleet-token-file</code> to manage fleet&apos;s agents, groups, configs and rollouts here. Changes need a fleet credential with the admin role.
          </div>
        </div>
      </div>
    </div>
  );
}

/** ChipView renders a status chip. */
export function ChipView({ chip }: { chip: Chip }) {
  return <span className={chipClass(chip.tone)}><Icon name={chip.icon} size={12} />{chip.label}</span>;
}

/** Problem is a failed load or change, with the fix fleet's refusals call for. */
export function Problem({ title, error, retry, bar = false }: { title: string; error: unknown; retry?: () => void; bar?: boolean }) {
  return (
    <div className={`od-callout od-callout--fail${bar ? ' od-callout--bar' : ''}`} role="alert">
      <Icon name="circle-x" size={16} />
      <div className="od-callout__main">
        <div className="od-callout__title">{title}</div>
        <div className="od-callout__body">{problemText(error)}</div>
        {retry && <div className="od-callout__actions"><button type="button" className="od-btn od-btn--sm" onClick={retry}>Try again</button></div>}
      </div>
    </div>
  );
}

/** useCursors keeps a list's page cursors; the last one is the page shown. */
export function useCursors() {
  const [cursors, setCursors] = useState(['']);
  return {
    cursor: cursors[cursors.length - 1],
    page: cursors.length,
    reset: () => setCursors(['']),
    back: () => setCursors((p) => p.slice(0, -1)),
    next: (c?: string) => { if (c) setCursors((p) => [...p, c]); },
  };
}

/** Pager moves through a cursor list; it hides itself on a single page. */
export function Pager({ c, next, noun }: { c: ReturnType<typeof useCursors>; next?: string; noun: string }) {
  if (c.page === 1 && !next) return null;
  return (
    <div className="od-pager">
      <span>Page {c.page}</span>
      <span style={{ display: 'flex', gap: 4 }}>
        <button type="button" className="od-btn od-btn--ghost od-btn--sm" disabled={c.page === 1} onClick={c.back}>Previous {noun}</button>
        <button type="button" className="od-btn od-btn--ghost od-btn--sm" disabled={!next} onClick={() => c.next(next)}>Next {noun}</button>
      </span>
    </div>
  );
}
