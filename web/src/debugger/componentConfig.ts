// Pure helpers of the component pane: a component's configuration as written
// in the document, and the "How it will run" explanation shown before a run.
import { parseDocument } from 'yaml';
import type { Graph, OTTLReport, TopologyComponent } from '../api/types';
import { modeInfo } from '../graph/lanes';

/**
 * asWritten extracts a component's own block from a configuration document,
 * exactly as the text has it (comments and quoting kept), without its key.
 * Undefined when the text does not parse or has no such component; empty
 * when the component is declared without settings.
 */
export function asWritten(text: string, kind: string, id: string): string | undefined {
  const doc = parseDocument(text);
  if (doc.errors.length) return undefined;
  const path = [`${kind}s`, id];
  if (!doc.hasIn(path)) return undefined;
  const node = doc.getIn(path, true) as { range?: [number, number, number] | null } | null | undefined;
  if (!node || typeof node !== 'object' || !node.range) return '';
  const lines = text.slice(node.range[0], node.range[1]).replace(/\s+$/, '').split('\n');
  const rest = lines.slice(1).filter((l) => l.trim());
  // A block starts at its first key, so that key's column is the indent to remove.
  const column = node.range[0] - (text.lastIndexOf('\n', node.range[0] - 1) + 1);
  const indent = rest.length ? Math.min(column, ...rest.map((l) => l.length - l.trimStart().length)) : 0;
  return [lines[0], ...lines.slice(1).map((l) => l.slice(Math.min(indent, l.length - l.trimStart().length)))].join('\n');
}

export interface HowItRuns {
  title: string;
  body: string;
  /** A stand-in is a warning; every other mode is an explanation. */
  warn: boolean;
  facts: [string, string][];
  pipelines: { id: string; position?: string }[];
  available: { ok: boolean; text: string };
}

const sentence = (s: string) => (/[.!?]$/.test(s) ? s : `${s}.`);

/** howItRuns explains how a component will execute in the next run, from its execution mode. */
export function howItRuns(c: TopologyComponent, graph?: Graph): HowItRuns {
  const what = `${c.type} ${c.kind}`;
  const note = c.note ? ` ${sentence(c.note)}` : '';
  let title: string;
  let body: string;
  switch (c.mode) {
    case 'real':
      title = 'Real component code';
      body = `The actual ${what} runs exactly as the Collector runs it.${note}`;
      break;
    case 'dependency_mocked':
      title = 'Real component code, mocked environment';
      body = `The actual ${what} runs. ${c.note ? sentence(c.note) : 'The service it depends on is answered by a mock.'} Results are as faithful as the mock.`;
      break;
    case 'captured':
      title = 'Output captured, not sent';
      body = `What reaches ${c.id} is recorded in the run. Nothing is sent to its endpoint.${note}`;
      break;
    case 'simulated':
      title = 'Simulated';
      body = `${modeInfo('simulated').meaning}${note}`;
      break;
    case 'stand_in':
      title = 'Would run as a stand-in';
      body = `${c.id} is not in the selected runtime. A pass-through stand-in would take its place, so its work would not happen and results would not show what production does.${note}`;
      break;
    default:
      title = modeInfo(c.mode).label;
      body = `${modeInfo(c.mode).meaning}${note}`;
  }
  const facts: [string, string][] = [];
  if (c.provider) facts.push(['Provided by', c.provider]);
  if (c.stand_in_values?.length) facts.push(['Stand-in values', c.stand_in_values.map((v) => '${' + v + '}').join(', ')]);
  const pipelines = (c.pipelines ?? []).map((id) => {
    const list = graph?.pipelines?.find((p) => p.id === id)?.processors ?? [];
    const at = list.indexOf(c.ref);
    return { id, position: c.kind === 'processor' && at >= 0 ? `position ${at + 1} of ${list.length} processor${list.length === 1 ? '' : 's'}` : undefined };
  });
  const standIn = c.mode === 'stand_in';
  return {
    title,
    body,
    warn: standIn,
    facts,
    pipelines,
    available: standIn
      ? { ok: false, text: 'Not available in the selected runtime' }
      : { ok: true, text: 'Available in the selected runtime' },
  };
}

/** statementCount: how many OTTL statements the run replayed for a transform processor (the Statements tab badge). */
export function statementCount(ottl: OTTLReport[] | undefined, processor: string): number | undefined {
  const r = ottl?.find((o) => o.processor === processor);
  if (!r) return undefined;
  return (r.groups ?? []).reduce((n, g) => n + (g.statements?.length ?? 0), 0);
}
