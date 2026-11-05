// View model of a run comparison (GET /api/v1/runs/{before}/compare/{after}).
// Pure presentation over the platform's result: every claim comes from it, and
// nothing is matched or counted again here.
import type { ComparisonExporter, ComparisonFate, ComparisonRecord, ComparisonValue, Run, RunComparison } from '../api/types';
import { runActive } from '../api/runState';
import { diffValue } from '../inspect/structuredDiff';
import type { FieldView, Level } from '../inspect/recordViews';
import type { RecordFilter } from '../inspect/RecordList';

/** A run that can be compared: one that has ended, with or without a report. */
export function finished(run: Pick<Run, 'state'>): boolean {
  return !runActive(run.state);
}

/**
 * The runs the selected run can be compared with, in the run list's order:
 * newest admitted first.
 */
export function compareCandidates(runs: Run[], current: string): Run[] {
  return runs.filter((r) => r.id !== current && finished(r));
}

/**
 * comparePair makes the earlier admitted of two listed runs the "before" run,
 * whichever of the two is selected. It reads the run list's order, not the
 * creation times: the two runs of a configuration comparison are admitted
 * together with the same time.
 */
export function comparePair(runs: Pick<Run, 'id'>[], a: string, b: string): { before: string; after: string } | undefined {
  const at = (id: string) => runs.findIndex((r) => r.id === id);
  if (at(a) < 0 || at(b) < 0) return undefined;
  return at(a) > at(b) ? { before: a, after: b } : { before: b, after: a };
}

export interface Headline {
  text: string;
  cls: 'good' | 'bad' | 'warn';
}

/** Differing exporter outputs: neither equal nor equal once ignored paths are removed. */
export function differing(c: Pick<RunComparison, 'exporters'>): ComparisonExporter[] {
  return (c.exporters ?? []).filter((e) => !e.equal && !e.equal_after_ignore);
}

const plural = (n: number, noun: string) => `${n} ${noun}${n === 1 ? '' : 's'}`;

/** headline states the outcome in one sentence, with the status style it takes. */
export function headline(c: RunComparison): Headline {
  const outputs = c.exporters?.length ?? 0;
  if (c.outcome === 'inconclusive') {
    return { text: `Inconclusive: ${plural(c.reasons?.length ?? 0, 'reason')} below prevent a verdict.`, cls: 'warn' };
  }
  if (c.outcome === 'same') {
    return { text: outputs ? `Same: every exporter received the same records (${plural(outputs, 'output')}).` : 'Same: neither run delivered any output.', cls: 'good' };
  }
  const parts = [`${differing(c).length} of ${plural(outputs, 'exporter output')} differ`];
  if (c.records) parts.push(`${plural(c.records.changed, 'injected record')} changed`);
  return { text: `Different: ${parts.join('; ')}.`, cls: 'bad' };
}

/** exporterText says how one exporter's records compare, as the CLI does. */
export function exporterText(e: ComparisonExporter): string {
  if (e.only_in) return `output only in the ${e.only_in} run`;
  if (e.equal) return e.note ? 'same records, different order' : 'same records';
  if (e.equal_after_ignore) return 'same apart from ignored paths';
  return e.before === e.after ? 'different records, same count' : 'different records';
}

/** valueText formats a recorded value as the record views do; cut values say so. */
export function valueText(v?: ComparisonValue): string | undefined {
  if (!v) return undefined;
  return diffValue(v.value, v.type) + (v.truncated ? '…' : '');
}

/** The level an OTTL path lives at: resource, scope or the record itself. */
export function pathLevel(path: string): Level {
  if (path.startsWith('resource.') || path === 'resource') return 'resource';
  if (path.startsWith('scope.') || path.startsWith('instrumentation_scope.')) return 'scope';
  return 'record';
}

const OPS: Record<string, FieldView['op']> = { added: 'added', removed: 'removed', modified: 'modified', type_changed: 'type_changed' };

/** One exporter both runs delivered a record to, with the fields that differ there. */
export interface ExporterFields {
  exporter: string;
  fields: FieldView[];
}

export interface CompareItem {
  id: string;
  record: ComparisonRecord;
  title: string;
  /** One sentence: how the record differs, without repeating the fields. */
  why: string;
  tone: 'drop' | 'mod';
  exporters: ExporterFields[];
}

const ends = (f: ComparisonFate) => [...(f.delivered ?? []).map((d) => `delivered to ${d}`), ...(f.stopped ?? [])];

/** fateText says where a record ended in one run. */
export function fateText(f: ComparisonFate): string {
  const all = ends(f);
  return all.length ? all.join('; ') : 'no end recorded';
}

/** compareItems is one row per changed injected record, in the platform's order. */
export function compareItems(c: RunComparison): CompareItem[] {
  return (c.records?.items ?? []).map((r) => {
    const byExporter = new Map<string, FieldView[]>();
    for (const ch of r.changes ?? []) {
      const before = ch.before?.type, after = ch.after?.type;
      const fields = byExporter.get(ch.exporter) ?? [];
      fields.push({
        level: pathLevel(ch.path), path: ch.path, op: OPS[ch.op] ?? 'modified',
        before: valueText(ch.before), after: valueText(ch.after),
        type: before && after && before !== after ? `${before} → ${after}` : before ?? after,
        confidence: r.confidence,
      });
      byExporter.set(ch.exporter, fields);
    }
    const exporters = [...byExporter].map(([exporter, fields]) => ({ exporter, fields }));
    const changed = exporters.length ? `changed at ${plural(exporters.length, 'exporter')}` : '';
    const why = r.fate_changed ? (changed ? `Ends differently and ${changed}` : 'Ends differently')
      : changed ? `C${changed.slice(1)}` : 'Changed; its field changes are not listed';
    return {
      id: `${r.input}:${r.ref}`, record: r, title: r.name || `Record ${r.ref} of input ${r.input}`, why,
      tone: r.fate_changed ? 'drop' : 'mod', exporters,
    };
  });
}

export const COMPARE_FILTERS: RecordFilter<CompareItem>[] = [
  { id: 'all', label: 'All', match: () => true },
  { id: 'fate', label: 'Ends differently', match: (i) => i.record.fate_changed },
  { id: 'fields', label: 'Fields changed', match: (i) => i.exporters.length > 0 },
];

/** Verdict is the comparison page's first answer: whether the runs can be compared, and why not. */
export interface Verdict {
  title: string;
  /** `ok` when the platform reached a verdict, `no` when it could not. */
  tone: 'ok' | 'no';
  lead: string;
  /** Every reason the comparison is inconclusive, in the platform's words. */
  reasons: string[];
}

const sentence = (s: string) => (s ? s.charAt(0).toUpperCase() + s.slice(1) : s);

/**
 * verdict states eligibility before findings (application map, section 9).
 * An inconclusive comparison lists every reason; "Not comparable" is used
 * when the inputs are not proven identical, "Inconclusive" for the other
 * reasons (incomplete or truncated evidence, differing builds).
 */
export function verdict(c: RunComparison): Verdict {
  const reasons = (c.reasons ?? []).map((r) => sentence(r.message));
  if (c.outcome === 'inconclusive') {
    return {
      title: c.inputs_identical ? 'Inconclusive' : 'Not comparable', tone: 'no', reasons,
      lead: reasons.length
        ? 'Differences between these executions would not prove anything about the configuration:'
        : 'The platform reported no verdict and gave no reason.',
    };
  }
  return {
    title: 'Comparable', tone: 'ok', reasons,
    lead: `${c.inputs_identical ? 'Same captured inputs (identical input digest). ' : ''}${headline(c).text}`,
  };
}

/** One of the four summary figures above the details. */
export interface SummaryCard {
  id: 'outputs' | 'records' | 'routes';
  label: string;
  value: number;
  of: string;
  note?: string;
}

/**
 * summaryCards counts what differs from the platform's own tallies. Record
 * counts come from `records.changed`, which covers every changed record even
 * when the listed items are cut at 100; unaligned and uncertain records are
 * named, never counted as unchanged.
 */
export function summaryCards(c: RunComparison): SummaryCard[] {
  const outputs = c.exporters?.length ?? 0;
  const routes = c.routes ?? [];
  const cards: SummaryCard[] = [{ id: 'outputs', label: 'Outputs changed', value: differing(c).length, of: `of ${plural(outputs, 'exporter output')}` }];
  if (c.records) {
    const notes = [c.records.unaligned && `${c.records.unaligned} injected in one run only`, c.records.uncertain && `${c.records.uncertain} not followed with confidence`].filter(Boolean);
    cards.push({ id: 'records', label: 'Records changed', value: c.records.changed, of: `of ${plural(c.records.aligned, 'aligned record')}`, note: notes.join('; ') || undefined });
  }
  cards.push({ id: 'routes', label: 'Routes changed', value: routes.filter((r) => r.changed).length, of: `of ${plural(routes.length, 'route')}` });
  return cards;
}

export interface OutputRow {
  key: string;
  exporter: string;
  signal: string;
  before: number;
  after: number;
  delta: { text: string; tone: 'up' | 'down' | 'same' };
  records: string;
}

/** outputRows is one row per exporter output: both counts, the count change and how the records compare. */
export function outputRows(c: Pick<RunComparison, 'exporters'>): OutputRow[] {
  return (c.exporters ?? []).map((e) => {
    const n = e.after - e.before;
    const same = e.equal || e.equal_after_ignore;
    const delta: OutputRow['delta'] = n !== 0
      ? { text: `${n > 0 ? '+' : '−'}${Math.abs(n)} ${e.signal}`, tone: n > 0 ? 'up' : 'down' }
      : same ? { text: 'no change', tone: 'same' } : { text: 'same count, different records', tone: 'down' };
    return { key: `${e.exporter}:${e.signal}`, exporter: e.exporter, signal: e.signal, before: e.before, after: e.after, delta, records: exporterText(e) };
  });
}

/** assertionOutcomes lists one side's assertion outcomes, for counting its passes and failures. */
export function assertionOutcomes(c: Pick<RunComparison, 'assertions'>, side: 'before' | 'after'): string[] {
  return (c.assertions ?? []).map((a) => a[side]).filter((o): o is string => !!o);
}
