import { ApiError } from '../api/client';
import type { RetentionPolicy, RunPruneRequest, RunPruneResult, StorageUsage } from '../api/types';
import { formatSize } from '../runtimes/runtimes';
import { RESULTS, whenText } from './runFacts';

/**
 * The view model of the run history's storage page (`/runs/storage`): usage against the limit and the retention policy, and the manual prune
 * with its preview. The runtimes' own storage page measures Docker's disk and
 * is unrelated (`runtimes/pages/storage.ts`).
 */

const number = (n: number) => n.toLocaleString('en-US');
const plural = (n: number, one: string, many = `${one}s`) => `${number(n)} ${n === 1 ? one : many}`;

/** ageText words a retention age: whole days, else hours, minutes or seconds. */
export function ageText(seconds: number): string {
  if (seconds % 86_400 === 0) return plural(seconds / 86_400, 'day');
  if (seconds % 3600 === 0) return plural(seconds / 3600, 'hour');
  if (seconds % 60 === 0) return plural(seconds / 60, 'minute');
  return plural(seconds, 'second');
}

/** retentionText words the policy the server applies; a zero field bounds nothing. */
export function retentionText(p: RetentionPolicy): string {
  const runs = p.runs > 0;
  const age = p.age_seconds > 0;
  if (runs && age) return `At most ${plural(p.runs, 'run')}, removed after ${ageText(p.age_seconds)}.`;
  if (runs) return `At most ${plural(p.runs, 'run')}.`;
  if (age) return `Runs are removed after ${ageText(p.age_seconds)}.`;
  return 'No bound on the number or the age of runs.';
}

/** The runs no policy removes, which the page states beside the policy. */
export const PROTECTED_TEXT =
  'Never removed: unfinished runs, kept runs, runs of a suite and runs a saved source still references.';

export interface UsageView {
  /** "1.2 MB of 4.0 GB", or "1.2 MB used, no limit". */
  text: string;
  /** The share of the limit in use, 0 to 100; undefined without a limit. */
  percent?: number;
  /** More is stored than the limit allows (the limit was lowered): new work is refused. */
  over: boolean;
  /** Words the meter for assistive technology. */
  summary: string;
}

/** usageView compares the stored bytes with the limit; a limit of zero or less bounds nothing. */
export function usageView(st: StorageUsage): UsageView {
  const used = formatSize(Math.max(st.used, 0));
  if (st.limit <= 0) return { text: `${used} used, no limit`, over: false, summary: `${used} used, no storage limit` };
  const ratio = st.used / st.limit;
  const percent = Math.min(100, Math.round(ratio * 1000) / 10);
  const text = `${used} of ${formatSize(st.limit)}`;
  return { text, percent, over: st.used > st.limit, summary: `${text}, ${percent}% in use` };
}

/** oldestText names when the oldest stored run was admitted, or says there is none. */
export function oldestText(st: StorageUsage, now?: Date): string {
  return st.oldest_run ? whenText(st.oldest_run, now) : 'No runs stored';
}

/** storageProblem words a failed read of the usage. */
export function storageProblem(error: Error): string {
  if (error instanceof ApiError) {
    if (error.status === 403) return 'Your role may not read saved work, so it cannot show the run history storage.';
    if (error.status === 503) return 'This server keeps no saved work, so it keeps no run history to bound.';
  }
  return `Could not read the storage: ${error.message}`;
}

/** PruneForm is the prune's filters as the form holds them: strings, empty meaning unfiltered. */
export interface PruneForm {
  /** A date, `YYYY-MM-DD`: runs admitted before the start of that day, in this browser's time zone. */
  before: string;
  result: string;
  scenario: string;
  runtime: string;
  project: string;
}

export const NO_PRUNE_FORM: PruneForm = { before: '', result: '', scenario: '', runtime: '', project: '' };

const ACTIVE = ['queued', 'assigned', 'running'];

/** PRUNE_RESULTS are the run list's result values a prune can match: only finished runs are pruned. */
export const PRUNE_RESULTS = RESULTS.filter(([value]) => !ACTIVE.includes(value));

const DATE = /^(\d{4})-(\d{2})-(\d{2})$/;

/** beforeTime is the RFC 3339 time of the start of a form date, or undefined when it is empty or not a date. */
export function beforeTime(date: string): string | undefined {
  const m = DATE.exec(date.trim());
  if (!m) return undefined;
  const [y, mo, d] = [Number(m[1]), Number(m[2]), Number(m[3])];
  const t = new Date(y, mo - 1, d);
  if (t.getFullYear() !== y || t.getMonth() !== mo - 1 || t.getDate() !== d) return undefined;
  return t.toISOString();
}

/** formProblem says why the form cannot be sent, or is undefined when it can. */
export function formProblem(f: PruneForm): string | undefined {
  if (f.before.trim() && !beforeTime(f.before)) return 'Enter the date as year-month-day.';
  return undefined;
}

/** normalized trims the free-text fields, as the request sends them. */
function normalized(f: PruneForm): PruneForm {
  return { before: f.before.trim(), result: f.result, scenario: f.scenario.trim(), runtime: f.runtime.trim(), project: f.project.trim() };
}

/** isUnfiltered: the form selects every run retention could remove. */
export function isUnfiltered(f: PruneForm): boolean {
  const n = normalized(f);
  return !n.before && !n.result && !n.scenario && !n.runtime;
}

/** pruneRequest is the request for the form; each unset filter leaves its key out. */
export function pruneRequest(f: PruneForm, dryRun: boolean): RunPruneRequest {
  const n = normalized(f);
  const req: RunPruneRequest = {};
  const before = beforeTime(n.before);
  if (before) req.before = before;
  if (n.project) req.project = n.project;
  if (n.scenario) req.scenario = n.scenario;
  if (n.runtime) req.runtime = n.runtime;
  if (n.result) req.result = n.result;
  if (dryRun) req.dry_run = true;
  return req;
}

/** Preview is a dry run's result and the form it was made for. */
export interface Preview {
  form: PruneForm;
  result: RunPruneResult;
}

/** sameForm: two forms send the same request. */
export function sameForm(a: PruneForm, b: PruneForm): boolean {
  const [x, y] = [normalized(a), normalized(b)];
  return x.before === y.before && x.result === y.result && x.scenario === y.scenario && x.runtime === y.runtime && x.project === y.project;
}

/** previewMatches: the preview was made for these very form values, so its count is what a prune would remove. */
export function previewMatches(f: PruneForm, preview?: Preview): preview is Preview {
  return !!preview && sameForm(f, preview.form);
}

/** canPrune: a preview of this form found runs to remove. Nothing else enables the destructive button. */
export function canPrune(f: PruneForm, preview?: Preview): boolean {
  return !formProblem(f) && previewMatches(f, preview) && preview.result.runs > 0;
}

/** previewText words a dry run: how many runs and how much storage a prune would free. */
export function previewText(r: RunPruneResult): string {
  if (r.runs === 0) return 'No run matches, so there is nothing to remove.';
  return `${plural(r.runs, 'run')}, ${formatSize(r.bytes)} freed`;
}

/** prunedText words a finished prune. */
export function prunedText(r: RunPruneResult): string {
  return r.runs === 0 ? 'No run was removed.' : `Removed ${plural(r.runs, 'run')}, freed ${formatSize(r.bytes)}.`;
}

/** resultLabel is the run list's word for a result value. */
export function resultLabel(value: string): string {
  return RESULTS.find(([v]) => v === value)?.[1] ?? value;
}

/** scopeText names what the form selects, for the confirmation: always finished runs that nothing protects. */
export function scopeText(f: PruneForm): string {
  const n = normalized(f);
  const parts: string[] = [];
  if (n.before) parts.push(`admitted before ${n.before}`);
  if (n.result) parts.push(`with result ${resultLabel(n.result)}`);
  if (n.scenario) parts.push(`of scenario ${n.scenario}`);
  if (n.runtime) parts.push(`on runtime ${n.runtime}`);
  if (n.project) parts.push(`in project ${n.project}`);
  return parts.length ? `finished runs ${parts.join(', ')}` : 'every finished run';
}

/**
 * pruneQuestion is the inline confirmation: the counted runs, the filters,
 * that it cannot be undone, and what stays.
 */
export function pruneQuestion(f: PruneForm, r: RunPruneResult): string {
  return `Delete ${plural(r.runs, 'run')} (${scopeText(f)}), freeing ${formatSize(r.bytes)}? Their reports, recorded evidence and captured inputs are removed for good. Kept runs, runs of a suite and runs a saved source references stay.`;
}

/** pruneProblem words a failed preview or prune; only an admin may prune. */
export function pruneProblem(error: Error): string {
  if (error instanceof ApiError) {
    if (error.status === 403) return 'Only an admin may prune runs.';
    if (error.status === 503) return 'This server keeps no run history, so there is nothing to prune.';
  }
  return error.message;
}
