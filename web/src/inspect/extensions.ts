import type { ComponentMetric, Event, Report } from '../api/types';

/**
 * ActivityLine is one thing a running extension did: it started, it
 * stopped, or it logged an entry.
 */
export interface ActivityLine {
  seq: number;
  what: 'started' | 'stopped' | 'log';
  /** The log level, or "error" for a lifecycle event with an error. */
  level?: string;
  text: string;
  fields?: [string, string][];
}

/** activity lists an extension's lifecycle and log entries in event order. */
export function activity(events: readonly Event[], id: string): ActivityLine[] {
  const out: ActivityLine[] = [];
  for (const e of events) {
    if (e.component_kind !== 'extension' || e.component !== id) continue;
    if (e.kind === 'component.started' || e.kind === 'component.stopped') {
      const what = e.kind === 'component.started' ? 'started' : 'stopped';
      out.push({ seq: e.seq, what, level: e.error ? 'error' : undefined, text: e.error ? `${what}: ${e.error}` : what });
    } else if (e.kind === 'component.log' && e.log) {
      const fields = Object.entries(e.log.fields ?? {}).sort(([a], [b]) => a.localeCompare(b));
      out.push({ seq: e.seq, what: 'log', level: e.log.level, text: e.log.message, fields: fields.length ? fields : undefined });
    }
  }
  return out;
}

/** EndpointLine is one simulated client endpoint of a component and what reached it. */
export interface EndpointLine {
  component: string;
  /** Where the client configuration sits in the component's configuration. */
  path: string;
  protocol: string;
  /** The endpoint the configuration named, never contacted. */
  configured: string;
  received: { signal: string; batches: number; items: number }[];
  /** Calls that were not OTLP exports, refused and counted. */
  refused: { call: string; count: number }[];
  /** The module provider serving the endpoint; absent for the debugger's OTLP sinks. */
  provider?: string;
}

/** simulatedEndpoints joins the endpoints a run simulated with what each received; ref narrows them to one component. */
export function simulatedEndpoints(rep?: Report, ref?: string): EndpointLine[] {
  const res = rep?.result;
  return (res?.simulated_endpoints ?? [])
    .filter((s) => ref === undefined || s.component === ref)
    .map((s) => ({
      component: s.component,
      path: s.path,
      protocol: s.protocol,
      configured: s.endpoint,
      received: (res?.endpoint_outputs ?? [])
        .filter((o) => o.component === s.component && o.endpoint === s.path)
        .map((o) => ({ signal: o.signal, batches: o.batches, items: o.counts.items })),
      refused: s.refused ?? [],
      provider: s.provider || undefined,
    }));
}

/** endpointText says in one sentence what an endpoint received. */
export function endpointText(l: EndpointLine): string {
  const got = l.received.length
    ? l.received.map((r) => `${r.items} ${r.signal} item(s) in ${r.batches} batch(es)`).join(', ')
    : 'nothing';
  const refused = l.refused.length
    ? `; refused ${l.refused.map((r) => `${r.call}${r.count > 1 ? ` ×${r.count}` : ''}`).join(', ')} (not an OTLP export)`
    : '';
  return `${l.provider ? `served by the provider ${l.provider}; ` : ''}received ${got}${refused}.`;
}

/** metricsOf lists the metric points a component recorded about itself, by name. */
export function metricsOf(rep: Report | undefined, kind: string, id: string): ComponentMetric[] {
  return (rep?.result?.component_metrics ?? [])
    .filter((m) => m.component_kind === kind && m.component === id)
    .sort((a, b) => a.name.localeCompare(b.name));
}

/** metricValue words a point's value by its type. */
export function metricValue(m: ComponentMetric): string {
  if (m.type === 'histogram') return `count ${m.count ?? 0}, sum ${m.sum ?? 0}`;
  return String(m.int_value ?? m.value ?? '');
}
