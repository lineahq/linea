import { stringify } from 'yaml';
import type { Graph, TopologyComponent } from '../api/types';

/**
 * Mock suggestions are derived from the configuration and the input, never
 * from a real environment: which components need a stand-in, which
 * ${env:NAME} values the configuration reads, and starter fixtures that the
 * configured components would accept. A suggestion is only a starting
 * point; the run reports what the mocked components actually read.
 */

type Obj = Record<string, unknown>;
const obj = (v: unknown): Obj => (v && typeof v === 'object' && !Array.isArray(v) ? (v as Obj) : {});
const arr = (v: unknown): unknown[] => (Array.isArray(v) ? v : []);
const strs = (v: unknown): string[] => arr(v).filter((x): x is string => typeof x === 'string');

/** Provider is a mock provider of the debugger (internal/debugger/mock). */
export type Provider = 'files' | 'kubernetes';

/** PROVIDER_TYPES are the component types each provider stands in for, by alias. */
const PROVIDER_TYPES: Record<Provider, string[]> = {
  files: ['filelog', 'file_log'],
  kubernetes: ['k8sattributes', 'k8s_attributes'],
};

export function providerOf(c: TopologyComponent): Provider | undefined {
  if (c.mode !== 'dependency_mocked') return undefined;
  return (Object.keys(PROVIDER_TYPES) as Provider[]).find((p) => PROVIDER_TYPES[p].includes(c.type));
}

/** mockedComponents lists the components that run against a mocked dependency, by provider. */
export function mockedComponents(g?: Graph): { component: TopologyComponent; provider: Provider }[] {
  return (g?.components ?? []).flatMap((component) => {
    const provider = providerOf(component);
    return provider ? [{ component, provider }] : [];
  });
}

/** componentConfig is a component's effective configuration, by kind and ID. */
export function componentConfig(effective: unknown, c: Pick<TopologyComponent, 'kind' | 'id'>): Obj {
  return obj(obj(obj(effective)[`${c.kind}s`])[c.id]);
}

export interface EnvRef {
  name: string;
  /** ${env:NAME:-default}: the configuration resolves without a value. */
  hasDefault: boolean;
}

/**
 * envReferences lists the ${env:NAME} (and bare ${NAME}) references of
 * configuration text in order of appearance. Comments and $${escaped}
 * references are skipped.
 */
export function envReferences(configYAML: string): EnvRef[] {
  const out = new Map<string, EnvRef>();
  for (const line of configYAML.split('\n')) {
    const code = line.trimStart().startsWith('#') ? '' : line.replace(/\s#.*$/, '');
    for (const m of code.matchAll(/(?<!\$)\$\{(?:env:)?([A-Za-z_][A-Za-z0-9_]*)(:-[^}]*)?\}/g)) {
      const prev = out.get(m[1]);
      out.set(m[1], { name: m[1], hasDefault: (prev?.hasDefault ?? true) && m[2] !== undefined });
    }
  }
  return [...out.values()];
}

/** globMatch matches a path against a doublestar glob: *, **, ? and {a,b}. */
export function globMatch(pattern: string, path: string): boolean {
  let re = '';
  for (let i = 0; i < pattern.length; i++) {
    const c = pattern[i];
    if (c === '*' && pattern[i + 1] === '*') {
      const slash = pattern[i + 2] === '/';
      re += slash ? '(?:.*/)?' : '.*';
      i += slash ? 2 : 1;
    } else if (c === '*') re += '[^/]*';
    else if (c === '?') re += '[^/]';
    else if (c === '{') {
      const end = pattern.indexOf('}', i);
      if (end < 0) re += '\\{';
      else {
        re += `(?:${pattern
          .slice(i + 1, end)
          .split(',')
          .map(escape)
          .join('|')})`;
        i = end;
      }
    } else re += escape(c);
  }
  return new RegExp(`^${re}$`).test(path);
}

const escape = (s: string) => s.replace(/[.+^$()|[\]\\]/g, '\\$&');

/** Example identities used for generated fixtures, consistent across suggestions. */
export const EXAMPLE = {
  namespace: 'default',
  pod: 'app-7d9f8b6c4-x2x9q',
  uid: '0b7c3f52-1d2e-4f6a-9b8c-7d6e5f4a3b2c',
  container: 'app',
  ip: '10.42.0.17',
  node: 'worker-1',
};

/**
 * concretePath turns an include pattern into a path the pattern matches.
 * Kubernetes log directories get kubelet-style names, so the container
 * parser can read the namespace, pod and container from the path.
 */
export function concretePath(pattern: string): string {
  const { namespace, pod, uid, container } = EXAMPLE;
  const parts = pattern.split('/');
  const pods = parts.indexOf('pods');
  const containers = parts.indexOf('containers');
  const out = parts.map((seg, i) => {
    if (!/[*?{]/.test(seg)) return seg;
    if (seg === '**') return container;
    let name = 'app';
    if (pods >= 0 && i === pods + 1) name = `${namespace}_${pod}_${uid}`;
    else if (pods >= 0 && i === pods + 2) name = container;
    else if (pods >= 0 && i === pods + 3) name = '0';
    else if (containers >= 0 && i === containers + 1) name = `${pod}_${namespace}_${container}-${'0'.repeat(64)}`;
    return seg
      .replace(/\{([^,}]*)[^}]*\}/g, '$1')
      .replace(/\*+/g, () => {
        const n = name;
        name = '';
        return n;
      })
      .replace(/\?/g, '0');
  });
  return out.join('/');
}

function operatorTypes(cfg: Obj): string[] {
  return arr(cfg.operators).map((o) => {
    const op = obj(o);
    return String(op.type ?? obj(op.builder).type ?? '');
  });
}

/** LogFormat is a log line format the filelog receiver's parsers read. */
export type LogFormat = 'plain' | 'container' | 'json';

/** sampleLog is three example log lines in a format, with a final newline. */
export function sampleLog(format: LogFormat): string {
  const lines = [
    ['INFO', 'request completed in 12ms'],
    ['WARN', 'cache miss for key user:42'],
    ['ERROR', 'payment declined: card expired'],
  ];
  const out =
    format === 'container'
      ? lines.map(([, msg], i) => `2026-10-10T08:00:0${i}.000000000Z stdout F ${msg}`)
      : format === 'json'
        ? lines.map(([level, msg], i) => JSON.stringify({ time: `2026-10-10T08:00:0${i}Z`, level: level.toLowerCase(), msg }))
        : lines.map(([level, msg], i) => `2026-10-10T08:00:0${i}Z ${level} ${msg}`);
  return out.join('\n') + '\n';
}

export interface FileSuggestion {
  path: string;
  content: string;
  /** Why the suggestion may not be read, when it may not. */
  warning?: string;
}

/**
 * suggestFile proposes a file a filelog receiver reads: a path its include
 * patterns match (and no exclude pattern does), in the format its first
 * parser expects. used avoids paths already mocked.
 */
export function suggestFile(cfg: Obj, used: string[] = []): FileSuggestion {
  const include = strs(cfg.include);
  const exclude = strs(cfg.exclude);
  const readable = (p: string) => include.some((i) => globMatch(i, p)) && !exclude.some((e) => globMatch(e, p));
  let path = include.length ? concretePath(include[0]) : '/var/log/app.log';
  for (let n = 1; used.includes(path) && n < 10; n++) path = path.replace(/(\d*)(\.[^./]*)?$/, (_, _d, ext = '') => `${n}${ext}`);
  const types = operatorTypes(cfg);
  const content = sampleLog(types.includes('container') ? 'container' : types.includes('json_parser') ? 'json' : 'plain');
  const warning = !include.length
    ? 'The receiver has no include patterns.'
    : readable(path)
      ? undefined
      : 'The receiver may not read this path: check it against the include and exclude patterns.';
  return { path, content: content + '\n', warning };
}

/** PodIdentity is what an input says about the pod that sent it. */
export interface PodIdentity {
  name?: string;
  namespace?: string;
  uid?: string;
  ip?: string;
  service?: string;
}

const RESOURCE_KEYS = ['resourceSpans', 'resource_spans', 'resourceLogs', 'resource_logs', 'resourceMetrics', 'resource_metrics'];

function anyValue(v: unknown): string | undefined {
  const o = obj(v);
  for (const k of ['stringValue', 'string_value', 'intValue', 'int_value']) {
    if (o[k] !== undefined) return String(o[k]);
  }
  return undefined;
}

/** podIdentities reads the k8s.* and service.name resource attributes of OTLP JSON payloads. */
export function podIdentities(payloads: unknown[]): PodIdentity[] {
  const out: PodIdentity[] = [];
  const seen = new Set<string>();
  for (const p of payloads) {
    for (const key of RESOURCE_KEYS) {
      for (const r of arr(obj(p)[key])) {
        const attrs = new Map<string, string>();
        for (const a of arr(obj(obj(r).resource).attributes)) {
          const v = anyValue(obj(a).value);
          if (typeof obj(a).key === 'string' && v !== undefined) attrs.set(obj(a).key as string, v);
        }
        const id: PodIdentity = {
          name: attrs.get('k8s.pod.name'),
          namespace: attrs.get('k8s.namespace.name'),
          uid: attrs.get('k8s.pod.uid'),
          ip: attrs.get('k8s.pod.ip'),
          service: attrs.get('service.name'),
        };
        const k = JSON.stringify(id);
        if (seen.has(k)) continue;
        seen.add(k);
        out.push(id);
      }
    }
  }
  return out;
}

/** association names how the processor's pod_association rules can match. */
function associations(cfg: Obj): { connection: boolean; attributes: string[] } {
  const rules = arr(cfg.pod_association);
  // The processor's default: k8s.pod.ip, k8s.pod.uid, then the connection.
  if (!rules.length) return { connection: true, attributes: ['k8s.pod.ip', 'k8s.pod.uid'] };
  const sources = rules.flatMap((r) => arr(obj(r).sources).map(obj));
  return {
    connection: sources.some((s) => s.from === 'connection'),
    attributes: sources.filter((s) => s.from === 'resource_attribute').map((s) => String(s.name)),
  };
}

export interface KubeSuggestion {
  /** Manifests to add to mocks.kubernetes.objects. */
  manifests: string;
  /** Set as the input's client address, for association by connection. */
  clientAddress?: string;
  /** What the suggestion assumed, for the user to review. */
  notes: string[];
}

const slug = (s: string) =>
  s
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, '-')
    .replace(/^-+|-+$/g, '') || 'app';

/**
 * suggestKube proposes the pods (with their namespace and node) that a
 * k8sattributes processor would associate with the inputs: one per pod
 * identity the inputs carry, else one pod reached by the input's client
 * address. Labels and annotations the processor extracts get example
 * values, so their attributes appear in the output.
 */
export function suggestKube(cfg: Obj, payloads: unknown[], clientAddress = ''): KubeSuggestion {
  const extract = obj(cfg.extract);
  const metadata = strs(extract.metadata);
  const rules = (key: 'labels' | 'annotations') => arr(extract[key]).map(obj);
  const from = (o: Obj) => String(o.from || 'pod');
  const example = (o: Obj) => (String(o.key || o.key_regex || 'example').includes('team') ? 'payments' : 'example');
  const pick = (key: 'labels' | 'annotations', on: string) => {
    const m: Obj = {};
    for (const r of rules(key)) if (from(r) === on && r.key) m[String(r.key)] = example(r);
    return Object.keys(m).length ? m : undefined;
  };
  const assoc = associations(cfg);
  const notes: string[] = [];
  const ids = podIdentities(payloads).filter((p) => p.name || p.uid || p.ip);
  let address: string | undefined;
  if (!ids.length) {
    const service = podIdentities(payloads).find((p) => p.service)?.service;
    address = clientAddress || EXAMPLE.ip;
    ids.push({ ip: address, service });
    if (assoc.connection) notes.push(`The input is sent from ${address}, the pod's IP: the processor associates it by connection.`);
    else notes.push(`The processor associates by ${assoc.attributes.join(', ') || 'resource attributes'} only: add one of them to the input's resources.`);
  } else {
    const matched = ids.some(
      (p) =>
        (p.ip && assoc.attributes.includes('k8s.pod.ip')) ||
        (p.uid && assoc.attributes.includes('k8s.pod.uid')) ||
        (p.name && assoc.attributes.includes('k8s.pod.name')),
    );
    notes.push(`Pods are named after the input's k8s.* resource attributes.`);
    if (!matched && assoc.connection) {
      address = clientAddress || ids.find((p) => p.ip)?.ip || EXAMPLE.ip;
      notes.push(`No association rule reads those attributes: the input is sent from ${address}, the first pod's IP.`);
    }
  }
  const objects: Obj[] = [];
  const namespaces = new Set<string>();
  ids.forEach((id, i) => {
    const base = slug(id.service ?? EXAMPLE.container);
    const name = id.name ?? `${base}-7d9f8b6c4-${['x2x9q', 'k4m2n', 'p8r3t'][i % 3]}`;
    const namespace = id.namespace ?? EXAMPLE.namespace;
    namespaces.add(namespace);
    const ip = id.ip ?? (i === 0 && address ? address : `10.42.0.${17 + i}`);
    const md: Obj = { name, namespace, uid: id.uid ?? EXAMPLE.uid.replace(/.$/, String(i)) };
    const labels = pick('labels', 'pod') ?? {};
    const annotations = pick('annotations', 'pod');
    if (annotations) md.annotations = annotations;
    // A ReplicaSet named <deployment>-<hash> owns a deployment's pods; the
    // processor takes the deployment name from it and the template hash.
    const rs = name.split('-').length > 2 ? name.split('-').slice(0, -1).join('-') : undefined;
    const owned = rs && metadata.some((m) => /deployment|replicaset/.test(m));
    if (owned) labels['pod-template-hash'] = rs.split('-').pop();
    if (Object.keys(labels).length) md.labels = labels;
    if (owned) {
      md.ownerReferences = [{ apiVersion: 'apps/v1', kind: 'ReplicaSet', name: rs, uid: `5e1d3c2b-0a9f-4e8d-8c7b-6a5f4e3d2c1${i}`, controller: true }];
    }
    objects.push({
      apiVersion: 'v1',
      kind: 'Pod',
      metadata: md,
      spec: { nodeName: EXAMPLE.node, containers: [{ name: base, image: `registry.example/${base}:1.0.0` }] },
      status: { phase: 'Running', podIP: ip, podIPs: [{ ip }], startTime: '2026-10-10T08:00:00Z' },
    });
  });
  for (const ns of namespaces) {
    const md: Obj = { name: ns };
    const labels = pick('labels', 'namespace');
    if (labels) md.labels = labels;
    const annotations = pick('annotations', 'namespace');
    if (annotations) md.annotations = annotations;
    objects.push({ apiVersion: 'v1', kind: 'Namespace', metadata: md });
  }
  if (pick('labels', 'node') || pick('annotations', 'node')) {
    objects.push({
      apiVersion: 'v1',
      kind: 'Node',
      metadata: { name: EXAMPLE.node, labels: pick('labels', 'node'), annotations: pick('annotations', 'node') },
    });
  }
  return {
    manifests: objects.map((o) => stringify(o, { aliasDuplicateObjects: false })).join('---\n'),
    clientAddress: address,
    notes,
  };
}
