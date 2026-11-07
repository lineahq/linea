// View model of the Investigate Overview tab. Execution, evidence and
// assertions are three separate facts (application map, section 9); none
// stands in for another. Everything here comes from the run or its report.
import type { AssertionReport, Event, Report, Run } from '../api/types';
import { runActive } from '../api/runState';
import { assertionFact, evidenceFact, executionFact, type Fact } from './runFacts';
import { limitations, type Limitation } from '../runs/limitations';
import { standIns } from '../runs/standins';
import { modeInfo } from '../graph/lanes';
import type { IconName } from '../ui/Icon';

/** durationText: how long a finished run took. */
export function durationText(r: Pick<Run, 'started' | 'finished'>): string | undefined {
  if (!r.started || !r.finished) return undefined;
  const ms = new Date(r.finished).getTime() - new Date(r.started).getTime();
  if (!(ms >= 0)) return undefined;
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`;
}

export interface Facet {
  name: string;
  fact: Fact;
  detail?: string;
}

/** The kinds that take part in a run's evidence; extensions record activity, not records. */
const RECORDING = new Set(['receiver', 'processor', 'connector', 'exporter']);

export type CoverageState = 'recorded' | 'partial' | 'missing';

export interface CoverageRow {
  id: string;
  kind: string;
  state: CoverageState;
  /** "5 in · 5 out", "input recorded, no output", "not recorded". */
  detail: string;
}

const itemsOf = (e: Event) => e.counts?.items ?? 0;

/**
 * coverage lists what was recorded at each component. A completed run with
 * no truncation recorded everything: an exporter that captured nothing
 * received nothing. After an early end, a component with no recorded output
 * is "not recorded", which says nothing about drops.
 */
export function coverage(report: Report | undefined, events: readonly Event[]): CoverageRow[] {
  const complete = report?.result?.status === 'completed' && !report.result.truncation;
  const rows: CoverageRow[] = [];
  for (const c of report?.topology?.components ?? []) {
    if (!RECORDING.has(c.kind)) continue;
    const mine = events.filter((e) => e.component === c.id && (c.kind === 'connector' ? e.source === 'connector_wrapper' : e.source !== 'connector_wrapper'));
    const sum = (kind: string) => mine.filter((e) => e.kind === kind).reduce((n, e) => n + itemsOf(e), 0);
    const has = (kind: string) => mine.some((e) => e.kind === kind);
    let state: CoverageState;
    let detail: string;
    switch (c.kind) {
      case 'receiver':
        state = has('input.injected') || complete ? 'recorded' : 'missing';
        detail = state === 'recorded' ? `${sum('input.injected')} injected` : 'not recorded';
        break;
      case 'exporter':
        state = has('exporter.captured') || complete ? 'recorded' : 'missing';
        detail = state === 'recorded' ? `received ${sum('exporter.captured')}` : 'not recorded';
        break;
      default:
        if (has('stage.output') || complete) {
          state = 'recorded';
          detail = `${sum('stage.input')} → ${sum('stage.output')}`;
        } else if (has('stage.input')) {
          state = 'partial';
          detail = 'input recorded, no output';
        } else {
          state = 'missing';
          detail = 'not recorded';
        }
    }
    rows.push({ id: c.id, kind: c.kind, state, detail });
  }
  return rows;
}

/** coverageText: "6 of 6 components recorded". */
export function coverageText(rows: CoverageRow[]): string | undefined {
  if (!rows.length) return undefined;
  return `${rows.filter((r) => r.state === 'recorded').length} of ${rows.length} components recorded`;
}

/** facets are the three facts at the top of the Overview, each with its own detail. */
export function facets(run: Run, events: readonly Event[]): Facet[] {
  const rep = run.report;
  const exec = executionFact(run);
  const ev = evidenceFact(run);
  const asr = assertionFact(run);
  const dur = durationText(run);
  const finished = !runActive(run.state);
  const out: Facet[] = [{ name: 'Execution', fact: exec, detail: exec.tone === 'warn' || exec.tone === 'fail' ? (dur ? `after ${dur}` : undefined) : dur }];
  if (ev) out.push({ name: 'Evidence', fact: ev, detail: finished ? coverageText(coverage(rep, events)) : undefined });
  if (asr) {
    const unevaluated = asr.text === 'Not evaluated' && finished && rep?.result?.status !== 'completed';
    out.push({ name: 'Assertions', fact: asr, detail: unevaluated ? 'run did not finish' : undefined });
  }
  return out;
}

export interface AssertionRow {
  /** pass, fail, inconclusive or error. */
  outcome: string;
  icon: IconName;
  name: string;
  target: string;
  /** What each non-passing check expected and observed. */
  evidence: string[];
}

const ASSERT_ICON: Record<string, IconName> = { pass: 'circle-check', fail: 'circle-x', inconclusive: 'triangle-alert', error: 'circle-x' };

export function assertionRows(a: AssertionReport | undefined): AssertionRow[] {
  return (a?.results ?? []).map((r) => ({
    outcome: r.outcome,
    icon: ASSERT_ICON[r.outcome] ?? 'circle-help',
    name: r.name,
    target: r.target,
    evidence: (r.checks ?? [])
      .filter((c) => c.outcome !== 'pass')
      .map((c) => `${c.check}${c.key ? ` ${c.key}` : ''}: expected ${c.expected}${c.observed !== undefined ? `, observed ${c.observed}` : ''}${c.message ? ` — ${c.message}` : ''}`),
  }));
}

export interface EnvironmentRow {
  /** Suffix of the `od-mode--*` class. */
  mode: 'mocked' | 'captured' | 'simulated' | 'standin' | 'unsupported';
  label: string;
  subject: string;
  detail: string;
  warnings: string[];
}

/**
 * environmentRows lists what the run did not execute for real: mocked
 * dependencies and what they supplied, captured exporters, simulated
 * components, stand-in components and unavailable references. Real components
 * are not listed.
 */
export function environmentRows(rep: Report | undefined): EnvironmentRow[] {
  const rows: EnvironmentRow[] = [];
  const mocked = new Set<string>();
  for (const d of rep?.result?.mocks ?? []) {
    mocked.add(d.component);
    rows.push({
      mode: 'mocked',
      label: 'Mocked dep.',
      subject: d.component,
      detail: d.supplied?.length ? `${d.provider} mock supplied ${d.supplied.join(', ')}` : `${d.provider} mock supplied nothing`,
      warnings: d.warnings ?? [],
    });
  }
  const comps = rep?.topology?.components ?? [];
  const group = (mode: string) => comps.filter((c) => c.mode === mode);
  for (const c of group('dependency_mocked')) {
    if (mocked.has(c.id) || mocked.has(c.ref)) continue;
    rows.push({ mode: 'mocked', label: modeInfo('dependency_mocked').label, subject: c.id, detail: c.note ?? modeInfo('dependency_mocked').meaning, warnings: [] });
  }
  const captured = group('captured');
  if (captured.length) rows.push({ mode: 'captured', label: 'Captured', subject: captured.map((c) => c.id).join(', '), detail: 'output recorded, not sent', warnings: [] });
  for (const c of group('simulated')) {
    rows.push({ mode: 'simulated', label: 'Simulated', subject: c.id, detail: c.note ?? modeInfo('simulated').meaning, warnings: [] });
  }
  for (const c of group('unsupported')) {
    rows.push({ mode: 'unsupported', label: 'Unsupported', subject: c.id, detail: c.note ?? modeInfo('unsupported').meaning, warnings: [] });
  }
  const s = standIns(rep);
  for (const l of [...s.components, ...s.references]) {
    rows.push({ mode: 'standin', label: 'Stand-in', subject: l.subject, detail: l.text, warnings: [] });
  }
  return rows;
}

/** hasStandIns: a stand-in component, stand-in value or unavailable reference took part. */
export function hasStandIns(rep: Report | undefined): boolean {
  const s = standIns(rep);
  return s.components.length + s.references.length > 0;
}

export interface Problems {
  errors: Limitation[];
  warnings: Limitation[];
  notes: Limitation[];
}

/** problems splits a run's evidence limits by level; errors and warnings are shown in the open. */
export function problems(run: Run): Problems {
  const all = limitations(run);
  return {
    errors: all.filter((l) => l.level === 'error'),
    warnings: all.filter((l) => l.level === 'warning'),
    notes: all.filter((l) => l.level === 'info'),
  };
}

const count = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;

export interface AssertionsCard {
  /** The short status in the card header: "1 of 2 failed", "Passed" or "None". */
  status: string;
  tone: Fact['tone'];
  /** The checks that did not pass, failed or errored first; never their evidence. */
  failing: AssertionRow[];
  /** The one line shown when nothing failed: "All 2 passed" or "No assertions". */
  line?: string;
  /** Whether the muted "behavior was not checked" sentence follows "No assertions". */
  unchecked: boolean;
  /** Number of assertions, for the tab's count. */
  total: number;
  /** Failed or errored assertions, for the tab's tone. */
  failed: number;
}

/**
 * assertionsCard summarizes a run's assertions for the Overview: how many
 * failed and which, without their evidence. A check that is inconclusive is
 * listed with the failures and never counted as a pass. A run that did not
 * complete and declared none has no card.
 */
export function assertionsCard(rows: AssertionRow[], completed: boolean): AssertionsCard | undefined {
  const n = rows.length;
  const failed = rows.filter((r) => r.outcome === 'fail' || r.outcome === 'error').length;
  const inconclusive = rows.filter((r) => r.outcome === 'inconclusive').length;
  if (!n) return completed ? { status: 'None', tone: 'neutral', failing: [], line: 'No assertions', unchecked: true, total: 0, failed: 0 } : undefined;
  const order = (r: AssertionRow) => (r.outcome === 'fail' || r.outcome === 'error' ? 0 : 1);
  const failing = rows.filter((r) => r.outcome !== 'pass').sort((a, b) => order(a) - order(b));
  if (failed) {
    const extra = inconclusive ? `, ${inconclusive} inconclusive` : '';
    return { status: `${failed} of ${n} failed${extra}`, tone: 'fail', failing, unchecked: false, total: n, failed };
  }
  if (inconclusive) return { status: `${inconclusive} of ${n} inconclusive`, tone: 'warn', failing, unchecked: false, total: n, failed };
  return { status: 'Passed', tone: 'pass', failing: [], line: `All ${n} passed`, unchecked: false, total: n, failed };
}

export interface NotRealLine {
  text: string;
  tone: 'warn';
}

/**
 * notRealLine is the one line that keeps "what did not run for real or was
 * not recorded" visible while the details are collapsed: mocked dependencies,
 * stand-ins, unsupported components and components with no recorded output.
 * Captured exporters and simulated extensions are how every run works and
 * are not counted; the details list them. Nothing to say gives no line.
 */
export function notRealLine(rep: Report | undefined, rows: readonly CoverageRow[], live: boolean): NotRealLine | undefined {
  if (!rep) return undefined;
  const env = environmentRows(rep);
  const of = (mode: EnvironmentRow['mode']) => env.filter((x) => x.mode === mode).length;
  const unrecorded = live ? 0 : rows.filter((x) => x.state !== 'recorded').length;
  const parts: string[] = [];
  if (of('mocked')) parts.push(count(of('mocked'), 'mocked component', 'mocked components'));
  if (of('standin')) parts.push(count(of('standin'), 'stand-in', 'stand-ins'));
  if (of('unsupported')) parts.push(count(of('unsupported'), 'unsupported component', 'unsupported components'));
  if (unrecorded) parts.push(`${count(unrecorded, 'component', 'components')} not recorded`);
  if (!parts.length) return undefined;
  const tail: string[] = [];
  if (hasStandIns(rep)) tail.push('checks through stand-ins are inconclusive');
  if (unrecorded) tail.push('missing output is not a drop');
  return { text: [parts.join(', '), ...tail].join(' · '), tone: 'warn' };
}
