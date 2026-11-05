import { isScalar, isSeq, parse, parseDocument, type Pair, type YAMLMap } from 'yaml';
import type { WorkspaceState } from '../workspace/store';
import { addInput, editScenario, readScenario, setField, type ScenarioModel } from '../scenario/scenarioDoc';

/**
 * The workspace edits one document, the scenario: its inputs, mocks and
 * assertions. A quick run needs no more than a configuration and one
 * payload, so a document with a single inline input and no assertions runs
 * as a quick run; anything more runs as a scenario. Any input can also run
 * on its own, with the document's mocks.
 */

export type RunMode = 'quick' | 'scenario';

/** RunPlan is what the run button submits for the document. */
export type RunPlan = { mode: 'quick'; input: number } | { mode: 'scenario' };

/** runPlan decides how the document runs as a whole. */
export function runPlan(yaml: string): RunPlan {
  const r = readScenario(yaml);
  if ('error' in r) return { mode: 'scenario' };
  const { inputs, assertions } = r.model;
  return inputs.length === 1 && !assertions.length && inputs[0].data !== undefined ? { mode: 'quick', input: 0 } : { mode: 'scenario' };
}

/** hasMocks reports whether the model supplies any fixture or environment value. */
export function hasMocks(m: ScenarioModel): boolean {
  return m.files.length + m.kubeObjects.length + m.kubeFiles.length + m.environment.length > 0;
}

export interface JSONProblem {
  message: string;
  line: number;
  column: number;
}

/** jsonProblem locates a JSON syntax error, or returns undefined. */
export function jsonProblem(text: string): JSONProblem | undefined {
  try {
    JSON.parse(text);
    return undefined;
  } catch (e) {
    const message = e instanceof Error ? e.message : String(e);
    const lc = /line (\d+) column (\d+)/.exec(message);
    if (lc) return { message, line: Number(lc[1]), column: Number(lc[2]) };
    const pos = /position (\d+)/.exec(message);
    const at = pos ? Math.min(Number(pos[1]), text.length) : text.length;
    const before = text.slice(0, at).split('\n');
    return { message, line: before.length, column: before[before.length - 1].length + 1 };
  }
}

export interface PayloadSummary {
  signal: 'traces' | 'metrics' | 'logs';
  resources: number;
  /** Spans, log records or metric data points. */
  items: number;
}

const SHAPES = [
  { signal: 'traces', keys: ['resourceSpans', 'resource_spans'], scopes: ['scopeSpans', 'scope_spans'], items: ['spans'] },
  { signal: 'logs', keys: ['resourceLogs', 'resource_logs'], scopes: ['scopeLogs', 'scope_logs'], items: ['logRecords', 'log_records'] },
  { signal: 'metrics', keys: ['resourceMetrics', 'resource_metrics'], scopes: ['scopeMetrics', 'scope_metrics'], items: ['metrics'] },
] as const;

type Obj = Record<string, unknown>;
const pick = (o: Obj, keys: readonly string[]): unknown[] => {
  for (const k of keys) if (Array.isArray(o[k])) return o[k] as unknown[];
  return [];
};
const objects = (v: unknown[]): Obj[] => v.filter((x): x is Obj => !!x && typeof x === 'object');

function dataPoints(metric: Obj): number {
  for (const kind of ['gauge', 'sum', 'histogram', 'exponentialHistogram', 'exponential_histogram', 'summary']) {
    const m = metric[kind];
    if (m && typeof m === 'object') return pick(m as Obj, ['dataPoints', 'data_points']).length;
  }
  return 0;
}

/**
 * summarizePayload names an OTLP JSON payload's signal and counts its
 * resources and items, or returns an error message for the user.
 */
export function summarizePayload(text: string): PayloadSummary | { error: string } {
  if (!text.trim()) return { error: 'Paste or open an OTLP JSON payload.' };
  const p = jsonProblem(text);
  if (p) return { error: `Not valid JSON (line ${p.line}): ${p.message}` };
  const v: unknown = JSON.parse(text);
  if (!v || typeof v !== 'object' || Array.isArray(v)) return { error: 'An OTLP JSON payload is an object.' };
  for (const s of SHAPES) {
    const rs = objects(pick(v as Obj, s.keys));
    if (!s.keys.some((k) => k in (v as Obj))) continue;
    let items = 0;
    for (const r of rs) {
      for (const sc of objects(pick(r, s.scopes))) {
        const xs = objects(pick(sc, s.items));
        items += s.signal === 'metrics' ? xs.reduce((n, m) => n + dataPoints(m), 0) : xs.length;
      }
    }
    return { signal: s.signal, resources: rs.length, items };
  }
  return { error: 'No resourceSpans, resourceLogs or resourceMetrics: is this OTLP JSON?' };
}

/**
 * runBlocker says why the document cannot run, if it cannot: with input
 * set, why that input cannot run on its own.
 */
export function runBlocker(ws: Pick<WorkspaceState, 'scenarioYAML'>, configValid: boolean, input?: number): string | undefined {
  if (!configValid) return 'The configuration is not valid';
  const r = readScenario(ws.scenarioYAML);
  // Unreadable text is sent, so the run explains what is wrong with it.
  if ('error' in r) return input === undefined ? undefined : `The scenario YAML cannot be read: ${r.error}`;
  const { inputs } = r.model;
  if (input !== undefined) {
    const i = inputs[input];
    if (!i) return 'There is no such input';
    if (i.data === undefined) return i.file ? undefined : 'Name a file or add OTLP JSON';
    const s = summarizePayload(i.data);
    return 'error' in s ? s.error : undefined;
  }
  if (!inputs.length) return hasMocks(r.model) ? undefined : 'Add an input to run';
  const plan = runPlan(ws.scenarioYAML);
  return plan.mode === 'quick' ? runBlocker(ws, true, plan.input) : undefined;
}

/** setupInUse reports whether quick-run setup text declares anything to apply. */
export function setupInUse(yaml: string): boolean {
  try {
    const v = parse(yaml) as Record<string, unknown> | null;
    return !!v && typeof v === 'object' && Object.keys(v).some((k) => k !== 'version' && v[k] !== null);
  } catch {
    // Invalid text is sent, so the run explains what is wrong with it.
    return yaml.trim() !== '';
  }
}

/** QuickFields are the run request fields of a quick run. */
export interface QuickFields {
  fixture_json: string;
  receiver?: string;
  client_address?: string;
  setup_yaml?: string;
}

/** ScenarioFields are the run request fields of a scenario run. */
export interface ScenarioFields {
  scenario_yaml: string;
}

/**
 * runFields are the run request fields that run the document (input
 * undefined) or one of its inputs alone. An inline input runs as a quick run
 * with the document's mocks, environment and settings; an input read from a
 * file runs as a scenario of that input only, without assertions.
 */
export function runFields(ws: Pick<WorkspaceState, 'scenarioYAML'>, input?: number): QuickFields | ScenarioFields {
  const scenario = (yaml: string): ScenarioFields => ({ scenario_yaml: yaml });
  let at = input;
  if (at === undefined) {
    const plan = runPlan(ws.scenarioYAML);
    if (plan.mode === 'scenario') return scenario(ws.scenarioYAML);
    at = plan.input;
  }
  const r = readScenario(ws.scenarioYAML);
  if ('error' in r) return scenario(ws.scenarioYAML);
  const i = r.model.inputs[at];
  if (i?.data === undefined) {
    const only = editScenario(ws.scenarioYAML, (doc) => {
      const all = doc.get('inputs');
      if (isSeq(all)) all.items = all.items.slice(at, at + 1);
      doc.delete('assertions');
      doc.deleteIn(['inputs', 0, 'after']);
    });
    return scenario('error' in only ? ws.scenarioYAML : only.yaml);
  }
  const setup = scenarioSetup(ws.scenarioYAML);
  return {
    fixture_json: i.data,
    receiver: i.receiver || undefined,
    client_address: i.clientAddress.trim() || undefined,
    setup_yaml: setup || undefined,
  };
}

/**
 * quickDoc builds the document of a quick run: its mocks and environment,
 * with the payload as the only input and, when the run had them, its
 * assertions.
 */
export function quickDoc(q: {
  fixture: string;
  receiver: string;
  clientAddress?: string;
  setup?: string;
  assertions?: string;
  name?: string;
}): { yaml: string } | { error: string } {
  const base = editScenario(q.setup ?? '', (doc) => {
    if (!doc.has('version')) doc.set('version', 1);
    if (q.name) doc.set('name', q.name);
    // version and name lead the file, as in a hand-written scenario.
    const contents = doc.contents as YAMLMap;
    const first = (k: string) => (contents.items as Pair[]).findIndex((p) => (isScalar(p.key) ? p.key.value : p.key) === k);
    for (const k of ['name', 'version']) if (first(k) >= 0) contents.items.unshift(...contents.items.splice(first(k), 1));
  });
  if ('error' in base) return base;
  return editScenario(base.yaml, (doc) => {
    addInput({ receiver: q.receiver, data: q.fixture, raw: true })(doc);
    if (q.clientAddress?.trim()) setField(['inputs', 0, 'client_address'], q.clientAddress.trim())(doc);
    if (q.assertions?.trim()) {
      const a = parseDocument(q.assertions);
      if (a.errors.length) throw new Error(`Assertions: ${a.errors[0].message}`);
      const list = a.get('assertions', true);
      if (list) doc.set('assertions', list);
    }
  });
}

/**
 * scenarioSetup is the part of a scenario a quick run can use: its mocks,
 * environment, execution settings and declared capture origins, without name,
 * inputs or assertions. Origins remain private captured source metadata.
 * Text that is not a scenario mapping yields nothing.
 */
export function scenarioSetup(yaml: string): string {
  const r = editScenario(yaml, (doc) => {
    for (const k of ['name', 'inputs', 'assertions']) doc.delete(k);
  });
  if ('error' in r) return '';
  return setupInUse(r.yaml) ? r.yaml : '';
}
