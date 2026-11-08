import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../api/client';
import type { FleetConnection } from '../../api/types';
import { Icon } from '../../ui/Icon';
import { checkChip, checkTitle, connectionVerdict, durationText } from '../connection';
import { relTime } from '../fleetView';
import { ChipView, FleetHead, Problem } from './FleetHead';

/**
 * ConnectionPage shows the server's fleet connection and checks it on
 * request: reaching fleet with the server's credential, the livecapture
 * plugin and config management. The connection itself is deployment
 * configuration; without one the page says what to set.
 */
export function ConnectionPage() {
  const client = useQueryClient();
  const check = useQuery({ queryKey: ['fleet-connection'], queryFn: () => api.fleetConnection(), refetchOnWindowFocus: false });
  const test = () => {
    void check.refetch();
    void client.invalidateQueries({ queryKey: ['fleet-overview'] });
  };
  const action = check.data?.configured && (
    <button type="button" className="od-btn od-btn--sm" onClick={test} disabled={check.isFetching}>
      <Icon name={check.isFetching ? 'loader-circle' : 'plug'} size={14} /><span>{check.isFetching ? 'Testing…' : 'Test connection'}</span>
    </button>
  );
  return (
    <>
      <FleetHead actions={action || undefined} />
      <div className="od-page" style={{ display: 'flex', flexDirection: 'column', gap: 12, maxWidth: 880 }}>
        {check.isPending && <p role="status" className="od-muted">Checking the fleet connection…</p>}
        {check.error && <Problem title="Could not check the fleet connection" error={check.error} retry={test} />}
        {check.data && (check.data.configured ? <Checks c={check.data} fetching={check.isFetching} /> : <Setup />)}
      </div>
    </>
  );
}

function Checks({ c, fetching }: { c: FleetConnection; fetching: boolean }) {
  const verdict = connectionVerdict(c);
  return (
    <>
      {verdict && (
        <div className={`od-callout od-callout--${verdict.tone}`} role="status" aria-busy={fetching}>
          <Icon name={verdict.tone === 'pass' ? 'circle-check' : verdict.tone === 'warn' ? 'triangle-alert' : 'circle-x'} size={16} />
          <div className="od-callout__main">
            <div className="od-callout__title">{verdict.title}</div>
            <div className="od-callout__body">{verdict.body}</div>
          </div>
        </div>
      )}
      <div className="od-card" style={{ overflow: 'hidden' }}>
        <div className="od-card__head"><h3 className="od-card__title" style={{ margin: 0 }}>Checks</h3></div>
        <table className="od-table">
          <thead><tr><th>Step</th><th>Result</th><th>Details</th><th>Took</th></tr></thead>
          <tbody>
            {(c.checks ?? []).map((s) => (
              <tr key={s.name}>
                <td>{checkTitle(s.name)}</td>
                <td><ChipView chip={checkChip(s)} /></td>
                <td>{s.message}</td>
                <td className="od-subtle">{s.status === 'skipped' ? '' : durationText(s.duration_ms)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="od-card">
        <div className="od-card__head"><h3 className="od-card__title" style={{ margin: 0 }}>Connection</h3></div>
        <div className="od-card__body">
          <dl className="od-kv">
            <dt>Fleet</dt><dd><code>{c.endpoint}</code></dd>
            <dt>Checked</dt><dd>{relTime(c.checked_at)}</dd>
          </dl>
          <p className="od-muted" style={{ marginBottom: 0 }}>
            The server reads the fleet address and its credential at startup. Changing either is a deployment change and needs a restart.
          </p>
        </div>
      </div>
    </>
  );
}

/** Setup says how each deployment connects the server to fleet. */
function Setup() {
  const ways: { title: string; body: string; code: string }[] = [
    { title: 'Helm chart', body: 'Run fleet and live capture in the release; the chart connects the server on its own.', code: 'fleet:\n  deploy:\n    enabled: true' },
    { title: 'Docker Compose', body: 'Add fleet and live capture to the stack on its PostgreSQL; the script connects the server.', code: 'scripts/debugger/compose-fleet.sh' },
    { title: 'A fleet you run yourself', body: 'Start the server with fleet\'s management address and a file holding a fleet admin credential.', code: 'linea-server --fleet-url https://fleet.example:8080 --fleet-token-file ./fleet-token' },
  ];
  return (
    <>
      <div className="od-callout od-callout--info" role="status">
        <Icon name="server" size={16} />
        <div className="od-callout__main">
          <div className="od-callout__title">Fleet is not connected</div>
          <div className="od-callout__body">
            Fleet manages agents, groups, configs and rollouts, and live capture runs through it. The connection is part of the server&apos;s deployment; set it one of these ways, then come back to test it.
          </div>
        </div>
      </div>
      {ways.map((w) => (
        <div key={w.title} className="od-card">
          <div className="od-card__head"><h3 className="od-card__title" style={{ margin: 0 }}>{w.title}</h3></div>
          <div className="od-card__body">
            <p style={{ marginTop: 0 }}>{w.body}</p>
            <pre className="od-code" style={{ padding: '8px 12px' }}>{w.code}</pre>
          </div>
        </div>
      ))}
    </>
  );
}
