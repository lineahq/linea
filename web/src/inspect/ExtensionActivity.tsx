import { useMemo, useState } from 'react';
import type { Event, Report } from '../api/types';
import type { LayoutNode } from '../graph/layout';
import { activity, endpointText, metricValue, metricsOf, simulatedEndpoints } from './extensions';
import { pageOf } from './pages';
import { Pagination } from './Pagination';

/**
 * ExtensionActivity shows what a running extension did: its
 * simulated endpoints and what reached them, its lifecycle and log entries
 * in event order, and the metrics it recorded about itself.
 */
export function ExtensionActivity({ node, events, report, live }: { node: LayoutNode; events: Event[]; report?: Report; live: boolean }) {
  const c = node.component;
  const lines = useMemo(() => activity(events, c.id), [events, c.id]);
  const endpoints = useMemo(() => simulatedEndpoints(report, c.ref), [report, c.ref]);
  const metrics = useMemo(() => metricsOf(report, 'extension', c.id), [report, c.id]);
  const [page, setPage] = useState(0);
  const p = pageOf(lines, page);
  return (
    <div className="details extension-activity">
      {endpoints.length > 0 && (
        <section aria-label="Simulated endpoints">
          <h4>Simulated endpoints</h4>
          <p className="note">Its client endpoints were loopback OTLP sinks; the configured endpoints were never contacted.</p>
          <ul className="limitations">
            {endpoints.map((l) => (
              <li key={l.path}>
                <code>{l.path}</code> ({l.protocol}, configured <code>{l.configured}</code>) {endpointText(l)}
              </li>
            ))}
          </ul>
        </section>
      )}
      <section aria-label="Activity">
        <h4>Activity</h4>
        {lines.length === 0 ? (
          <p className="muted">{live ? 'Nothing recorded yet.' : 'It neither started nor logged anything in this run.'}</p>
        ) : (
          <>
            <ol className="activity">
              {p.shown.map((l) => (
                <li key={l.seq} className={l.level ? `level-${l.level}` : undefined}>
                  <span className="seq mono">{l.seq}</span> <span className="muted">{l.what === 'log' ? l.level : l.what}</span>{' '}
                  {l.what === 'log' && <span className="mono">{l.text}</span>}
                  {l.what !== 'log' && l.level && <span className="error">{l.text}</span>}
                  {l.fields && (
                    <dl className="facts small">
                      {l.fields.map(([k, v]) => <div key={k}><dt>{k}</dt><dd className="mono">{v}</dd></div>)}
                    </dl>
                  )}
                </li>
              ))}
            </ol>
            <Pagination p={p} total={lines.length} noun="entries" setPage={setPage} />
          </>
        )}
      </section>
      {metrics.length > 0 && (
        <section aria-label="Metrics">
          <h4>Metrics</h4>
          <table className="grid">
            <thead>
              <tr><th>Metric</th><th>Attributes</th><th>Type</th><th>Value</th></tr>
            </thead>
            <tbody>
              {metrics.map((m, i) => (
                <tr key={i}>
                  <td className="mono">{m.name}{m.unit ? ` (${m.unit})` : ''}</td>
                  <td className="mono">{Object.entries(m.attributes ?? {}).map(([k, v]) => `${k}=${v}`).join(', ')}</td>
                  <td>{m.type}</td>
                  <td>{metricValue(m)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      )}
    </div>
  );
}
