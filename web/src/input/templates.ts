/**
 * Payload templates are small, realistic OTLP JSON payloads to start an
 * input from: the common shapes a pipeline is written for (application
 * logs, container logs, an HTTP trace with a health check, service
 * metrics). Timestamps are set relative to now, so configurations that
 * compare record times with Now() see recent data.
 */

export type Signal = 'logs' | 'traces' | 'metrics';

export interface PayloadTemplate {
  id: string;
  signal: Signal;
  label: string;
  description: string;
  json(now?: number): string;
}

type Attr = { key: string; value: Record<string, unknown> };

/** attrs encodes plain values as OTLP JSON attributes. */
export function attrs(o: Record<string, string | number | boolean>): Attr[] {
  return Object.entries(o).map(([key, v]) => ({
    key,
    value:
      typeof v === 'boolean'
        ? { boolValue: v }
        : typeof v === 'number'
          ? Number.isInteger(v)
            ? { intValue: String(v) }
            : { doubleValue: v }
          : { stringValue: v },
  }));
}

/** nanos is a Unix time in nanoseconds as OTLP JSON writes it, ms before now. */
const nanos = (now: number, msBefore = 0) => `${BigInt(Math.round(now - msBefore)) * 1_000_000n}`;

const pretty = (v: unknown) => JSON.stringify(v, null, 2) + '\n';

const SCOPE = { name: 'example', version: '1.0.0' };

export const PAYLOAD_TEMPLATES: PayloadTemplate[] = [
  {
    id: 'app-logs',
    signal: 'logs',
    label: 'Application logs',
    description: 'Info, warning and error records from one service, with attributes and a trace context.',
    json: (now = Date.now()) =>
      pretty({
        resourceLogs: [
          {
            resource: { attributes: attrs({ 'service.name': 'checkout', 'service.version': '1.8.2', 'deployment.environment': 'dev' }) },
            scopeLogs: [
              {
                scope: SCOPE,
                logRecords: [
                  {
                    timeUnixNano: nanos(now, 3000),
                    severityNumber: 9,
                    severityText: 'INFO',
                    body: { stringValue: 'user logged in' },
                    attributes: attrs({ 'user.id': 'u-1042', 'http.route': '/login' }),
                  },
                  {
                    timeUnixNano: nanos(now, 2000),
                    severityNumber: 13,
                    severityText: 'WARN',
                    body: { stringValue: 'slow request' },
                    attributes: attrs({ 'http.route': '/cart', 'duration_ms': 1840 }),
                  },
                  {
                    timeUnixNano: nanos(now, 1000),
                    severityNumber: 17,
                    severityText: 'ERROR',
                    body: { stringValue: 'payment failed: card declined' },
                    attributes: attrs({ 'exception.type': 'CardDeclined', 'http.route': '/pay' }),
                    traceId: '5b8efff798038103d269b633813fc60c',
                    spanId: 'eee19b7ec3c1b174',
                  },
                ],
              },
            ],
          },
        ],
      }),
  },
  {
    id: 'k8s-logs',
    signal: 'logs',
    label: 'Kubernetes container logs',
    description: 'Logs of a pod as a node agent ships them: k8s.* resource attributes and a JSON body.',
    json: (now = Date.now()) =>
      pretty({
        resourceLogs: [
          {
            resource: {
              attributes: attrs({
                'k8s.namespace.name': 'shop',
                'k8s.pod.name': 'checkout-7d9f8b6c4-x2x9q',
                'k8s.container.name': 'checkout',
                'k8s.node.name': 'worker-1',
              }),
            },
            scopeLogs: [
              {
                scope: {},
                logRecords: [
                  {
                    timeUnixNano: nanos(now, 2000),
                    observedTimeUnixNano: nanos(now, 1900),
                    body: { stringValue: '{"level":"info","msg":"order created","order_id":"o-981"}' },
                    attributes: attrs({ 'log.iostream': 'stdout' }),
                  },
                  {
                    timeUnixNano: nanos(now, 1000),
                    observedTimeUnixNano: nanos(now, 900),
                    body: { stringValue: '{"level":"error","msg":"inventory timeout","order_id":"o-982"}' },
                    attributes: attrs({ 'log.iostream': 'stderr' }),
                  },
                ],
              },
            ],
          },
        ],
      }),
  },
  {
    id: 'http-trace',
    signal: 'traces',
    label: 'HTTP request trace',
    description: 'A server span with a database child span, plus a health-check span.',
    json: (now = Date.now()) =>
      pretty({
        resourceSpans: [
          {
            resource: { attributes: attrs({ 'service.name': 'checkout', 'deployment.environment': 'dev' }) },
            scopeSpans: [
              {
                scope: SCOPE,
                spans: [
                  {
                    traceId: '5b8efff798038103d269b633813fc60c',
                    spanId: 'eee19b7ec3c1b174',
                    name: 'GET /api/orders/{id}',
                    kind: 2,
                    startTimeUnixNano: nanos(now, 1200),
                    endTimeUnixNano: nanos(now, 1000),
                    attributes: attrs({
                      'http.request.method': 'GET',
                      'http.route': '/api/orders/{id}',
                      'url.full': 'https://shop.example/api/orders/42?token=secret',
                      'http.response.status_code': 200,
                    }),
                    status: {},
                  },
                  {
                    traceId: '5b8efff798038103d269b633813fc60c',
                    spanId: '9a1c3b5d7e9f0a2b',
                    parentSpanId: 'eee19b7ec3c1b174',
                    name: 'SELECT orders',
                    kind: 3,
                    startTimeUnixNano: nanos(now, 1150),
                    endTimeUnixNano: nanos(now, 1050),
                    attributes: attrs({ 'db.system': 'postgresql', 'db.query.text': 'SELECT * FROM orders WHERE id = $1' }),
                    status: {},
                  },
                  {
                    traceId: '0af7651916cd43dd8448eb211c80319c',
                    spanId: 'b7ad6b7169203331',
                    name: 'GET /healthz',
                    kind: 2,
                    startTimeUnixNano: nanos(now, 600),
                    endTimeUnixNano: nanos(now, 599),
                    attributes: attrs({ 'http.request.method': 'GET', 'http.route': '/healthz', 'http.response.status_code': 200 }),
                    status: {},
                  },
                ],
              },
            ],
          },
        ],
      }),
  },
  {
    id: 'error-trace',
    signal: 'traces',
    label: 'Failed request trace',
    description: 'A slow server span with an error status and an exception event.',
    json: (now = Date.now()) =>
      pretty({
        resourceSpans: [
          {
            resource: { attributes: attrs({ 'service.name': 'payments' }) },
            scopeSpans: [
              {
                scope: SCOPE,
                spans: [
                  {
                    traceId: '4bf92f3577b34da6a3ce929d0e0e4736',
                    spanId: '00f067aa0ba902b7',
                    name: 'POST /pay',
                    kind: 2,
                    startTimeUnixNano: nanos(now, 3500),
                    endTimeUnixNano: nanos(now, 1000),
                    attributes: attrs({ 'http.request.method': 'POST', 'http.route': '/pay', 'http.response.status_code': 500 }),
                    events: [
                      {
                        timeUnixNano: nanos(now, 1100),
                        name: 'exception',
                        attributes: attrs({ 'exception.type': 'TimeoutError', 'exception.message': 'card network timed out' }),
                      },
                    ],
                    status: { code: 2, message: 'card network timed out' },
                  },
                ],
              },
            ],
          },
        ],
      }),
  },
  {
    id: 'service-metrics',
    signal: 'metrics',
    label: 'Service metrics',
    description: 'A request counter (sum), a memory gauge and a latency histogram.',
    json: (now = Date.now()) => {
      const start = nanos(now, 60_000);
      const at = nanos(now, 1000);
      return pretty({
        resourceMetrics: [
          {
            resource: { attributes: attrs({ 'service.name': 'checkout', 'deployment.environment': 'dev' }) },
            scopeMetrics: [
              {
                scope: SCOPE,
                metrics: [
                  {
                    name: 'http.server.request.count',
                    unit: '{request}',
                    sum: {
                      aggregationTemporality: 2,
                      isMonotonic: true,
                      dataPoints: [
                        { startTimeUnixNano: start, timeUnixNano: at, asInt: '120', attributes: attrs({ 'http.route': '/cart', 'http.response.status_code': 200 }) },
                        { startTimeUnixNano: start, timeUnixNano: at, asInt: '3', attributes: attrs({ 'http.route': '/pay', 'http.response.status_code': 500 }) },
                      ],
                    },
                  },
                  {
                    name: 'process.memory.usage',
                    unit: 'By',
                    gauge: { dataPoints: [{ timeUnixNano: at, asInt: '73400320' }] },
                  },
                  {
                    name: 'http.server.request.duration',
                    unit: 'ms',
                    histogram: {
                      aggregationTemporality: 2,
                      dataPoints: [
                        {
                          startTimeUnixNano: start,
                          timeUnixNano: at,
                          count: '123',
                          sum: 9840.5,
                          explicitBounds: [10, 50, 100, 500],
                          bucketCounts: ['20', '60', '30', '10', '3'],
                          attributes: attrs({ 'http.route': '/cart' }),
                        },
                      ],
                    },
                  },
                ],
              },
            ],
          },
        ],
      });
    },
  },
];

/**
 * templatesFor orders the templates by the signals a receiver feeds, most
 * relevant first; with no signals known, all are offered in order.
 */
export function templatesFor(signals: string[]): PayloadTemplate[] {
  if (!signals.length) return PAYLOAD_TEMPLATES;
  const rank = (t: PayloadTemplate) => (signals.includes(t.signal) ? 0 : 1);
  return [...PAYLOAD_TEMPLATES].sort((a, b) => rank(a) - rank(b));
}
