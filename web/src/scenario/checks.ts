import { isMap, isScalar, isSeq, parseDocument, Scalar, visit, type Document, type YAMLMap } from 'yaml';
import type { Event, Report } from '../api/types';
import { CONDITION_KEYS, list, setTarget, targetKey, type Edit, type TargetChoice, type TargetModel } from './scenarioDoc';

/**
 * An assertion's conditions as rows the builder edits: one row per bound
 * and per attribute key. The YAML stays the source of truth: rows are read
 * from it and written back into the assertion's condition keys.
 */

export type BoundOp = 'equals' | 'min' | 'max' | 'between';
export interface Bound {
  op: BoundOp;
  n: number;
  /** The upper limit of between. */
  m?: number;
}

export type AttrScope = 'attributes' | 'resource_attributes';
export type AttrRule = 'present' | 'absent' | 'equals' | 'types';
/** The scalar types an equals condition compares, after the OTLP kinds. */
export type ValueType = 'string' | 'int' | 'double' | 'bool';
export const VALUE_TYPES: ValueType[] = ['string', 'int', 'double', 'bool'];
/** The attribute types a types condition accepts (scenario.AttributeTypes). */
export const ATTRIBUTE_TYPES = ['string', 'bool', 'int', 'double', 'bytes', 'slice', 'map'];

export type Check =
  | { kind: 'count' | 'dropped'; bound: Bound }
  | {
      kind: 'attribute';
      scope: AttrScope;
      rule: AttrRule;
      key: string;
      /** equals: the value as typed; types: the type name. */
      value?: string;
      valueType?: ValueType;
    };

export type ReadChecks = { checks: Check[] } | { unsupported: string };

const isInt = (s: string) => /^[-+]?\d+$/.test(s.trim());
const isDouble = (s: string) => s.trim() !== '' && Number.isFinite(Number(s));

function readBound(v: unknown): Bound | undefined {
  if (!isMap(v)) return undefined;
  const get = (k: string) => {
    const n = v.get(k, true);
    return isScalar(n) && typeof n.value === 'number' && Number.isInteger(n.value) ? n.value : undefined;
  };
  for (const p of v.items) {
    const k = isScalar(p.key) ? p.key.value : p.key;
    if (k !== 'equals' && k !== 'min' && k !== 'max') return undefined;
    if (!isScalar(p.value) || typeof p.value.value !== 'number' || !Number.isInteger(p.value.value)) return undefined;
  }
  const [eq, min, max] = [get('equals'), get('min'), get('max')];
  if (eq !== undefined && min === undefined && max === undefined) return { op: 'equals', n: eq };
  if (eq !== undefined) return undefined;
  if (min !== undefined && max !== undefined) return { op: 'between', n: min, m: max };
  if (min !== undefined) return { op: 'min', n: min };
  if (max !== undefined) return { op: 'max', n: max };
  return undefined;
}

/** scalarType names a parsed YAML scalar's type as the Go decoder sees it. */
function scalarType(s: Scalar): ValueType | undefined {
  if (typeof s.value === 'boolean') return 'bool';
  if (typeof s.value === 'string') return 'string';
  if (typeof s.value === 'number') {
    const src = (s as Scalar & { source?: string }).source ?? String(s.value);
    return Number.isInteger(s.value) && !/[.eEnN]/.test(src) ? 'int' : 'double';
  }
  return undefined;
}

function readAttributes(scope: AttrScope, v: unknown): Check[] | undefined {
  if (!isMap(v)) return undefined;
  const out: Check[] = [];
  for (const p of v.items) {
    const rule = isScalar(p.key) ? p.key.value : p.key;
    if (rule === 'present' || rule === 'absent') {
      if (!isSeq(p.value)) return undefined;
      for (const k of p.value.items) {
        if (!isScalar(k) || typeof k.value !== 'string') return undefined;
        out.push({ kind: 'attribute', scope, rule, key: k.value });
      }
    } else if (rule === 'equals' || rule === 'types') {
      if (!isMap(p.value)) return undefined;
      for (const q of p.value.items) {
        if (!isScalar(q.key) || !isScalar(q.value)) return undefined;
        const key = String(q.key.value);
        if (rule === 'types') {
          if (typeof q.value.value !== 'string') return undefined;
          out.push({ kind: 'attribute', scope, rule, key, value: q.value.value });
        } else {
          const t = scalarType(q.value);
          if (!t) return undefined;
          out.push({ kind: 'attribute', scope, rule, key, value: String(q.value.value), valueType: t });
        }
      }
    } else return undefined;
  }
  return out;
}

function hasComments(doc: Document): boolean {
  if (doc.commentBefore || doc.comment) return true;
  let found = false;
  visit(doc, (_, n) => {
    const c = n as { commentBefore?: string | null; comment?: string | null };
    if (c.commentBefore || c.comment) {
      found = true;
      return visit.BREAK;
    }
  });
  return found;
}

/**
 * readChecks reads an assertion's conditions text (AssertionModel.conditions)
 * into rows, or says why only the YAML can edit it: rows would rewrite
 * comments and shapes they cannot represent.
 */
export function readChecks(conditions: string): ReadChecks {
  if (!conditions.trim()) return { checks: [] };
  const doc = parseDocument(conditions);
  if (doc.errors.length) return { unsupported: doc.errors[0].message };
  if (hasComments(doc)) return { unsupported: 'Its conditions have comments, which only the YAML keeps.' };
  const m = doc.contents;
  if (!isMap(m)) return { unsupported: 'Its conditions are not a mapping.' };
  const checks: Check[] = [];
  for (const k of CONDITION_KEYS) {
    if (!m.has(k)) continue;
    const v = m.get(k, true);
    if (k === 'count' || k === 'dropped') {
      const bound = readBound(v);
      if (!bound) return { unsupported: `Its ${k} is not one of equals, min, max or both.` };
      checks.push({ kind: k, bound });
    } else {
      const rows = readAttributes(k, v);
      if (!rows) return { unsupported: `Its ${k} has a shape the rows cannot show.` };
      checks.push(...rows);
    }
  }
  return { checks };
}

/** checkProblem says why a row cannot be written yet, or undefined. */
export function checkProblem(c: Check, target: TargetModel): string | undefined {
  if (c.kind !== 'attribute') {
    if (c.kind === 'dropped' && !target.processor) return 'Dropped applies to processor targets only.';
    const { n, m, op } = c.bound;
    if (!Number.isInteger(n) || n < 0) return 'Enter a whole number, 0 or more.';
    if (op === 'between') {
      if (m === undefined || !Number.isInteger(m) || m < 0) return 'Enter a whole number, 0 or more.';
      if (n > m) return `${n} is more than ${m}.`;
    }
    return undefined;
  }
  if (!c.key.trim()) return 'Enter the attribute key.';
  if (c.rule === 'equals') {
    const v = c.value ?? '';
    if (c.valueType === 'int' && !isInt(v)) return 'Enter a whole number, or choose another type.';
    if (c.valueType === 'double' && !isDouble(v)) return 'Enter a number, or choose another type.';
    if (c.valueType === 'bool' && v !== 'true' && v !== 'false') return 'Choose true or false.';
  }
  if (c.rule === 'types' && !ATTRIBUTE_TYPES.includes(c.value ?? '')) return 'Choose a type.';
  return undefined;
}

/** sameCheck compares rows by what they write, so 2.0 and 2 as doubles agree. */
export function sameCheck(a: Check, b: Check): boolean {
  if (a.kind !== 'attribute' || b.kind !== 'attribute')
    return (
      a.kind === b.kind && a.kind !== 'attribute' && b.kind !== 'attribute' && a.bound.op === b.bound.op && a.bound.n === b.bound.n && a.bound.m === b.bound.m
    );
  const num = a.rule === 'equals' && (a.valueType === 'int' || a.valueType === 'double');
  const v = (c: typeof a) => (num ? String(Number(c.value)) : (c.value ?? ''));
  return a.scope === b.scope && a.rule === b.rule && a.key === b.key && (a.valueType ?? '') === (b.valueType ?? '') && v(a) === v(b);
}

/** conflicts finds rows that contradict or repeat another row of the assertion. */
export function conflicts(checks: Check[]): Map<number, string> {
  const out = new Map<number, string>();
  checks.forEach((c, i) => {
    checks.slice(0, i).forEach((d) => {
      if (c.kind !== 'attribute' && d.kind === c.kind) out.set(i, `A second ${c.kind} replaces the first.`);
      if (c.kind !== 'attribute' || d.kind !== 'attribute' || c.scope !== d.scope || c.key !== d.key || !c.key) return;
      if (c.rule === d.rule) out.set(i, `${c.key} already has this condition.`);
      else if ((c.rule === 'present' && d.rule === 'absent') || (c.rule === 'absent' && d.rule === 'present'))
        out.set(i, `${c.key} cannot be both present and absent.`);
    });
  });
  return out;
}

function boundNode(doc: Document, b: Bound) {
  const o = b.op === 'between' ? { min: b.n, max: b.m } : { [b.op]: b.n };
  const n = doc.createNode(o) as YAMLMap;
  n.flow = true;
  return n;
}

function valueNode(c: Extract<Check, { kind: 'attribute' }>): Scalar {
  const v = c.value ?? '';
  switch (c.valueType) {
    case 'int':
      return new Scalar(parseInt(v, 10));
    case 'double': {
      const s = new Scalar(Number(v));
      if (Number.isInteger(s.value)) s.minFractionDigits = 1;
      return s;
    }
    case 'bool':
      return new Scalar(v === 'true');
  }
  // The stringifier quotes a string that would read as another type.
  return new Scalar(v);
}

/** conditionsOf builds the condition keys rows describe, in schema order. */
function conditionsOf(doc: Document, checks: Check[]): Map<string, unknown> {
  const out = new Map<string, unknown>();
  for (const c of checks) {
    if (c.kind !== 'attribute') {
      out.set(c.kind, boundNode(doc, c.bound));
      continue;
    }
    let a = out.get(c.scope) as YAMLMap | undefined;
    if (!a) out.set(c.scope, (a = doc.createNode({}) as YAMLMap));
    if (c.rule === 'present' || c.rule === 'absent') {
      let s = a.get(c.rule, true) as ReturnType<typeof doc.createNode> | undefined;
      if (!isSeq(s)) {
        s = doc.createNode([]);
        (s as { flow: boolean }).flow = true;
        a.set(c.rule, s);
      }
      if (isSeq(s) && !s.items.some((k) => isScalar(k) && k.value === c.key)) s.items.push(doc.createNode(c.key));
    } else {
      let m: unknown = a.get(c.rule, true);
      if (!isMap(m)) a.set(c.rule, (m = doc.createNode({})));
      (m as YAMLMap).set(doc.createNode(c.key), c.rule === 'types' ? doc.createNode(c.value ?? '') : valueNode(c));
    }
  }
  return new Map(CONDITION_KEYS.filter((k) => out.has(k)).map((k) => [k, out.get(k)]));
}

/** setChecks replaces assertions[i]'s condition keys with those of the rows. */
export const setChecks =
  (i: number, checks: Check[]): Edit =>
  (doc) => {
    const n = doc.getIn(['assertions', i], true);
    if (!isMap(n)) throw new Error(`assertions[${i}] is not a mapping`);
    for (const k of CONDITION_KEYS) n.delete(k);
    for (const [k, v] of conditionsOf(doc, checks)) n.set(k, v);
  };

/** appendAssertion adds an assertion with a target, rows and an optional name. */
export const appendAssertion =
  (target: TargetModel, checks: Check[], name = ''): Edit =>
  (doc) => {
    const items = list(doc, 'assertions').items;
    items.push(doc.createNode(name ? { name, target: {} } : { target: {} }));
    setTarget(items.length - 1, target)(doc);
    setChecks(items.length - 1, checks)(doc);
  };

const noun = (signal: string, n: number) =>
  signal === 'traces' ? (n === 1 ? 'span' : 'spans') : signal === 'metrics' ? (n === 1 ? 'data point' : 'data points') : n === 1 ? 'log record' : 'log records';

function boundText(b: Bound): string {
  return b.op === 'equals' ? `${b.n}` : b.op === 'min' ? `at least ${b.n}` : b.op === 'max' ? `at most ${b.n}` : `${b.n} to ${b.m}`;
}

export function targetText(t: TargetModel): string {
  if (t.exporter !== undefined) return `exporter ${t.exporter}`;
  if (t.endpoint) return `endpoint ${t.endpoint.path} of ${t.endpoint.component}`;
  return `processor ${t.processor ?? ''}${t.pipeline ? ` in ${t.pipeline}` : ''}`;
}

/** describeAssertion words an assertion in one line, for its default name. */
export function describeAssertion(t: TargetModel, checks: Check[]): string {
  const parts = checks.map((c) => {
    if (c.kind === 'count') {
      const one = c.bound.op === 'equals' && c.bound.n === 1;
      return `${boundText(c.bound)} ${noun(t.signal, one ? 1 : 2)} ${one ? 'reaches' : 'reach'} ${targetText(t)}`;
    }
    if (c.kind !== 'attribute') return `${targetText(t)} drops ${boundText(c.bound)}`;
    const where = c.scope === 'resource_attributes' ? 'resource ' : '';
    if (c.rule === 'present') return `${where}${c.key || '…'} is set`;
    if (c.rule === 'absent') return `${where}${c.key || '…'} is removed`;
    if (c.rule === 'types') return `${where}${c.key || '…'} is ${c.value ?? '…'}`;
    return `${where}${c.key || '…'} = ${c.valueType === 'string' || !c.valueType ? JSON.stringify(c.value ?? '') : c.value}`;
  });
  if (!parts.length) return `nothing checked at ${targetText(t)}`;
  const s = parts.join(', ');
  return checks.some((c) => c.kind !== 'attribute') || !t.exporter ? s : `${s} at ${t.exporter}`;
}

/** The ways the "+ check" menu adds a row, by label. */
export const NEW_CHECKS: { label: string; detail: string; check: Check; processorOnly?: boolean }[] = [
  { label: 'Record count', detail: 'How many records reach the target', check: { kind: 'count', bound: { op: 'equals', n: 1 } } },
  {
    label: 'Records dropped',
    detail: 'How many records the processor received but did not emit',
    check: { kind: 'dropped', bound: { op: 'min', n: 1 } },
    processorOnly: true,
  },
  { label: 'Attribute is set', detail: 'Every record has the key', check: { kind: 'attribute', scope: 'attributes', rule: 'present', key: '' } },
  { label: 'Attribute is removed', detail: 'No record has the key', check: { kind: 'attribute', scope: 'attributes', rule: 'absent', key: '' } },
  {
    label: 'Attribute equals',
    detail: 'Every record has the key with this value and type',
    check: { kind: 'attribute', scope: 'attributes', rule: 'equals', key: '', value: '', valueType: 'string' },
  },
  {
    label: 'Attribute type',
    detail: 'Every record has the key with this type',
    check: { kind: 'attribute', scope: 'attributes', rule: 'types', key: '', value: 'string' },
  },
  {
    label: 'Resource attribute is set',
    detail: 'Every resource has the key',
    check: { kind: 'attribute', scope: 'resource_attributes', rule: 'present', key: '' },
  },
  {
    label: 'Resource attribute equals',
    detail: 'Every resource has the key with this value',
    check: { kind: 'attribute', scope: 'resource_attributes', rule: 'equals', key: '', value: '', valueType: 'string' },
  },
];

export interface AssertionTemplate {
  id: string;
  label: string;
  detail: string;
  /** The target kind it needs. */
  needs: 'exporter' | 'processor';
  checks: Check[];
}

export const ASSERTION_TEMPLATES: AssertionTemplate[] = [
  {
    id: 'count',
    label: 'Count at an exporter',
    detail: 'So many records reach an exporter',
    needs: 'exporter',
    checks: [{ kind: 'count', bound: { op: 'equals', n: 1 } }],
  },
  {
    id: 'removed',
    label: 'Attribute removed',
    detail: 'A key no longer reaches the exporter, such as a secret',
    needs: 'exporter',
    checks: [{ kind: 'attribute', scope: 'attributes', rule: 'absent', key: '' }],
  },
  {
    id: 'equals',
    label: 'Attribute equals',
    detail: 'Every exported record has a key with a value',
    needs: 'exporter',
    checks: [{ kind: 'attribute', scope: 'attributes', rule: 'equals', key: '', value: '', valueType: 'string' }],
  },
  {
    id: 'drops',
    label: 'Processor drops records',
    detail: 'A filter or sampler drops at least one record',
    needs: 'processor',
    checks: [{ kind: 'dropped', bound: { op: 'min', n: 1 } }],
  },
];

/** DROPPING are processor types that exist to drop records, offered first. */
const DROPPING = ['filter', 'tail_sampling', 'probabilistic_sampler'];
const typeOf = (id: string) => id.split('/')[0];

/** templateTarget picks the target a template starts with, or undefined. */
export function templateTarget(t: AssertionTemplate, choices: TargetChoice[]): TargetModel | undefined {
  if (t.needs === 'exporter') return choices.find((c) => c.target.exporter)?.target;
  const procs = choices.filter((c) => c.target.processor);
  return (procs.find((c) => DROPPING.includes(typeOf(c.target.processor!))) ?? procs[0])?.target;
}

export interface Suggestion {
  /** A stable key, for selection. */
  key: string;
  target: TargetModel;
  checks: Check[];
  /** Already asserted by the scenario. */
  present: boolean;
}

/**
 * suggestFromRun proposes what a finished run observed, for the targets the
 * current configuration still has: the records each recorded exporter and
 * each simulated endpoint received and, for each processor that dropped
 * some, how many. The report
 * is the evidence; nothing is re-run.
 */
export function suggestFromRun(report: Report | undefined, choices: TargetChoice[], existing: { target: TargetModel; conditions: string }[]): Suggestion[] {
  const result = report?.result;
  if (!result) return [];
  const asserted = (t: TargetModel, kind: string) =>
    existing.some((e) => {
      const r = readChecks(e.conditions);
      return targetKey(e.target) === targetKey(t) && 'checks' in r && r.checks.some((c) => c.kind === kind);
    });
  const out: Suggestion[] = [];
  for (const o of result.outputs ?? []) {
    const c = choices.find((x) => x.target.exporter === o.exporter && x.target.signal === o.signal);
    if (!c) continue;
    out.push({
      key: `count|${targetKey(c.target)}`,
      target: c.target,
      checks: [{ kind: 'count', bound: { op: 'equals', n: o.counts.items } }],
      present: asserted(c.target, 'count'),
    });
  }
  for (const o of result.endpoint_outputs ?? []) {
    const c = choices.find((x) => x.target.endpoint?.component === o.component && x.target.endpoint.path === o.endpoint && x.target.signal === o.signal);
    if (!c) continue;
    out.push({
      key: `count|${targetKey(c.target)}`,
      target: c.target,
      checks: [{ kind: 'count', bound: { op: 'equals', n: o.counts.items } }],
      present: asserted(c.target, 'count'),
    });
  }
  const totals = new Map<string, { in: number; out: number }>();
  for (const e of result.events ?? ([] as Event[])) {
    if (e.kind !== 'stage.input' && e.kind !== 'stage.output') continue;
    const k = `${e.component}|${e.pipeline}|${e.signal}`;
    const t = totals.get(k) ?? { in: 0, out: 0 };
    if (e.kind === 'stage.input') t.in += e.counts?.items ?? 0;
    else t.out += e.counts?.items ?? 0;
    totals.set(k, t);
  }
  // Drops are certain only once the run completed: buffered records may still be in flight.
  if (result.status === 'completed') {
    for (const c of choices) {
      const t = c.target;
      if (!t.processor) continue;
      const n = totals.get(`${t.processor}|${t.pipeline}|${t.signal}`);
      if (!n || n.in <= n.out) continue;
      out.push({
        key: `dropped|${targetKey(t)}`,
        target: t,
        checks: [{ kind: 'dropped', bound: { op: 'equals', n: n.in - n.out } }],
        present: asserted(t, 'dropped'),
      });
    }
  }
  return out;
}

/** attributeKeys lists the record and resource attribute keys of OTLP JSON payloads. */
export function attributeKeys(payloads: unknown[]): { attributes: string[]; resource_attributes: string[] } {
  const rec = new Set<string>();
  const res = new Set<string>();
  const keys = (attrs: unknown, into: Set<string>) => {
    if (Array.isArray(attrs)) for (const a of attrs) if (a && typeof (a as { key?: unknown }).key === 'string') into.add((a as { key: string }).key);
  };
  const walk = (v: unknown, depth: number) => {
    if (!v || typeof v !== 'object' || depth > 8) return;
    if (Array.isArray(v)) {
      for (const x of v) walk(x, depth + 1);
      return;
    }
    for (const [k, x] of Object.entries(v)) {
      if (k === 'resource') keys((x as { attributes?: unknown })?.attributes, res);
      else if (k === 'attributes') keys(x, rec);
      else if (k !== 'scope') walk(x, depth + 1);
    }
  };
  for (const p of payloads) walk(p, 0);
  return { attributes: [...rec].sort(), resource_attributes: [...res].sort() };
}
