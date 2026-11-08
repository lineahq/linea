import { isMap, isScalar, isSeq, Pair, parseDocument, Scalar, YAMLMap, YAMLSeq, type Document } from 'yaml';

/**
 * Configuration templates: starting configurations and the components of
 * the debugger's Collector distribution (build/linea-runtime.yaml), each
 * with a small, valid configuration for the common case. Inserting one
 * edits the YAML text in place, so comments and formatting survive.
 */

export type Signal = 'logs' | 'traces' | 'metrics';
export type Kind = 'receiver' | 'processor' | 'exporter' | 'connector';
export const SIGNALS: Signal[] = ['logs', 'traces', 'metrics'];

export interface ConfigTemplate {
  id: string;
  label: string;
  description: string;
  yaml: string;
}

const PIPELINE_HEADER = `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch: {}

exporters:
  # Prints what reaches it; the debugger captures every exporter's input.
  debug:
    verbosity: detailed
`;

const pipeline = (s: Signal) => `    ${s}:
      receivers: [otlp]
      processors: [batch]
      exporters: [debug]
`;

/** starterConfig is OTLP in, batch, debug out, with one pipeline per signal. */
export const starterConfig = (signals: Signal[]) => `${PIPELINE_HEADER}
service:
  pipelines:
${signals.map(pipeline).join('')}`;
const starter = starterConfig;

export const CONFIG_TEMPLATES: ConfigTemplate[] = [
  {
    id: 'logs',
    label: 'Logs pipeline',
    description: 'OTLP in, batch, debug out. Add processors to shape the logs.',
    yaml: starter(['logs']),
  },
  {
    id: 'traces',
    label: 'Traces pipeline',
    description: 'OTLP in, batch, debug out. Add sampling, filtering or redaction.',
    yaml: starter(['traces']),
  },
  {
    id: 'metrics',
    label: 'Metrics pipeline',
    description: 'OTLP in, batch, debug out. Add renames, filters or conversions.',
    yaml: starter(['metrics']),
  },
  {
    id: 'all',
    label: 'All signals',
    description: 'One OTLP receiver feeding a logs, a traces and a metrics pipeline.',
    yaml: starter(SIGNALS),
  },
];

/** What a snippet's body is written for: the signals of the pipelines it joins. */
export interface SnippetContext {
  signals: Signal[];
  /** For connectors, the pipelines created for its output. */
  outputs: string[];
}

export interface ComponentSnippet {
  kind: Kind;
  type: string;
  label: string;
  description: string;
  /** Signals the component accepts. */
  signals: Signal[];
  /** For connectors, the signal they emit; undefined means the one they accept. */
  emits?: Signal;
  /** For connectors, the output pipelines to create (name suffixes). */
  outputs?: string[];
  /** For connectors, whether they take over the source pipelines' exporters, as a router does. */
  replaces?: boolean;
  /** Components a scenario mocks: the Mocks tab supplies what they read. */
  mocked?: boolean;
  /** The component's configuration as YAML. */
  body(c: SnippetContext): string;
}

const ALL = SIGNALS;
const pick = <T>(c: SnippetContext, by: Partial<Record<Signal, T>>, fallback: Signal): [Signal, T][] => {
  const s = c.signals.filter((x) => by[x] !== undefined);
  return (s.length ? s : [fallback]).map((x) => [x, by[x] as T]);
};

export const COMPONENT_SNIPPETS: ComponentSnippet[] = [
  // Receivers.
  {
    kind: 'receiver',
    type: 'otlp',
    label: 'OTLP',
    description: 'Receives OTLP over gRPC and HTTP. Run inputs enter here.',
    signals: ALL,
    body: () => `protocols:
  grpc:
    endpoint: 0.0.0.0:4317
  http:
    endpoint: 0.0.0.0:4318
`,
  },
  {
    kind: 'receiver',
    type: 'filelog',
    label: 'File log',
    description: 'Tails log files, here Kubernetes container logs. The scenario mocks the files.',
    signals: ['logs'],
    mocked: true,
    body: () => `include: [/var/log/pods/*/*/*.log]
start_at: beginning
include_file_path: true
operators:
  # Parses the CRI or Docker format and sets k8s.* attributes from the path.
  - type: container
`,
  },
  // Processors.
  {
    kind: 'processor',
    type: 'transform',
    label: 'Transform (OTTL)',
    description: 'Rewrites records with OTTL statements: set, delete, rename, parse.',
    signals: ALL,
    body: (c) =>
      `error_mode: ignore
` +
      pick(
        c,
        {
          logs: `log_statements:
  - context: log
    statements:
      - set(log.severity_text, "INFO") where log.severity_text == ""
      - delete_key(log.attributes, "password")
`,
          traces: `trace_statements:
  - context: span
    statements:
      - set(span.name, Concat([span.attributes["http.request.method"], span.attributes["http.route"]], " ")) where span.attributes["http.route"] != nil
`,
          metrics: `metric_statements:
  - context: metric
    statements:
      - set(metric.description, "") where metric.description == nil
`,
        },
        'logs',
      )
        .map(([, b]) => b)
        .join(''),
  },
  {
    kind: 'processor',
    type: 'filter',
    label: 'Filter (OTTL)',
    description: 'Drops records that match OTTL conditions.',
    signals: ALL,
    body: (c) =>
      `error_mode: ignore
` +
      pick(
        c,
        {
          logs: `logs:
  log_record:
    - IsMatch(body, "health")
`,
          traces: `traces:
  span:
    - attributes["url.path"] == "/health"
`,
          metrics: `metrics:
  metric:
    - IsMatch(name, "^debug\\\\.")
`,
        },
        'logs',
      )
        .map(([, b]) => b)
        .join(''),
  },
  {
    kind: 'processor',
    type: 'attributes',
    label: 'Attributes',
    description: 'Inserts, updates, hashes or deletes record attributes.',
    signals: ALL,
    body: () => `actions:
  - key: deployment.environment
    value: production
    action: upsert
  - key: user.password
    action: delete
  - key: enduser.id
    action: hash
`,
  },
  {
    kind: 'processor',
    type: 'resource',
    label: 'Resource',
    description: 'Inserts, updates or deletes resource attributes.',
    signals: ALL,
    body: () => `attributes:
  - key: deployment.environment
    value: production
    action: upsert
`,
  },
  {
    kind: 'processor',
    type: 'redaction',
    label: 'Redaction',
    description: 'Masks attribute values that match patterns, such as card numbers.',
    signals: ALL,
    body: () => `allow_all_keys: true
blocked_values:
  # Visa and Mastercard numbers.
  - "4[0-9]{12}(?:[0-9]{3})?"
  - "(5[1-5][0-9]{14})"
summary: info
`,
  },
  {
    kind: 'processor',
    type: 'k8sattributes',
    label: 'Kubernetes attributes',
    description: 'Adds pod, namespace and deployment metadata. The scenario mocks the cluster.',
    signals: ALL,
    mocked: true,
    body: () => `auth_type: serviceAccount
extract:
  metadata:
    - k8s.namespace.name
    - k8s.pod.name
    - k8s.deployment.name
    - k8s.node.name
pod_association:
  - sources:
      - from: resource_attribute
        name: k8s.pod.ip
  - sources:
      - from: connection
`,
  },
  {
    kind: 'processor',
    type: 'batch',
    label: 'Batch',
    description: 'Groups records before export. Usually last.',
    signals: ALL,
    body: () => `timeout: 200ms
send_batch_size: 8192
`,
  },
  {
    kind: 'processor',
    type: 'memory_limiter',
    label: 'Memory limiter',
    description: 'Refuses data when memory is short. Usually first.',
    signals: ALL,
    body: () => `check_interval: 1s
limit_percentage: 80
spike_limit_percentage: 20
`,
  },
  {
    kind: 'processor',
    type: 'tail_sampling',
    label: 'Tail sampling',
    description: 'Keeps whole traces by policy: errors, slow requests, a percentage.',
    signals: ['traces'],
    body: () => `decision_wait: 10s
policies:
  - name: errors
    type: status_code
    status_code: {status_codes: [ERROR]}
  - name: slow
    type: latency
    latency: {threshold_ms: 500}
  - name: some-of-the-rest
    type: probabilistic
    probabilistic: {sampling_percentage: 10}
`,
  },
  {
    kind: 'processor',
    type: 'probabilistic_sampler',
    label: 'Probabilistic sampler',
    description: 'Keeps a percentage of traces or logs.',
    signals: ['traces', 'logs'],
    body: () => `sampling_percentage: 25
`,
  },
  {
    kind: 'processor',
    type: 'span',
    label: 'Span',
    description: 'Renames spans from their attributes.',
    signals: ['traces'],
    body: () => `name:
  from_attributes: [http.request.method, http.route]
  separator: " "
`,
  },
  {
    kind: 'processor',
    type: 'groupbyattrs',
    label: 'Group by attributes',
    description: 'Moves record attributes to the resource and regroups records by them.',
    signals: ALL,
    body: () => `keys:
  - host.name
`,
  },
  {
    kind: 'processor',
    type: 'metricstransform',
    label: 'Metrics transform',
    description: 'Renames metrics and their labels.',
    signals: ['metrics'],
    body: () => `transforms:
  - include: http_request_duration_ms
    action: update
    new_name: http.server.request.duration
`,
  },
  {
    kind: 'processor',
    type: 'cumulativetodelta',
    label: 'Cumulative to delta',
    description: 'Converts cumulative sums and histograms to deltas.',
    signals: ['metrics'],
    body: () => `{}
`,
  },
  {
    kind: 'processor',
    type: 'deltatocumulative',
    label: 'Delta to cumulative',
    description: 'Accumulates delta sums and histograms into cumulative ones.',
    signals: ['metrics'],
    body: () => `{}
`,
  },
  // Exporters.
  {
    kind: 'exporter',
    type: 'debug',
    label: 'Debug',
    description: 'Prints what reaches it. Never sends anything.',
    signals: ALL,
    body: () => `verbosity: detailed
`,
  },
  {
    kind: 'exporter',
    type: 'otlp',
    label: 'OTLP (gRPC)',
    description: 'Sends to a backend. The debugger captures it instead of sending.',
    signals: ALL,
    body: () => `endpoint: backend.example.invalid:4317
tls:
  insecure: true
`,
  },
  {
    kind: 'exporter',
    type: 'otlphttp',
    label: 'OTLP (HTTP)',
    description: 'Sends over HTTP. The debugger captures it instead of sending.',
    signals: ALL,
    body: () => `endpoint: https://backend.example.invalid:4318
`,
  },
  {
    kind: 'exporter',
    type: 'nop',
    label: 'No-op',
    description: 'Discards everything.',
    signals: ALL,
    body: () => `{}
`,
  },
  // Connectors.
  {
    kind: 'connector',
    type: 'routing',
    label: 'Routing',
    description: 'Sends records to pipelines by OTTL conditions. Creates the routed and default pipelines.',
    signals: ALL,
    outputs: ['routed', 'default'],
    replaces: true,
    body: (c) => {
      const cond = {
        logs: 'log.severity_number >= SEVERITY_NUMBER_ERROR',
        traces: 'span.status.code == STATUS_CODE_ERROR',
        metrics: 'resource.attributes["service.name"] == "checkout"',
      }[c.signals[0] ?? 'logs'];
      return `default_pipelines: [${c.outputs[1]}]
table:
  - condition: ${cond}
    pipelines: [${c.outputs[0]}]
`;
    },
  },
  {
    kind: 'connector',
    type: 'count',
    label: 'Count',
    description: 'Counts records into metrics. Creates a metrics pipeline for them.',
    signals: ALL,
    emits: 'metrics',
    outputs: ['counts'],
    body: (c) =>
      pick(
        c,
        {
          logs: `logs:
  log.records.by_service:
    description: Log records per service.
    attributes:
      - key: service.name
`,
          traces: `spans:
  span.count.by_service:
    description: Spans per service.
    attributes:
      - key: service.name
`,
          metrics: `datapoints:
  datapoint.count:
    description: Data points received.
`,
        },
        'logs',
      )
        .map(([, b]) => b)
        .join(''),
  },
  {
    kind: 'connector',
    type: 'spanmetrics',
    label: 'Span metrics',
    description: 'Turns spans into request, error and duration metrics. Creates a metrics pipeline.',
    signals: ['traces'],
    emits: 'metrics',
    outputs: ['red'],
    body: () => `histogram:
  explicit:
    buckets: [50ms, 250ms, 1s]
dimensions:
  - name: http.request.method
  - name: http.response.status_code
metrics_flush_interval: 15s
`,
  },
  {
    kind: 'connector',
    type: 'forward',
    label: 'Forward',
    description: 'Passes records on to another pipeline of the same signal.',
    signals: ALL,
    outputs: ['next'],
    replaces: true,
    body: () => `{}
`,
  },
];

export const KIND_LABELS: Record<Kind, string> = {
  receiver: 'Receivers',
  processor: 'Processors',
  exporter: 'Exporters',
  connector: 'Connectors',
};

export interface PipelineInfo {
  name: string;
  signal?: Signal;
}

const signalOf = (name: string): Signal | undefined => SIGNALS.find((s) => name === s || name.startsWith(`${s}/`));

/** pipelinesOf lists the configuration's pipelines; unreadable text has none. */
export function pipelinesOf(text: string): PipelineInfo[] {
  const doc = parseDocument(text);
  if (doc.errors.length) return [];
  const p = doc.getIn(['service', 'pipelines']);
  if (!isMap(p)) return [];
  return p.items.map((i) => String(isScalar(i.key) ? i.key.value : i.key)).map((name) => ({ name, signal: signalOf(name) }));
}

/** defaultPipelines are the pipelines a component joins unless the user says otherwise. */
export function defaultPipelines(s: ComponentSnippet, pipelines: PipelineInfo[]): string[] {
  return pipelines.filter((p) => p.signal && s.signals.includes(p.signal)).map((p) => p.name);
}

const keyOf = (k: unknown) => String(isScalar(k) ? k.value : k);

function keys(m: unknown): string[] {
  return isMap(m) ? m.items.map((i) => keyOf(i.key)) : [];
}

function uniqueName(base: string, taken: string[]): string {
  if (!taken.includes(base)) return base;
  for (let i = 2; ; i++) if (!taken.includes(`${base}/${i}`)) return `${base}/${i}`;
}

/** lastEmptyValue is the null scalar ending a mapping, as in "debug:" at its end. */
function lastEmptyValue(v: unknown): Scalar | undefined {
  while (isMap(v) && v.items.length) v = v.items[v.items.length - 1].value;
  return isScalar(v) && v.value === null ? v : undefined;
}

const ORDER = ['receivers', 'processors', 'exporters', 'connectors', 'extensions', 'service'];

/** section returns a top-level section, adding it in the usual order. */
function section(doc: Document, name: string): YAMLMap {
  const s = doc.get(name);
  if (isMap(s)) return s;
  const m = new YAMLMap();
  const root = doc.contents as YAMLMap;
  const existing = root.items.findIndex((p) => keyOf(p.key) === name);
  if (existing >= 0) {
    root.items[existing].value = m;
    return m;
  }
  const key = new Scalar(name);
  key.spaceBefore = root.items.length > 0;
  const after = ORDER.slice(ORDER.indexOf(name) + 1);
  const found = root.items.findIndex((p) => after.includes(keyOf(p.key)));
  const at = found < 0 ? root.items.length : found;
  if (at > 0 && at < root.items.length) {
    // A blank line after an empty value (debug:) is kept on that value;
    // it now belongs before the section that follows the new one.
    const empty = lastEmptyValue(root.items[at - 1].value);
    if (empty?.spaceBefore) empty.spaceBefore = false;
    const next = root.items[at].key;
    if (isScalar(next)) next.spaceBefore = true;
  }
  root.items.splice(at, 0, new Pair(key, m));
  return m;
}

function list(doc: Document, path: string[]): YAMLSeq {
  const s = doc.getIn(path);
  if (isSeq(s)) return s;
  const seq = new YAMLSeq();
  seq.flow = true;
  doc.setIn(path, seq);
  return seq;
}

export interface Inserted {
  yaml: string;
  /** The new component's ID, such as transform/2. */
  id: string;
  /** The 1-based line of its key, to reveal in the editor. */
  line: number;
  /** Pipelines created for a connector's output. */
  created: string[];
}

/**
 * insertComponent adds a component to its section and to the given
 * pipelines. Processors go before a batch processor (memory_limiter goes
 * first); a connector exports from the given pipelines and receives into
 * new pipelines that send to the same exporters (a router takes them over
 * from the source pipelines, a counter copies).
 */
export function insertComponent(text: string, s: ComponentSnippet, into: string[]): Inserted | { error: string } {
  let doc = parseDocument(text);
  // Empty or comment-only text has no mapping yet: start one below the comments.
  if (!doc.errors.length && doc.contents === null) doc = parseDocument(`${text.trimEnd()}\n{}\n`);
  if (doc.errors.length)
    return {
      error: `The configuration cannot be read: ${doc.errors[0].message}`,
    };
  if (doc.contents !== null && !isMap(doc.contents)) return { error: 'The configuration is not a YAML mapping.' };
  if (isMap(doc.contents) && doc.contents.flow) doc.contents.flow = false;

  const sectionName = `${s.kind}s`;
  const sec = section(doc, sectionName);
  sec.flow = false;
  const id = uniqueName(s.type, keys(sec));
  const pipes = pipelinesOf(text);
  const signals = [...new Set(into.map((n) => pipes.find((p) => p.name === n)?.signal).filter((x): x is Signal => !!x))];
  const outSignal = s.emits ?? signals[0] ?? s.signals[0];
  const allPipes = keys(doc.getIn(['service', 'pipelines']));
  const outputs: string[] = [];
  for (const o of s.outputs ?? []) outputs.push(uniqueName(`${outSignal}/${o}`, [...allPipes, ...outputs]));
  // Output pipelines exist only for a connector some pipeline exports to.
  const created = into.length ? outputs : [];
  const body = parseDocument(s.body({ signals, outputs }));
  sec.set(id, body.contents);

  const exporters = new Set<string>();
  for (const name of into) {
    const base = ['service', 'pipelines', name];
    if (s.kind === 'processor') {
      const l = list(doc, [...base, 'processors']);
      const names = l.items.map(keyOf);
      const at = s.type === 'memory_limiter' ? 0 : names.findIndex((n) => n === 'batch' || n.startsWith('batch/'));
      l.items.splice(at < 0 ? l.items.length : at, 0, doc.createNode(id));
    } else if (s.kind === 'receiver') {
      list(doc, [...base, 'receivers']).items.push(doc.createNode(id));
    } else {
      const l = list(doc, [...base, 'exporters']);
      const own = l.items.map(keyOf).filter((e) => keys(doc.get('exporters')).includes(e));
      own.forEach((e) => exporters.add(e));
      // A router sends on what the pipeline exported; a counter copies.
      if (s.replaces) l.items = l.items.filter((i) => !own.includes(keyOf(i)));
      l.items.push(doc.createNode(id));
    }
  }
  if (created.length) {
    if (!exporters.size) {
      const ex = section(doc, 'exporters');
      if (!keys(ex).includes('debug')) ex.set('debug', parseDocument('verbosity: detailed\n').contents);
      exporters.add('debug');
    }
    for (const name of created) {
      const p = new YAMLMap();
      const recv = new YAMLSeq();
      recv.flow = true;
      recv.items.push(doc.createNode(id));
      const exp = new YAMLSeq();
      exp.flow = true;
      exp.items.push(...[...exporters].map((e) => doc.createNode(e)));
      p.set('receivers', recv);
      p.set('exporters', exp);
      doc.setIn(['service', 'pipelines', name], p);
    }
  }
  // The user's formatting stays: no padding inside [a, b], no folding.
  const yaml = doc.toString({ flowCollectionPadding: false, lineWidth: 0 });
  return { yaml, id, line: lineOf(yaml, sectionName, id), created };
}

/**
 * addSignalPipeline adds a pipeline for `signal` to the configuration: from
 * its OTLP receiver (added when it has none) to its debug exporter (added
 * likewise). Nothing existing changes.
 */
export function addSignalPipeline(text: string, signal: Signal): { yaml: string; pipeline: string; receiver: string } | { error: string } {
  const doc = parseDocument(text.trim() ? text : '{}');
  if (doc.errors.length) return { error: `The configuration cannot be read: ${doc.errors[0].message}` };
  if (!isMap(doc.contents)) return { error: 'The configuration is not a YAML mapping.' };
  if (doc.contents.flow) doc.contents.flow = false;
  const pick = (sec: string, type: string, body: string) => {
    const m = section(doc, sec);
    m.flow = false;
    const found = keys(m).find((k) => k === type || k.startsWith(`${type}/`));
    if (found) return found;
    m.set(type, parseDocument(body).contents);
    return type;
  };
  const receiver = pick('receivers', 'otlp', 'protocols:\n  grpc:\n    endpoint: 0.0.0.0:4317\n  http:\n    endpoint: 0.0.0.0:4318\n');
  const exporter = pick('exporters', 'debug', 'verbosity: detailed\n');
  const service = section(doc, 'service');
  service.flow = false;
  let pipes = service.get('pipelines');
  if (!isMap(pipes)) {
    pipes = new YAMLMap();
    service.set('pipelines', pipes);
  }
  const pipelines = pipes as YAMLMap;
  pipelines.flow = false;
  const name = uniqueName(signal, keys(pipelines));
  const seq = (id: string) => {
    const s = new YAMLSeq();
    s.flow = true;
    s.items.push(doc.createNode(id));
    return s;
  };
  const p = new YAMLMap();
  p.set('receivers', seq(receiver));
  p.set('exporters', seq(exporter));
  pipelines.set(name, p);
  return { yaml: doc.toString({ flowCollectionPadding: false, lineWidth: 0 }), pipeline: name, receiver };
}

/** lineOf finds the 1-based line of a key in a top-level section. */
export function lineOf(yaml: string, sectionName: string, key: string): number {
  const lines = yaml.split('\n');
  const start = lines.findIndex((l) => l.startsWith(`${sectionName}:`));
  for (let i = start + 1; start >= 0 && i < lines.length; i++) {
    if (/^\S/.test(lines[i])) break;
    if (new RegExp(`^\\s+${key.replace(/[.*+?^${}()|[\]\\/]/g, '\\$&')}:`).test(lines[i])) return i + 1;
  }
  return 1;
}
