import { Document, isMap, isScalar, isSeq, parseAllDocuments, parseDocument, stringify, YAMLMap, type Pair } from 'yaml';
import type { Edit } from '../scenario/scenarioDoc';
import { concretePath, EXAMPLE, sampleLog, suggestFile, type LogFormat } from './suggest';

/**
 * Mock editing beyond whole entries: the form fields of Kubernetes objects
 * (written back at their manifest paths, keeping the rest of the object),
 * one object as YAML, and templates for files and objects. The scenario
 * text stays the source of truth.
 */

type Obj = Record<string, unknown>;
const obj = (v: unknown): Obj => (v && typeof v === 'object' && !Array.isArray(v) ? (v as Obj) : {});
const str = (v: unknown) => (v === undefined || v === null ? '' : String(v));

export type Entries = [string, string][];

/** KubeField is a scalar the object form edits. */
export type KubeField = 'name' | 'namespace' | 'uid' | 'ip' | 'node';

/** KubeForm is what the form shows of one mocks.kubernetes.objects entry. */
export interface KubeForm {
  kind: string;
  name: string;
  namespace: string;
  uid: string;
  /** A Pod's status.podIP, which association by IP or connection reads. */
  ip: string;
  /** A Pod's spec.nodeName. */
  node: string;
  labels: Entries;
  annotations: Entries;
  /** Why the object is edited as YAML only. */
  yamlOnly?: string;
}

const OBJECTS = ['mocks', 'kubernetes', 'objects'];
const at = (i: number, ...rest: (string | number)[]) => [...OBJECTS, i, ...rest];

const PATHS: Record<KubeField, string[]> = {
  name: ['metadata', 'name'],
  namespace: ['metadata', 'namespace'],
  uid: ['metadata', 'uid'],
  ip: ['status', 'podIP'],
  node: ['spec', 'nodeName'],
};

/** FIELDS are the form fields of each kind; other kinds get the namespaced ones. */
const FIELDS: Record<string, KubeField[]> = {
  Pod: ['name', 'namespace', 'uid', 'ip', 'node'],
  Namespace: ['name', 'uid'],
  Node: ['name', 'uid'],
};

export const fieldsOf = (kind: string): KubeField[] => FIELDS[kind] ?? ['name', 'namespace', 'uid'];

export const FIELD_LABELS: Record<KubeField, string> = {
  name: 'Name',
  namespace: 'Namespace',
  uid: 'UID',
  ip: 'Pod IP',
  node: 'Node',
};

function entries(v: unknown): Entries | undefined {
  if (v === undefined || v === null) return [];
  if (typeof v !== 'object' || Array.isArray(v)) return undefined;
  return Object.entries(v as Obj).map(([k, x]) => [k, str(x)]);
}

/** readKubeForms reads the form of every mocks.kubernetes.objects entry. */
export function readKubeForms(text: string): KubeForm[] {
  const doc = parseDocument(text);
  if (doc.errors.length) return [];
  const s = doc.getIn(OBJECTS);
  if (!isSeq(s)) return [];
  return s.items.map((n) => {
    const o = obj(isMap(n) ? n.toJS(doc) : undefined);
    const md = obj(o.metadata);
    const kind = str(o.kind);
    const labels = entries(md.labels);
    const annotations = entries(md.annotations);
    const form: KubeForm = {
      kind,
      name: str(md.name),
      namespace: str(md.namespace),
      uid: str(md.uid),
      ip: str(obj(o.status).podIP),
      node: str(obj(o.spec).nodeName),
      labels: labels ?? [],
      annotations: annotations ?? [],
    };
    if (!isMap(n)) form.yamlOnly = 'This entry is not a mapping.';
    else if (kind.endsWith('List')) form.yamlOnly = `A list of ${Array.isArray(o.items) ? o.items.length : 0} objects.`;
    else if (!labels || !annotations) form.yamlOnly = 'Its labels or annotations are not a mapping.';
    return form;
  });
}

/** kubeSummary is one line about an object: where it lives and what it carries. */
export function kubeSummary(f: KubeForm): string {
  const parts = [f.namespace ? `${f.namespace}/${f.name}` : f.name];
  if (f.ip) parts.push(f.ip);
  if (f.node && f.kind === 'Pod') parts.push(`on ${f.node}`);
  if (f.labels.length) parts.push(`${f.labels.length} label${f.labels.length === 1 ? '' : 's'}`);
  if (f.annotations.length) parts.push(`${f.annotations.length} annotation${f.annotations.length === 1 ? '' : 's'}`);
  return parts.join(' · ');
}

function objectAt(doc: Document, i: number): YAMLMap {
  const n = doc.getIn([...OBJECTS, i]);
  if (!isMap(n)) throw new Error(`mocks.kubernetes.objects[${i}] is not a mapping`);
  return n;
}

/** prune removes the empty mappings on a path below the object, innermost first. */
function prune(doc: Document, path: (string | number)[], stop: number) {
  for (let k = path.length; k > stop; k--) {
    const n = doc.getIn(path.slice(0, k));
    if ((isMap(n) || isSeq(n)) && n.items.length === 0) doc.deleteIn(path.slice(0, k));
    else break;
  }
}

/**
 * setKubeField sets a form field at its manifest path, or removes it when
 * empty. A Pod's IP is also its first status.podIPs entry, as the API
 * server reports both.
 */
export const setKubeField =
  (i: number, field: KubeField, value: string): Edit =>
  (doc) => {
    const o = objectAt(doc, i);
    const v = value.trim();
    if (field === 'name' && !v) throw new Error('A Kubernetes object needs a name.');
    const base = at(i);
    const path = [...base, ...PATHS[field]];
    if (v) {
      const cur = doc.getIn(path, true);
      if (isScalar(cur)) cur.value = v;
      else doc.setIn(path, v);
    } else {
      doc.deleteIn(path);
      prune(doc, path.slice(0, -1), base.length);
    }
    if (field !== 'ip' || o.get('kind') !== 'Pod') return;
    const ips = doc.getIn([...base, 'status', 'podIPs']);
    if (!v) {
      if (isSeq(ips)) ips.items.splice(0, 1);
      prune(doc, [...base, 'status', 'podIPs'], base.length);
    } else if (isSeq(ips) && isMap(ips.items[0])) {
      (ips.items[0] as YAMLMap).set('ip', v);
    } else {
      doc.setIn([...base, 'status', 'podIPs'], doc.createNode([{ ip: v }]));
    }
  };

/**
 * setKubeMap replaces an object's labels or annotations. Values stay strings,
 * as Kubernetes requires; unchanged entries keep their nodes.
 */
export const setKubeMap =
  (i: number, key: 'labels' | 'annotations', list: Entries): Edit =>
  (doc) => {
    objectAt(doc, i);
    const kept = list.filter(([k]) => k.trim() !== '').map(([k, v]): [string, string] => [k.trim(), v]);
    const dup = kept.find(([k], j) => kept.findIndex(([o]) => o === k) !== j);
    if (dup) throw new Error(`The ${key} already have ${dup[0]}.`);
    const path = at(i, 'metadata', key);
    if (!kept.length) {
      doc.deleteIn(path);
      prune(doc, path.slice(0, -1), OBJECTS.length + 1);
      return;
    }
    const old = doc.getIn(path);
    const m = new YAMLMap();
    for (const [k, v] of kept) {
      const prev = isMap(old) ? (old.items as Pair[]).find((p) => isScalar(p.key) && p.key.value === k) : undefined;
      if (prev && isScalar(prev.value) && str(prev.value.value) === v) m.items.push(prev);
      else m.items.push(doc.createPair(k, v));
    }
    doc.setIn(path, m);
  };

/** kubeObjectYAML is one object as YAML text. */
export function kubeObjectYAML(text: string, i: number): string {
  const doc = parseDocument(text);
  const n = doc.getIn([...OBJECTS, i]);
  if (!isMap(n)) return '';
  const one = new Document();
  one.contents = n.clone() as YAMLMap;
  return one.toString();
}

/** replaceKubeObject replaces one object with manifest text holding exactly one object. */
export const replaceKubeObject =
  (i: number, manifest: string): Edit =>
  (doc) => {
    objectAt(doc, i);
    const docs = parseAllDocuments(manifest).filter((d) => d.contents !== null);
    if (docs.length !== 1) throw new Error('Write exactly one object here; add more with + Mock.');
    const d = docs[0];
    if (d.errors.length) throw new Error(`Manifest: ${d.errors[0].message}`);
    const o = obj(d.toJS());
    if (!isMap(d.contents) || typeof o.kind !== 'string' || typeof obj(o.metadata).name !== 'string') {
      throw new Error('The object needs a kind and metadata.name.');
    }
    (doc.getIn(OBJECTS) as { items: unknown[] }).items[i] = d.contents;
  };

/** nextName is base, or base-2, base-3… when taken. */
function nextName(base: string, taken: string[]): string {
  return taken.includes(base) ? numbered(base, taken) : base;
}

/** numbered is base-2, base-3 (after any number base ends in) that is not taken. */
function numbered(base: string, taken: string[]): string {
  const m = /^(.*)-(\d+)$/.exec(base);
  const stem = m ? m[1] : base;
  for (let n = m ? Number(m[2]) + 1 : 2; ; n++) if (!taken.includes(`${stem}-${n}`)) return `${stem}-${n}`;
}

/** nextIP is the first example pod IP no object has. */
function nextIP(taken: string[]): string {
  for (let n = 17; n < 255; n++) if (!taken.includes(`10.42.0.${n}`)) return `10.42.0.${n}`;
  return EXAMPLE.ip;
}

const uidFor = (n: number) => EXAMPLE.uid.replace(/..$/, String(n).padStart(2, '0').slice(-2));

/**
 * duplicateKubeObject copies an object after itself, with a name (and for
 * a Pod a UID and IP) of its own, so both are served and associated apart.
 */
export const duplicateKubeObject =
  (i: number, forms: KubeForm[]): Edit =>
  (doc) => {
    const src = objectAt(doc, i);
    const f = forms[i];
    const copy = src.clone() as YAMLMap;
    const s = doc.getIn(OBJECTS) as { items: unknown[] };
    s.items.splice(i + 1, 0, copy);
    const same = forms.filter((x) => x.kind === f.kind && x.namespace === f.namespace).map((x) => x.name);
    setKubeField(i + 1, 'name', numbered(f.name, same))(doc);
    if (f.uid) setKubeField(i + 1, 'uid', uidFor(forms.length))(doc);
    if (f.kind === 'Pod' && f.ip) setKubeField(i + 1, 'ip', nextIP(forms.map((x) => x.ip)))(doc);
  };

export type ObjectTemplate = 'Pod' | 'Namespace' | 'Node' | 'ReplicaSet';

export const OBJECT_TEMPLATES: { kind: ObjectTemplate; detail: string }[] = [
  { kind: 'Pod', detail: 'Name, namespace, UID, IP and labels the processor reads' },
  { kind: 'Namespace', detail: 'For namespace labels and annotations' },
  { kind: 'Node', detail: 'For node labels and annotations' },
  { kind: 'ReplicaSet', detail: "Owner of a Deployment's pods, for k8s.deployment.name" },
];

/** objectTemplate is the manifest of a new object with a name no object of its kind has. */
export function objectTemplate(kind: ObjectTemplate, forms: KubeForm[]): string {
  const names = forms.filter((f) => f.kind === kind).map((f) => f.name);
  const n = forms.length;
  let o: Obj;
  switch (kind) {
    case 'Pod': {
      const name = nextName(EXAMPLE.pod, names);
      const ip = nextIP(forms.map((f) => f.ip));
      o = {
        apiVersion: 'v1',
        kind,
        metadata: { name, namespace: EXAMPLE.namespace, uid: uidFor(n), labels: { app: EXAMPLE.container } },
        spec: { nodeName: EXAMPLE.node, containers: [{ name: EXAMPLE.container, image: `registry.example/${EXAMPLE.container}:1.0.0` }] },
        status: { phase: 'Running', podIP: ip, podIPs: [{ ip }] },
      };
      break;
    }
    case 'Namespace':
      o = { apiVersion: 'v1', kind, metadata: { name: nextName(EXAMPLE.namespace, names), labels: { team: 'payments' } } };
      break;
    case 'Node':
      o = { apiVersion: 'v1', kind, metadata: { name: nextName(EXAMPLE.node, names), uid: uidFor(n), labels: { 'topology.kubernetes.io/zone': 'zone-a' } } };
      break;
    case 'ReplicaSet': {
      const rs = EXAMPLE.pod.split('-').slice(0, -1).join('-');
      o = { apiVersion: 'apps/v1', kind, metadata: { name: nextName(rs, names), namespace: EXAMPLE.namespace, uid: uidFor(n) } };
      break;
    }
  }
  return stringify(o, { aliasDuplicateObjects: false });
}

export type FileTemplate = LogFormat | 'empty';

export const FILE_TEMPLATES: { id: FileTemplate; label: string; detail: string }[] = [
  { id: 'plain', label: 'Log file', detail: 'Plain lines with a timestamp and level' },
  { id: 'container', label: 'Container log (CRI)', detail: 'Kubelet-style lines the container parser reads' },
  { id: 'json', label: 'JSON log', detail: 'One JSON object per line, for json_parser' },
  { id: 'empty', label: 'Empty file', detail: 'Write the content yourself' },
];

/**
 * fileTemplate is a new mocked file: at a path the receiver's include
 * patterns match when there is one (cfg is its effective configuration),
 * else a common path for the format.
 */
export function fileTemplate(id: FileTemplate, cfg: Obj, used: string[]): { path: string; content: string } {
  const include = Array.isArray(cfg.include) && cfg.include.length > 0;
  const fallback = id === 'container' ? { include: [concretePath('/var/log/pods/*/*/*.log')] } : id === 'json' ? { include: ['/var/log/app.json'] } : {};
  const { path } = suggestFile(include ? cfg : fallback, used);
  return { path, content: id === 'empty' ? '' : sampleLog(id) };
}

/**
 * extractedKeys are the label or annotation keys a k8sattributes
 * configuration extracts from objects of a kind (its extract rules with a
 * key, from pod, namespace or node), so the form can offer them.
 */
export function extractedKeys(cfg: Obj, kind: string, key: 'labels' | 'annotations'): string[] {
  const from = kind.toLowerCase();
  const rules = obj(cfg.extract)[key];
  if (!Array.isArray(rules)) return [];
  return [
    ...new Set(
      rules
        .map(obj)
        .filter((r) => String(r.from || 'pod') === from && typeof r.key === 'string')
        .map((r) => String(r.key)),
    ),
  ];
}
