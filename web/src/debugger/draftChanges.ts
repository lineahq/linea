import type { RunInputs } from '../api/types';
import { runFields, runPlan } from '../input/quickInput';
import type { WorkspaceState } from '../workspace/store';

/** The part of the draft that differs from what a run used. */
export interface PartDifference {
  part: 'configuration' | 'inputs';
  label: string;
  /** Lines present only in the draft. */
  added: string[];
  /** Lines present only in the run's sources. */
  removed: string[];
}

type Drafts = Pick<WorkspaceState, 'configYAML' | 'scenarioYAML'>;

/**
 * lineChanges lists the lines that differ between two texts, as multisets of
 * lines: a moved line is not a change. It states where to look, not a diff.
 */
export function lineChanges(before: string, after: string): { added: string[]; removed: string[] } {
  const count = new Map<string, number>();
  const lines = (t: string) => t.split(/\r?\n/).filter((l) => l.trim() !== '');
  for (const l of lines(before)) count.set(l, (count.get(l) ?? 0) + 1);
  const added: string[] = [];
  for (const l of lines(after)) {
    const n = count.get(l) ?? 0;
    if (n > 0) count.set(l, n - 1);
    else added.push(l);
  }
  const removed: string[] = [];
  for (const [l, n] of count) for (let i = 0; i < n; i++) removed.push(l);
  return { added, removed };
}

const same = (a: string | undefined, b: string | undefined) => (a ?? '').trim() === (b ?? '').trim();

/**
 * draftDifferences compares the drafts with the sources a run was admitted
 * with. An empty draft is not a change: nothing was edited since the run. A
 * quick run compares what it sent (the payload, receiver and mocks); a
 * scenario run compares the scenario text.
 */
export function draftDifferences(ws: Drafts, inputs: RunInputs): PartDifference[] {
  const out: PartDifference[] = [];
  if (ws.configYAML.trim() && !same(ws.configYAML, inputs.config.text)) {
    out.push({ part: 'configuration', label: 'configuration', ...lineChanges(inputs.config.text, ws.configYAML) });
  }
  if (!ws.scenarioYAML.trim()) return out;
  if (inputs.mode === 'scenario') {
    const was = inputs.scenario?.text ?? '';
    if (!same(ws.scenarioYAML, was)) out.push({ part: 'inputs', label: 'inputs, mocks or assertions', ...lineChanges(was, ws.scenarioYAML) });
    return out;
  }
  // A quick run: the draft must still run the same way, with the same payload and mocks.
  const plan = runPlan(ws.scenarioYAML);
  const was = [inputs.fixture?.text ?? '', inputs.receiver ?? '', inputs.setup?.text ?? ''];
  if (plan.mode !== 'quick') {
    out.push({ part: 'inputs', label: 'inputs, mocks or assertions', ...lineChanges(was.join('\n'), ws.scenarioYAML) });
    return out;
  }
  // The inline payload is compared through the fields a run would send.
  const sent = runFields({ scenarioYAML: ws.scenarioYAML });
  if (!('fixture_json' in sent)) return out;
  const now = [sent.fixture_json, sent.receiver ?? '', sent.setup_yaml ?? ''];
  if (now.some((v, i) => !same(v, was[i]))) {
    out.push({ part: 'inputs', label: 'inputs, mocks or assertions', ...lineChanges(was.join('\n'), now.join('\n')) });
  }
  return out;
}

/** differenceSummary words what changed, as the draft-changed banner shows it. */
export function differenceSummary(diffs: PartDifference[]): string {
  if (!diffs.length) return '';
  const parts = diffs.map((d) => {
    const n = d.added.length + d.removed.length;
    return `${d.label} (${n} line${n === 1 ? '' : 's'})`;
  });
  return `${diffs.length} edit${diffs.length === 1 ? '' : 's'}: ${parts.join(' and ')}.`;
}

/** Whether neither draft holds anything: a run opened in a fresh session. */
export function draftsEmpty(ws: Pick<WorkspaceState, 'configYAML' | 'scenarioYAML'>): boolean {
  return !ws.configYAML.trim() && !ws.scenarioYAML.trim();
}
