// Facts about one run for the runs drawer, the run list and the comparison
// layout. Execution state, evidence completeness and assertion outcome are
// three separate facts (application map, section 9): none of them stands in
// for another, and none is inferred from the others.
//
// The run list omits reports but carries the platform's summary of each
// finished run's report: its scenario, runtime, execution status,
// injected inputs, truncation and assertion counts. A fact neither carries is
// undefined, and a view shows a placeholder, never a guess.
import type { RunQuery } from '../api/client';
import type { Report, Run, RunComparison } from '../api/types';
import { runActive } from '../api/runState';

/** The platform's list summary of a run's report. */
type Listing = NonNullable<Run['summary']>;
import type { IconName } from '../ui/Icon';

export type Tone = 'pass' | 'fail' | 'warn' | 'neutral' | 'running';

/** One independent fact about a run, worded for a chip. */
export interface Fact {
  text: string;
  tone: Tone;
  icon: IconName;
  /** The reason or detail behind the text, for a tooltip. */
  title?: string;
}

/** chipClass is the design-system chip class of a fact's tone. */
export function chipClass(tone: Tone): string {
  return tone === 'neutral' ? 'od-chip' : `od-chip od-chip--${tone}`;
}

/** isFinished: the run has ended, with or without a report. */
export function isFinished(run: Pick<Run, 'state'>): boolean {
  return !runActive(run.state);
}

/**
 * runLabel names a run in short. Runs carry no sequence number, only a random
 * identifier ("run-" and 16 hex digits), so the label is its first 8 digits.
 */
export function runLabel(id: string): string {
  return `#${id.replace(/^run-/, '').slice(0, 8)}`;
}

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
const two = (n: number) => String(n).padStart(2, '0');

/**
 * whenText words a run's creation time as the run list does: "Today 14:09",
 * "Yesterday 17:20", then "Oct 16 13:40" (the year only when it differs).
 */
export function whenText(iso: string, now: Date = new Date()): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const time = `${two(d.getHours())}:${two(d.getMinutes())}`;
  const day = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const diff = Math.round((day(now) - day(d)) / 86_400_000);
  if (diff === 0) return `Today ${time}`;
  if (diff === 1) return `Yesterday ${time}`;
  const year = d.getFullYear() === now.getFullYear() ? '' : ` ${d.getFullYear()}`;
  return `${MONTHS[d.getMonth()]} ${d.getDate()}${year} ${time}`;
}

const executionOfStatus = (status: string | undefined, reason?: string): Fact | undefined => {
  switch (status) {
    case 'completed': return { text: 'Completed', tone: 'pass', icon: 'circle-check' };
    case 'partial': return { text: 'Ended early', tone: 'warn', icon: 'triangle-alert', title: reason || 'The deadline expired or the evidence budget was reached.' };
    case 'failed': return { text: 'Failed', tone: 'fail', icon: 'circle-x', title: reason };
    case 'cancelled': return { text: 'Cancelled', tone: 'neutral', icon: 'circle-minus' };
    default: return undefined;
  }
};

/** executionFact is how the run ended (or that it has not): not whether its assertions passed. */
export function executionFact(run: Run): Fact {
  switch (run.state) {
    case 'queued': return { text: 'Queued', tone: 'neutral', icon: 'clock' };
    case 'assigned': return { text: 'Starting', tone: 'running', icon: 'loader-circle', title: 'A worker claimed the run and is starting it.' };
    case 'running': return { text: 'Running', tone: 'running', icon: 'loader-circle' };
    case 'lost': return { text: 'Lost', tone: 'warn', icon: 'triangle-alert', title: run.error };
    case 'cancelled': return { text: 'Cancelled', tone: 'neutral', icon: 'circle-minus' };
    case 'error':
      return run.interrupted
        ? { text: 'Interrupted', tone: 'warn', icon: 'triangle-alert', title: run.error }
        : { text: 'Error', tone: 'fail', icon: 'circle-x', title: run.error };
    default:
      return executionOfStatus(run.report?.result?.status ?? run.summary?.status, run.report?.result?.status_reason)
        ?? { text: 'Finished', tone: 'neutral', icon: 'circle-minus', title: 'The result is read when the run is opened.' };
  }
}

const injected = (rep: Report) => {
  const inputs = rep.result?.inputs ?? [];
  return { sent: inputs.filter((i) => i.injected).length, all: inputs.length };
};

/**
 * evidenceFact says how much of the run was recorded, from its report or else
 * its list summary. It is undefined when a finished run has neither.
 */
export function evidenceFact(run: Run): Fact | undefined {
  if (run.state === 'queued' || run.state === 'assigned') return { text: 'None yet', tone: 'neutral', icon: 'circle-minus' };
  if (run.state === 'running') return { text: 'Recording', tone: 'running', icon: 'loader-circle' };
  const rep = run.report;
  const sum = run.summary;
  if (!rep && !sum) return run.state === 'error' || run.state === 'cancelled' || run.state === 'lost' ? { text: 'None', tone: 'neutral', icon: 'circle-minus' } : undefined;
  if (rep?.result?.truncation) return { text: 'Truncated', tone: 'warn', icon: 'triangle-alert', title: rep.result.truncation.reason };
  if (!rep && sum?.truncated) return { text: 'Truncated', tone: 'warn', icon: 'triangle-alert', title: 'Open the run for the reason.' };
  const status = rep ? rep.result?.status : sum!.status;
  if (status === 'completed') return { text: 'Complete', tone: 'pass', icon: 'circle-check' };
  const { sent, all } = rep ? injected(rep) : { sent: sum!.injected, all: sum!.inputs };
  return {
    text: status === 'partial' && all > 0 ? `Partial ${sent} of ${all}` : 'Partial',
    tone: 'warn', icon: 'triangle-alert',
    title: status === 'partial' && all > 0 ? `${sent} of ${all} inputs were injected before the run ended.` : rep?.result?.status_reason,
  };
}

/** assertionsFact words assertion results; the run's own execution is a separate fact. */
function assertionsFact(outcome: string, outcomes: string[]): Fact {
  const total = outcomes.length;
  const count = (o: string) => outcomes.filter((x) => x === o).length;
  switch (outcome) {
    case 'pass': return { text: total ? `${count('pass')} of ${total} passed` : 'Passed', tone: 'pass', icon: 'circle-check' };
    case 'fail': return { text: total ? `${count('fail')} of ${total} failed` : 'Failed', tone: 'fail', icon: 'circle-x' };
    case 'inconclusive': return { text: total ? `${count('inconclusive')} of ${total} inconclusive` : 'Inconclusive', tone: 'warn', icon: 'triangle-alert', title: 'Inconclusive is neither a pass nor a fail.' };
    case 'error': return { text: 'Assertion error', tone: 'fail', icon: 'circle-x' };
    default: return { text: outcome || 'Not evaluated', tone: 'neutral', icon: 'circle-minus' };
  }
}

/**
 * assertionFact is the run's assertion outcome, from its report or else its
 * list summary. "No assertions" and "Not evaluated" differ: the first is a
 * finished run that declared none, the second a run that ended before it
 * could be checked. Undefined while a finished run has neither.
 */
export function assertionFact(run: Run): Fact | undefined {
  const rep = run.report;
  const sum = run.summary;
  if (!rep && !sum) return isFinished(run) && run.state === 'done' ? undefined : { text: 'Not evaluated', tone: 'neutral', icon: 'circle-minus' };
  if (rep?.assertions) return assertionsFact(rep.assertions.outcome, (rep.assertions.results ?? []).map((r) => r.outcome));
  if (!rep && sum!.assertions) return assertionsFact(sum!.assertions.outcome, countedOutcomes(sum!.assertions));
  return (rep ? rep.result?.status : sum!.status) === 'completed'
    ? { text: 'No assertions', tone: 'neutral', icon: 'circle-minus', title: 'Behaviour was not checked against expectations.' }
    : { text: 'Not evaluated', tone: 'neutral', icon: 'circle-minus' };
}

/** countedOutcomes expands a summary's counts into one outcome per assertion. */
function countedOutcomes(a: NonNullable<Listing['assertions']>): string[] {
  return (['pass', 'fail', 'inconclusive', 'error'] as const).flatMap((o) => Array<string>(a[o]).fill(o));
}

/** listingRuntimeLabel names a summary's runtime the way runtimeLabel names a report's. */
function listingRuntimeLabel(rt: Listing['runtime']): string | undefined {
  if (!rt) return undefined;
  return `${rt.name} ${rt.collector_version}${rt.kind === 'remote' ? ' · remote' : ''}${rt.verified ? '' : ' · unverified'}`;
}

/** runtimeText names the runtime a run executed on, from its report. */
export function runtimeText(rep: Report | undefined): string | undefined {
  return runtimeLabel(rep?.runtime);
}

/** runtimeLabel names a report's runtime: "built-in 0.162.0", marking remote and unverified ones. */
export function runtimeLabel(rt: Report['runtime']): string | undefined {
  if (!rt) return undefined;
  const name = rt.kind === 'built_in' ? 'built-in' : (rt.name || rt.image_id?.slice(0, 19) || 'container');
  return `${name} ${rt.collector_version}${rt.kind === 'remote' ? ' · remote' : ''}${rt.verified ? '' : ' · unverified'}`;
}

/**
 * sourceText names the configuration a run used: its file name when it had
 * one, else "draft" and the start of its digest.
 */
export function sourceText(rep: Report | undefined): string | undefined {
  const src = rep?.provenance?.config;
  if (!src) return undefined;
  const path = src.path?.split(/[\\/]/).pop();
  return path || `untitled ${src.sha256.replace(/^sha256:/, '').slice(0, 6)}`;
}

/** inputCount is how many inputs the run injected, when its report is known. */
export function inputCount(rep: Report | undefined): number | undefined {
  return rep?.result?.inputs ? rep.result.inputs.filter((i) => i.injected).length : undefined;
}

/** RunFacts is everything a run row shows. */
export interface RunFacts {
  id: string;
  label: string;
  when: string;
  source?: string;
  scenario?: string;
  runtime?: string;
  mode?: string;
  execution: Fact;
  evidence?: Fact;
  assertions?: Fact;
  /** Neither the report nor a list summary is known, so the facts above that need one are missing. */
  pending: boolean;
  /** The report could not be read: those facts will not arrive. */
  unreadable: boolean;
}

/**
 * runFacts collects a run's facts; `run` carries its report once that was
 * read. `unreadable` marks a finished run whose report could not be read, so
 * its missing facts are stated rather than shown as loading forever.
 */
export function runFacts(run: Run, now?: Date, unreadable = false): RunFacts {
  const evidence = evidenceFact(run) ?? (unreadable ? { text: 'Unavailable', tone: 'warn' as Tone, icon: 'triangle-alert' as IconName, title: 'The run could not be read.' } : undefined);
  return {
    id: run.id, label: runLabel(run.id), when: whenText(run.created, now),
    source: sourceText(run.report), scenario: run.report?.scenario ?? run.summary?.scenario,
    runtime: run.report ? runtimeText(run.report) : listingRuntimeLabel(run.summary?.runtime),
    mode: run.bundle?.mode,
    execution: executionFact(run), evidence, assertions: assertionFact(run) ?? (unreadable ? evidence : undefined),
    pending: !evidence, unreadable,
  };
}

/**
 * RunFilter narrows the run list. The server filters by scenario, runtime and
 * result (`serverFilter`); the search and the mode apply in this browser.
 */
export interface RunFilter {
  query: string;
  mode: string;
  scenario: string;
  runtime: string;
  result: string;
}

export const NO_FILTER: RunFilter = { query: '', mode: '', scenario: '', runtime: '', result: '' };

/** serverFilter is the part of a filter the run list's request carries. */
export function serverFilter(f: RunFilter): RunQuery {
  const q: RunQuery = {};
  if (f.scenario) q.scenario = f.scenario;
  if (f.runtime) q.runtime = f.runtime;
  if (f.result) q.result = f.result as RunQuery['result'];
  return q;
}

/** The results the server filters by, worded as the execution chip words them. */
export const RESULTS = [
  ['completed', 'completed'], ['partial', 'ended early'], ['failed', 'failed'], ['cancelled', 'cancelled'],
  ['error', 'error'], ['lost', 'lost'], ['queued', 'queued'], ['assigned', 'starting'], ['running', 'running'],
] as const;

/** factsText is every word a row shows, for searching. */
export function factsText(f: RunFacts): string {
  return [f.id, f.label, f.when, f.source, f.scenario, f.runtime, f.mode, f.execution.text, f.evidence?.text, f.assertions?.text]
    .filter(Boolean).join(' ').toLowerCase();
}

/** matchesQuery: every word of the query appears in the text. */
export function matchesQuery(text: string, query: string): boolean {
  return query.toLowerCase().split(/\s+/).filter(Boolean).every((w) => text.includes(w));
}

/**
 * filterRuns applies the browser's part of a filter, the search and the mode,
 * to runs the server already filtered. The search reads every word a row shows.
 */
export function filterRuns(runs: Run[], f: Pick<RunFilter, 'query' | 'mode'>): Run[] {
  return runs.filter((r) => (!f.mode || r.bundle?.mode === f.mode) && matchesQuery(factsText(runFacts(r)), f.query));
}

const distinct = (values: (string | undefined)[]) => [...new Set(values.filter((v): v is string => !!v))].sort();

/** Distinct modes (quick, scenario, ...) among the listed runs, for the mode filter. */
export function modesOf(runs: Run[]): string[] {
  return distinct(runs.map((r) => r.bundle?.mode));
}

/** Distinct scenario names among the listed runs' summaries, for the scenario filter. */
export function scenariosOf(runs: Run[]): string[] {
  return distinct(runs.map((r) => r.summary?.scenario));
}

/** Distinct runtime names among the listed runs' summaries, for the runtime filter. */
export function runtimesOf(runs: Run[]): string[] {
  return distinct(runs.map((r) => r.summary?.runtime?.name));
}

/**
 * toggleSelection keeps at most two runs selected, in selection order: a
 * third selection replaces the earliest one, so two runs can always be
 * compared without first clearing a box.
 */
export function toggleSelection(selected: string[], id: string): string[] {
  if (selected.includes(id)) return selected.filter((x) => x !== id);
  return [...selected, id].slice(-2);
}

/** assertionOutcomeFact words one assertion's outcome in one run; undefined is absent from that run. */
export function assertionOutcomeFact(outcome?: string): Fact {
  switch (outcome) {
    case 'pass': return { text: 'Passed', tone: 'pass', icon: 'circle-check' };
    case 'fail': return { text: 'Failed', tone: 'fail', icon: 'circle-x' };
    case 'inconclusive': return { text: 'Inconclusive', tone: 'warn', icon: 'triangle-alert' };
    case 'error': return { text: 'Error', tone: 'fail', icon: 'circle-x' };
    case undefined: case '': return { text: 'Absent', tone: 'neutral', icon: 'circle-minus', title: 'This run has no assertion of this name.' };
    default: return { text: outcome, tone: 'neutral', icon: 'circle-minus' };
  }
}

/** Fact helpers for the two sides of a comparison, built from the platform's own summary of each run. */
export type ComparedRun = RunComparison['before'];

export function comparedExecution(info: ComparedRun): Fact {
  return executionOfStatus(info.execution_status, info.truncated_reason)
    ?? { text: info.end === 'done' ? 'Finished' : info.end.charAt(0).toUpperCase() + info.end.slice(1), tone: info.end === 'done' ? 'neutral' : 'warn', icon: info.end === 'done' ? 'circle-minus' : 'triangle-alert' };
}

export function comparedEvidence(info: ComparedRun): Fact {
  if (info.truncated_reason) return { text: 'Evidence truncated', tone: 'warn', icon: 'triangle-alert', title: info.truncated_reason };
  if (info.complete) return { text: 'Evidence complete', tone: 'pass', icon: 'circle-check' };
  return { text: 'Evidence incomplete', tone: 'warn', icon: 'triangle-alert', title: info.incomplete };
}

/** comparedAssertions words one side's assertions from its summary and the per-assertion outcomes of that side. */
export function comparedAssertions(info: ComparedRun, outcomes: string[]): Fact {
  if (!info.assertions) return { text: 'No assertions', tone: 'neutral', icon: 'circle-minus' };
  return assertionsFact(info.assertions, outcomes);
}
