import { isSeq, parseDocument } from 'yaml';
import { describeAssertion, readChecks, targetText } from '../scenario/checks';
import { readScenario } from '../scenario/scenarioDoc';

/**
 * Read-only views of a saved scenario for Saved work: how many assertions it
 * has (the Tests filter) and a summary of its inputs, mocks, assertions and
 * settings for the source detail. Editing stays in the debugger.
 */

/**
 * assertionCount counts the entries of a scenario's `assertions` list, or
 * undefined when the text is not a readable YAML mapping.
 */
export function assertionCount(text: string): number | undefined {
  try {
    const doc = parseDocument(text);
    if (doc.errors.length) return undefined;
    const list = doc.get('assertions');
    return isSeq(list) ? list.items.length : 0;
  } catch {
    return undefined;
  }
}

export interface SummaryLine {
  text: string;
  detail?: string;
}

export interface ScenarioSummary {
  inputs: SummaryLine[];
  mocks: SummaryLine[];
  assertions: SummaryLine[];
  settings: SummaryLine[];
}

/**
 * scenarioSummary lists what a scenario holds in words. Environment values
 * are never shown, only the names the scenario sets.
 */
export function scenarioSummary(text: string): ScenarioSummary | { error: string } {
  const read = readScenario(text);
  if ('error' in read) return read;
  const m = read.model;

  const inputs = m.inputs.map((i, n): SummaryLine => {
    const data = i.file !== undefined ? `file ${i.file}` : i.inlineSize !== undefined ? `inline data, ${i.inlineSize} characters` : 'no data';
    const extra = [i.after && `after ${i.after}`, i.clientAddress && `from ${i.clientAddress}`].filter(Boolean).join(', ');
    return { text: `${n + 1}. ${i.receiver || 'receiver not set'}${i.signal ? ` · ${i.signal}` : ''}`, detail: extra ? `${data} · ${extra}` : data };
  });

  const mocks: SummaryLine[] = [
    ...m.files.map((f) => ({ text: `File ${f.path || '(no path)'}`, detail: f.file !== undefined ? `content from ${f.file}` : f.content ? 'written while the Collector runs' : 'present before it starts' })),
    ...m.kubeObjects.map((o) => ({ text: `${o.kind || 'Object'} ${o.namespace ? `${o.namespace}/` : ''}${o.name}`.trim(), detail: o.items !== undefined ? `${o.items} items` : undefined })),
    ...m.kubeFiles.map((f) => ({ text: `Kubernetes manifests ${f}` })),
    ...m.providers.map((p) => ({ text: `Provider ${p.name}`, detail: p.settings.trim() ? 'with settings' : undefined })),
    ...m.environment.map(([k]) => ({ text: `Environment ${k}`, detail: 'value hidden' })),
  ];

  const assertions = m.assertions.map((a): SummaryLine => {
    const checks = readChecks(a.conditions);
    const sentence = 'checks' in checks ? describeAssertion(a.target, checks.checks) : `${targetText(a.target)} (conditions in YAML)`;
    return a.name ? { text: a.name, detail: sentence } : { text: sentence };
  });

  const settings: SummaryLine[] = [];
  if (m.name) settings.push({ text: 'Name', detail: m.name });
  if (m.timeout) settings.push({ text: 'Timeout', detail: m.timeout });
  if (m.settle) settings.push({ text: 'Settle', detail: m.settle });
  if (m.timestamps) settings.push({ text: 'Timestamps', detail: m.timestamps });

  return { inputs, mocks, assertions, settings };
}
