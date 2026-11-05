import { Document, isMap, isScalar, isSeq, parseAllDocuments, parseDocument, Scalar, YAMLMap, type Pair } from 'yaml';
import type { Graph, Report } from '../api/types';

/**
 * The scenario YAML text is the source of truth; the form reads a model
 * from it and writes edits back through the parsed document, so comments and
 * untouched formatting survive.
 */

export const CONDITION_KEYS = ['count', 'dropped', 'attributes', 'resource_attributes'] as const;
export const SIGNALS = ['traces', 'metrics', 'logs'] as const;

export interface InputModel {
  receiver: string;
  /** Empty when the signal is taken from the payload. */
  signal: string;
  file?: string;
  /** Inline OTLP JSON size in characters, when the input has data. */
  inlineSize?: number;
  /** Inline OTLP JSON, when the input has data. */
  data?: string;
  /** The sender IP address, for connection-based Kubernetes association. */
  clientAddress: string;
  /** The wait before this input is sent (a Go duration), or empty. */
  after: string;
}

/** FileModel is one file of the mocked filesystem (mocks.files). */
export interface FileModel {
  /** The deployment path the receiver's include patterns match. */
  path: string;
  /** Present before the Collector starts; start_at decides whether it is read. */
  existing: string;
  /** Written while the Collector runs: always read. */
  content: string;
  /** Raw content from a file relative to the scenario, instead of content. */
  file?: string;
}

/** KubeObjectModel summarizes one mocks.kubernetes.objects entry. */
export interface KubeObjectModel {
  kind: string;
  name: string;
  namespace: string;
  /** Items of a List entry. */
  items?: number;
}

export interface TargetModel {
  exporter?: string;
  processor?: string;
  /** A simulated client endpoint of a component that runs for real. */
  endpoint?: { component: string; path: string };
  pipeline?: string;
  signal: string;
}

export interface AssertionModel {
  name: string;
  target: TargetModel;
  /** The assertion's condition keys as YAML text. */
  conditions: string;
}

export interface ScenarioModel {
  name: string;
  timeout: string;
  /** execution.settle: the wait after the last input, or empty to derive it. */
  settle: string;
  /** execution.timestamps: 'rebase', 'recorded' or empty (recorded). */
  timestamps: string;
  environment: [string, string][];
  inputs: InputModel[];
  files: FileModel[];
  kubeObjects: KubeObjectModel[];
  /** mocks.kubernetes.files: manifest paths relative to the scenario. */
  kubeFiles: string[];
  /** mocks.providers: each module provider's settings, as YAML text. */
  providers: ProviderModel[];
  assertions: AssertionModel[];
}

export interface ProviderModel {
  name: string;
  /** The settings mapping as YAML text, comments included; empty for none. */
  settings: string;
}

export type Edit = (doc: Document) => void;

function parse(text: string): Document | { error: string } {
  if (!text.trim()) return new Document({ version: 1 });
  const doc = parseDocument(text);
  if (doc.errors.length) return { error: doc.errors[0].message };
  if (doc.contents !== null && !isMap(doc.contents)) return { error: 'The scenario must be a YAML mapping.' };
  return doc;
}

const str = (v: unknown): string => (v === undefined || v === null ? '' : String(v));

/** readScenario reads the form model, or the reason the text cannot be read. */
export function readScenario(text: string): { model: ScenarioModel } | { error: string } {
  const doc = parse(text);
  if ('error' in doc) return doc;
  const env = doc.get('environment');
  const environment: [string, string][] = isMap(env)
    ? env.items.map((p) => [str(isScalar(p.key) ? p.key.value : p.key), str(isScalar(p.value) ? p.value.value : p.value)])
    : [];
  const inputs: InputModel[] = [];
  const ins = doc.get('inputs');
  if (isSeq(ins)) {
    for (const n of ins.items) {
      if (!isMap(n)) continue;
      const data = n.get('data', true);
      inputs.push({
        receiver: str(n.get('receiver')),
        signal: str(n.get('signal')),
        file: n.has('file') ? str(n.get('file')) : undefined,
        inlineSize: data === undefined ? undefined : isScalar(data) ? str(data.value).length : String(data).length,
        data: data === undefined ? undefined : isScalar(data) ? str(data.value) : JSON.stringify((data as YAMLMap).toJSON()),
        clientAddress: str(n.get('client_address')),
        after: str(n.get('after')),
      });
    }
  }
  const assertions: AssertionModel[] = [];
  const as = doc.get('assertions');
  if (isSeq(as)) {
    for (const n of as.items) {
      if (!isMap(n)) continue;
      const t = n.get('target');
      const target: TargetModel = { signal: '' };
      if (isMap(t)) {
        for (const k of ['exporter', 'processor', 'pipeline', 'signal'] as const) {
          if (t.has(k)) target[k] = str(t.get(k));
        }
        const e = t.get('endpoint');
        if (isMap(e)) target.endpoint = { component: str(e.get('component')), path: str(e.get('path')) };
      }
      assertions.push({ name: str(n.get('name')), target, conditions: conditionsText(n) });
    }
  }
  const files: FileModel[] = [];
  const fs = doc.getIn(['mocks', 'files']);
  if (isSeq(fs)) {
    for (const n of fs.items) {
      if (!isMap(n)) continue;
      files.push({
        path: str(n.get('path')),
        existing: str(n.get('existing')),
        content: str(n.get('content')),
        file: n.has('file') ? str(n.get('file')) : undefined,
      });
    }
  }
  const kubeObjects: KubeObjectModel[] = [];
  const objs = doc.getIn(['mocks', 'kubernetes', 'objects']);
  if (isSeq(objs)) {
    for (const n of objs.items) {
      if (!isMap(n)) continue;
      const items = n.get('items');
      kubeObjects.push({
        kind: str(n.get('kind')),
        name: str(n.getIn(['metadata', 'name'])),
        namespace: str(n.getIn(['metadata', 'namespace'])),
        items: str(n.get('kind')).endsWith('List') && isSeq(items) ? items.items.length : undefined,
      });
    }
  }
  const kf = doc.getIn(['mocks', 'kubernetes', 'files']);
  const kubeFiles = isSeq(kf) ? kf.items.map((n) => str(isScalar(n) ? n.value : n)) : [];
  const providers: ProviderModel[] = [];
  const ps = doc.getIn(['mocks', 'providers']);
  if (isMap(ps)) {
    for (const pair of ps.items as Pair[]) {
      providers.push({ name: str(isScalar(pair.key) ? pair.key.value : pair.key), settings: nodeText(pair.value) });
    }
  }
  return {
    model: {
      name: str(doc.get('name')),
      timeout: str(doc.getIn(['execution', 'timeout'])),
      settle: str(doc.getIn(['execution', 'settle'])),
      timestamps: str(doc.getIn(['execution', 'timestamps'])),
      environment,
      inputs,
      files,
      kubeObjects,
      kubeFiles,
      providers,
      assertions,
    },
  };
}

/** nodeText renders a node as its own YAML document, comments included. */
function nodeText(n: unknown): string {
  if (n === null || n === undefined || (isScalar(n) && (n.value === null || n.value === undefined))) return '';
  if ((isMap(n) || isSeq(n)) && n.items.length === 0) return '';
  return new Document(isMap(n) || isSeq(n) || isScalar(n) ? n.clone() : n).toString();
}

/** conditionsText renders an assertion's conditions, comments included. */
function conditionsText(n: YAMLMap): string {
  const sub = new Document(new YAMLMap());
  const m = sub.contents as YAMLMap;
  for (const p of n.items as Pair[]) {
    const k = isScalar(p.key) ? p.key.value : p.key;
    if ((CONDITION_KEYS as readonly unknown[]).includes(k)) m.items.push(p.clone() as Pair);
  }
  return m.items.length ? sub.toString() : '';
}

/**
 * editScenario applies edit to the parsed text and returns the new text. An
 * empty text starts a version 1 scenario.
 */
export function editScenario(text: string, edit: Edit): { yaml: string } | { error: string } {
  const doc = parse(text);
  if ('error' in doc) return doc;
  try {
    edit(doc);
  } catch (e) {
    return { error: e instanceof Error ? e.message : String(e) };
  }
  return { yaml: doc.toString() };
}

/** setField sets a scalar at path, or removes it when value is empty. */
export const setField =
  (path: (string | number)[], value: string): Edit =>
  (doc) => {
    if (value === '') {
      doc.deleteIn(path);
      // Leave no empty parent mapping behind (execution: {}).
      const parent = path.slice(0, -1);
      const p = parent.length ? doc.getIn(parent) : undefined;
      if (isMap(p) && p.items.length === 0) doc.deleteIn(parent);
      return;
    }
    const cur = doc.getIn(path, true);
    if (isScalar(cur)) cur.value = value;
    else doc.setIn(path, value);
  };

/** setEnvironment replaces the environment entries, keeping existing nodes. */
export const setEnvironment =
  (entries: [string, string][]): Edit =>
  (doc) => {
    const kept = entries.filter(([k]) => k.trim() !== '');
    const dup = kept.find(([k], i) => kept.findIndex(([o]) => o === k) !== i);
    if (dup) throw new Error(`The environment already has ${dup[0]}.`);
    if (!kept.length) {
      doc.delete('environment');
      return;
    }
    const old = doc.get('environment');
    const m = new YAMLMap();
    for (const [k, v] of kept) {
      const prev = isMap(old) ? (old.items as Pair[]).find((p) => isScalar(p.key) && p.key.value === k) : undefined;
      if (prev && isScalar(prev.value)) {
        // An empty value is written quoted; the typed value need not be.
        if (prev.value.value === '') prev.value.type = undefined;
        prev.value.value = v;
        m.items.push(prev);
      } else {
        m.items.push(doc.createPair(k, v));
      }
    }
    doc.set('environment', m);
  };

export function list(doc: Document, key: string) {
  let s = doc.get(key);
  if (!isSeq(s)) {
    doc.set(key, doc.createNode([]));
    s = doc.get(key);
  }
  if (!isSeq(s)) throw new Error(`${key} is not a list`);
  return s;
}

/** listIn is list for a nested path such as mocks.files. */
function listIn(doc: Document, path: string[]) {
  let s = doc.getIn(path);
  if (!isSeq(s)) {
    doc.setIn(path, doc.createNode([]));
    s = doc.getIn(path);
  }
  if (!isSeq(s)) throw new Error(`${path.join('.')} is not a list`);
  return s;
}

/** prune removes path's empty mappings and lists, innermost first. */
function prune(doc: Document, path: string[]) {
  for (let i = path.length; i > 0; i--) {
    const n = doc.getIn(path.slice(0, i));
    if ((isMap(n) || isSeq(n)) && n.items.length === 0) doc.deleteIn(path.slice(0, i));
    else break;
  }
}

/** block is text as a literal block scalar, with a final newline. */
function block(doc: Document, text: string): Scalar {
  const s = doc.createNode(text.endsWith('\n') ? text : text + '\n') as Scalar;
  s.type = Scalar.BLOCK_LITERAL;
  return s;
}

/** literal is text as a literal block scalar, exactly: no newline is added. */
function literal(doc: Document, text: string): Scalar {
  const s = doc.createNode(text) as Scalar;
  s.type = Scalar.BLOCK_LITERAL;
  return s;
}

/**
 * setProviderSettings sets mocks.providers.<name> to the settings YAML, a
 * mapping; empty text sets an empty mapping, keeping the entry.
 */
export const setProviderSettings =
  (name: string, text: string): Edit =>
  (doc) => {
    const key = name.trim();
    if (!key) throw new Error('A provider needs its name, as the runtime lists it.');
    let value: unknown = new YAMLMap();
    if (text.trim()) {
      const sub = parseDocument(text);
      if (sub.errors.length) throw new Error(`The settings of ${key} are not valid YAML: ${sub.errors[0].message}`);
      if (!isMap(sub.contents)) throw new Error(`The settings of ${key} must be a mapping (key: value).`);
      value = sub.contents;
    }
    doc.setIn(['mocks', 'providers', key], value);
  };

/**
 * addProviderSettings makes sure mocks.providers.<name> is a mapping and
 * adds the keys of settings it lacks (a provider's proposal), keeping every
 * key it has.
 */
export const addProviderSettings =
  (name: string, settings: Record<string, unknown>): Edit =>
  (doc) => {
    const path = ['mocks', 'providers', name];
    if (!isMap(doc.getIn(path, true))) doc.setIn(path, new YAMLMap());
    const current = doc.getIn(path, true) as YAMLMap;
    for (const [k, v] of Object.entries(settings)) {
      if (!current.has(k)) current.set(k, doc.createNode(v));
    }
  };

/** removeProvider removes mocks.providers.<name>, and the mappings it empties. */
export const removeProvider =
  (name: string): Edit =>
  (doc) => {
    doc.deleteIn(['mocks', 'providers', name]);
    prune(doc, ['mocks', 'providers']);
  };

export type MockList = 'files' | 'kubernetes.objects' | 'kubernetes.files';

const mockPath = (l: MockList) => ['mocks', ...l.split('.')];

/** removeMock removes an entry of a mocks list, and the lists it empties. */
export const removeMock =
  (l: MockList, i: number): Edit =>
  (doc) => {
    doc.deleteIn([...mockPath(l), i]);
    prune(doc, mockPath(l));
  };

/** addFile appends a mocked file at a deployment path with content. */
export const addFile =
  (path: string, content = ''): Edit =>
  (doc) => {
    if (!path.trim()) throw new Error('A mocked file needs its deployment path, for example /var/log/pods/ns_pod_uid/app/0.log.');
    const m = doc.createNode({ path: path.trim() }) as YAMLMap;
    m.set('content', block(doc, content));
    listIn(doc, ['mocks', 'files']).items.push(m);
  };

/**
 * setFileText sets a mocked file's existing or content text, removing the
 * key when the text is empty. Setting content replaces a file reference.
 */
export const setFileText =
  (i: number, key: 'existing' | 'content', text: string): Edit =>
  (doc) => {
    const n = doc.getIn(['mocks', 'files', i]);
    if (!isMap(n)) throw new Error(`mocks.files[${i}] is not a mapping`);
    if (text === '') {
      n.delete(key);
      return;
    }
    if (key === 'content') n.delete('file');
    n.set(key, block(doc, text));
  };

/**
 * addKubeObjects appends the Kubernetes objects of manifest text (one or
 * more YAML documents, as kubectl get -o yaml prints them) to
 * mocks.kubernetes.objects. A List contributes its items.
 */
export const addKubeObjects =
  (manifests: string): Edit =>
  (doc) => {
    const objects: unknown[] = [];
    for (const d of parseAllDocuments(manifests)) {
      if (d.errors.length) throw new Error(`Manifest: ${d.errors[0].message}`);
      const v = d.toJS() as unknown;
      if (v === null || v === undefined) continue;
      const o = v as Record<string, unknown>;
      const items = typeof o.kind === 'string' && o.kind.endsWith('List') && Array.isArray(o.items) ? o.items : [o];
      for (const it of items as Record<string, unknown>[]) {
        const md = it?.metadata as Record<string, unknown> | undefined;
        if (typeof it?.kind !== 'string' || typeof md?.name !== 'string') {
          throw new Error('Each manifest needs a kind and metadata.name.');
        }
        objects.push(it);
      }
    }
    if (!objects.length) throw new Error('The manifests hold no objects.');
    const s = listIn(doc, ['mocks', 'kubernetes', 'objects']);
    for (const o of objects) s.items.push(doc.createNode(o));
  };

/** removeItem removes inputs[i] or assertions[i]. */
export const removeItem =
  (key: 'inputs' | 'assertions', i: number): Edit =>
  (doc) => {
    doc.deleteIn([key, i]);
    const s = doc.get(key);
    if (isSeq(s) && s.items.length === 0) doc.delete(key);
  };

/** inlineData is pretty OTLP JSON as a literal block scalar. */
function inlineData(doc: Document, json: string): Scalar {
  const s = doc.createNode(JSON.stringify(JSON.parse(json), null, 2) + '\n') as Scalar;
  s.type = Scalar.BLOCK_LITERAL;
  return s;
}

/** addInput appends an input from a file path or inline OTLP JSON. */
export const addInput =
  (input: { receiver: string; signal?: string; file?: string; data?: string; raw?: boolean }): Edit =>
  (doc) => {
    const m = doc.createNode(input.receiver ? { receiver: input.receiver } : {}) as YAMLMap;
    if (input.signal) m.set('signal', input.signal);
    if (input.data !== undefined) m.set('data', input.raw ? literal(doc, input.data) : inlineData(doc, input.data));
    else m.set('file', input.file ?? '');
    list(doc, 'inputs').items.push(m);
  };

/** setInputSource makes inputs[i] read a file or inline OTLP JSON. */
export const setInputSource =
  (i: number, src: { file: string } | { data: string }): Edit =>
  (doc) => {
    const n = doc.getIn(['inputs', i]);
    if (!isMap(n)) throw new Error(`inputs[${i}] is not a mapping`);
    if ('file' in src) {
      n.delete('data');
      n.set('file', src.file);
    } else {
      n.delete('file');
      n.set('data', inlineData(doc, src.data));
    }
  };

/**
 * setInputData stores text as inputs[i]'s inline data exactly as typed, so
 * an editor can write each change back without reformatting it; text that
 * is not JSON yet is kept too, and the run says what is wrong with it.
 */
export const setInputData =
  (i: number, text: string): Edit =>
  (doc) => {
    const n = doc.getIn(['inputs', i]);
    if (!isMap(n)) throw new Error(`inputs[${i}] is not a mapping`);
    n.delete('file');
    n.set('data', literal(doc, text));
  };

/** moveItem moves inputs[from] or assertions[from] to index to. */
export const moveItem =
  (key: 'inputs' | 'assertions', from: number, to: number): Edit =>
  (doc) => {
    const s = doc.get(key);
    if (!isSeq(s) || to < 0 || to >= s.items.length || from === to) return;
    const [n] = s.items.splice(from, 1);
    s.items.splice(to, 0, n);
  };

/** duplicateItem inserts a copy of inputs[i] or assertions[i] after it. */
export const duplicateItem =
  (key: 'inputs' | 'assertions', i: number): Edit =>
  (doc) => {
    const s = doc.get(key);
    if (!isSeq(s) || !s.items[i]) return;
    const n = s.items[i];
    s.items.splice(i + 1, 0, (n as YAMLMap).clone());
  };

/** setTarget replaces an assertion's target as a flow mapping. */
export const setTarget =
  (i: number, t: TargetModel): Edit =>
  (doc) => {
    const o: Record<string, unknown> = {};
    if (t.exporter) o.exporter = t.exporter;
    if (t.processor) o.processor = t.processor;
    if (t.endpoint) o.endpoint = { component: t.endpoint.component, path: t.endpoint.path };
    if (t.pipeline) o.pipeline = t.pipeline;
    o.signal = t.signal;
    const m = doc.createNode(o) as YAMLMap;
    // An endpoint target is too long for one flow line: its endpoint stays
    // a flow mapping inside a block one.
    m.flow = !t.endpoint;
    const e = m.get('endpoint', true);
    if (isMap(e)) e.flow = true;
    doc.setIn(['assertions', i, 'target'], m);
  };

/**
 * setConditions replaces an assertion's condition keys with those of a YAML
 * snippet, which may hold only condition keys.
 */
export const setConditions =
  (i: number, snippet: string): Edit =>
  (doc) => {
    const n = doc.getIn(['assertions', i]);
    if (!isMap(n)) throw new Error(`assertions[${i}] is not a mapping`);
    const sub = parseDocument(snippet);
    if (sub.errors.length) throw new Error(sub.errors[0].message);
    const m = sub.contents;
    if (m !== null && !isMap(m)) throw new Error('Conditions must be a YAML mapping.');
    const pairs = (m?.items ?? []) as Pair[];
    for (const p of pairs) {
      const k = isScalar(p.key) ? p.key.value : p.key;
      if (!(CONDITION_KEYS as readonly unknown[]).includes(k)) {
        throw new Error(`Unknown condition ${String(k)}: use ${CONDITION_KEYS.join(', ')}.`);
      }
    }
    for (const k of CONDITION_KEYS) n.delete(k);
    for (const p of pairs) n.items.push(p);
  };

export interface TargetChoice {
  label: string;
  target: TargetModel;
}

/**
 * members resolves a pipeline's component refs (exporter/otlp/pager) to the
 * IDs that scenarios use (otlp/pager), keeping only components of kind:
 * connectors also appear among receivers and exporters.
 */
function members(g: Graph | undefined, refs: string[] | undefined, kind: string): string[] {
  const byRef = new Map((g?.components ?? []).map((c) => [c.ref, c]));
  return (refs ?? []).flatMap((r) => {
    const c = byRef.get(r);
    return c && c.kind === kind ? [c.id] : [];
  });
}

/**
 * targetChoices lists the boundaries an assertion can target: each exporter
 * per signal, each processor instance (processor in pipeline) and, once a
 * run simulated some, each endpoint per signal it received (or, when it
 * received nothing, per signal of the configuration's pipelines).
 */
export function targetChoices(g?: Graph, report?: Report): TargetChoice[] {
  const out: TargetChoice[] = [];
  const seen = new Set<string>();
  for (const p of g?.pipelines ?? []) {
    for (const e of members(g, p.exporters, 'exporter')) {
      const key = `e|${e}|${p.signal}`;
      if (seen.has(key)) continue;
      seen.add(key);
      out.push({ label: `exporter ${e} (${p.signal})`, target: { exporter: e, signal: p.signal } });
    }
  }
  for (const p of g?.pipelines ?? []) {
    for (const proc of members(g, p.processors, 'processor')) {
      out.push({ label: `processor ${proc} in ${p.id}`, target: { processor: proc, pipeline: p.id, signal: p.signal } });
    }
  }
  const res = report?.result;
  const pipelineSignals = [...new Set((g?.pipelines ?? []).map((p) => p.signal))];
  for (const s of res?.simulated_endpoints ?? []) {
    const received = (res?.endpoint_outputs ?? []).filter((o) => o.component === s.component && o.endpoint === s.path).map((o) => o.signal);
    for (const signal of received.length ? [...new Set(received)] : pipelineSignals) {
      const target = { endpoint: { component: s.component, path: s.path }, signal };
      if (seen.has(targetKey(target))) continue;
      seen.add(targetKey(target));
      out.push({ label: `endpoint ${s.path} of ${s.component} (${signal})`, target });
    }
  }
  return out;
}

/** targetKey identifies a target for matching against targetChoices. */
export function targetKey(t: TargetModel): string {
  if (t.exporter) return `e|${t.exporter}|${t.signal}`;
  if (t.endpoint) return `n|${t.endpoint.component}|${t.endpoint.path}|${t.signal}`;
  return `p|${t.processor ?? ''}|${t.pipeline ?? ''}|${t.signal}`;
}

/** signalOfPayload infers an OTLP JSON payload's signal, or undefined. */
export function signalOfPayload(json: string): string | undefined {
  let v: unknown;
  try {
    v = JSON.parse(json);
  } catch {
    return undefined;
  }
  if (!v || typeof v !== 'object') return undefined;
  const o = v as Record<string, unknown>;
  if ('resourceSpans' in o || 'resource_spans' in o) return 'traces';
  if ('resourceMetrics' in o || 'resource_metrics' in o) return 'metrics';
  if ('resourceLogs' in o || 'resource_logs' in o) return 'logs';
  return undefined;
}

/** receiverSignals maps each receiver used by the pipelines to the signals it feeds. */
export function receiverSignals(g?: Graph): Map<string, string[]> {
  const out = new Map<string, string[]>();
  for (const p of g?.pipelines ?? []) {
    for (const r of members(g, p.receivers, 'receiver')) {
      const s = out.get(r) ?? [];
      if (!s.includes(p.signal)) s.push(p.signal);
      out.set(r, s);
    }
  }
  return out;
}

/** receiverIds lists the receivers used by the topology's pipelines. */
export function receiverIds(g?: Graph): string[] {
  return [...new Set((g?.pipelines ?? []).flatMap((p) => members(g, p.receivers, 'receiver')))].sort();
}
