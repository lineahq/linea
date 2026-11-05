import { parse } from 'yaml';
import type { ArtifactMetadata, CaptureSessionPage, Run, RuntimeBuild, Suite } from '../api/types';
import { runActive } from '../api/runState';
import type { IconName } from '../ui/Icon';
import { baseName } from '../files';
import { readScenario } from '../scenario/scenarioDoc';
import { isDirty } from '../workspace/drafts';
import type { WorkspaceState } from '../workspace/store';

/**
 * Pure logic of the debugger's start state (StartView): who is looking, what
 * the lists show and how each row is worded. Every list is derived from what
 * the endpoints return; nothing is invented.
 */

export type Tone = 'pass' | 'fail' | 'warn' | 'running' | 'neutral';

/** A status chip: its words, tone and icon. */
export interface Badge {
  text: string;
  tone: Tone;
  icon: IconName;
}

/** How many rows each start list shows before it points at the full page. */
export const RECENT_LIMIT = 5;
export const SUITE_LIMIT = 3;

// ---------- Draft ----------

/** hasDraft: the workspace store holds a configuration or a scenario. */
export function hasDraft(ws: Pick<WorkspaceState, 'configYAML' | 'scenarioYAML'>): boolean {
  return ws.configYAML.trim() !== '' || ws.scenarioYAML.trim() !== '';
}

/** What the draft holds, for the "Resume current draft" card. */
export interface DraftSummary {
  /** The configuration's file name, or a stand-in when it was written by hand. */
  name: string;
  /** The draft differs from what was last loaded or saved. */
  dirty: boolean;
  /** The scenario's name, when the draft has a readable scenario. */
  scenario?: string;
  inputs: number;
  mocks: number;
  assertions: number;
}

/** draftSummary reads the draft's name and the counts of its scenario. */
export function draftSummary(ws: WorkspaceState): DraftSummary {
  const out: DraftSummary = {
    name: ws.configPath ? baseName(ws.configPath) : ws.configYAML.trim() ? 'Untitled configuration' : 'No configuration yet',
    dirty: isDirty(ws, 'config') || isDirty(ws, 'scenario'),
    inputs: 0,
    mocks: 0,
    assertions: 0,
  };
  if (!ws.scenarioYAML.trim()) return out;
  const read = readScenario(ws.scenarioYAML);
  if ('error' in read) return out;
  const m = read.model;
  out.scenario = m.name || (ws.scenarioPath ? baseName(ws.scenarioPath) : 'Untitled scenario');
  out.inputs = m.inputs.length;
  out.mocks = m.files.length + m.kubeObjects.length + m.kubeFiles.length + m.providers.length + m.environment.length;
  out.assertions = m.assertions.length;
  return out;
}

/** plural words a count: "1 input", "2 inputs". */
export function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`;
}

/** draftCounts words the scenario part of a draft: "1 input · 1 mock · 2 assertions". */
export function draftCounts(d: DraftSummary): string {
  return [plural(d.inputs, 'input'), plural(d.mocks, 'mock'), plural(d.assertions, 'assertion')].join(' · ');
}

// ---------- Who is looking ----------

/** What is known about earlier work. A list that has not answered is undefined. */
export interface Evidence {
  draft: boolean;
  runs?: number;
  saved?: number;
  suites?: number;
  active?: number;
}

/**
 * startMode decides which start state shows. A returning user has a draft,
 * runs, saved sources, suites or running work. Until one of those is known and
 * some list is still loading the answer is "checking"; once every list has
 * answered (or failed) without evidence the user is new.
 */
export function startMode(e: Evidence): 'first' | 'returning' | 'checking' {
  const known = [e.runs, e.saved, e.suites, e.active];
  if (e.draft || known.some((n) => (n ?? 0) > 0)) return 'returning';
  return known.some((n) => n === undefined) ? 'checking' : 'first';
}

// ---------- Time ----------

const pad = (n: number) => String(n).padStart(2, '0');
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

function dayStart(d: Date): number {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
}

/**
 * whenText words a time as the start lists do: "Today 14:09", "Yesterday"
 * and "Oct 14" (with the year when it is not this year).
 */
export function whenText(iso: string | undefined, now: Date): string {
  if (!iso) return '';
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return '';
  const days = Math.round((dayStart(now) - dayStart(t)) / 86_400_000);
  if (days === 0) return `Today ${pad(t.getHours())}:${pad(t.getMinutes())}`;
  if (days === 1) return 'Yesterday';
  const date = `${MONTHS[t.getMonth()]} ${t.getDate()}`;
  return t.getFullYear() === now.getFullYear() ? date : `${date}, ${t.getFullYear()}`;
}

/** editedText says how long ago the draft changed: "edited just now", "edited 6 min ago", "edited 3 h ago", then the day. */
export function editedText(iso: string | undefined, now: Date): string {
  if (!iso) return '';
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return '';
  const min = Math.floor((now.getTime() - t) / 60_000);
  if (min < 1) return 'edited just now';
  if (min < 60) return `edited ${min} min ago`;
  if (min < 24 * 60) return `edited ${Math.floor(min / 60)} h ago`;
  const day = whenText(iso, now);
  return `edited ${day === 'Yesterday' ? 'yesterday' : day}`;
}

/** whenDay is like whenText without the clock time, for saved sources. */
export function whenDay(iso: string | undefined, now: Date): string {
  return whenText(iso, now).replace(/^Today .*/, 'Today');
}

// ---------- Runs ----------

/** shortRunId is the run's identity as run lists show it. */
export function shortRunId(id: string): string {
  return id.slice(0, 8);
}

/**
 * runStatus is how a finished run's execution ended, from its report or else
 * its list summary: run lists carry the summary, never the report.
 */
function runStatus(run: Pick<Run, 'report' | 'summary'>): string | undefined {
  return run.report?.result?.status ?? run.summary?.status;
}

/** runOutcome is the run's overall assertion outcome, from its report or else its list summary. */
function runOutcome(run: Pick<Run, 'report' | 'summary'>): string | undefined {
  return run.report ? run.report.assertions?.outcome : run.summary?.assertions?.outcome;
}

/** runExecution words how the run ended (or is going), apart from its assertions. */
export function runExecution(run: Pick<Run, 'state' | 'interrupted' | 'report' | 'summary'>): Badge {
  switch (run.state) {
    case 'queued':
      return { text: 'Queued', tone: 'neutral', icon: 'clock' };
    case 'assigned':
      return { text: 'Starting', tone: 'running', icon: 'loader-circle' };
    case 'running':
      return { text: 'Running', tone: 'running', icon: 'loader-circle' };
    case 'lost':
      return { text: 'Lost', tone: 'fail', icon: 'triangle-alert' };
    case 'cancelled':
      return { text: 'Cancelled', tone: 'warn', icon: 'circle-stop' };
    case 'error':
      return { text: run.interrupted ? 'Interrupted' : 'Failed', tone: 'fail', icon: 'circle-x' };
  }
  switch (runStatus(run)) {
    case 'completed':
      return { text: 'Completed', tone: 'pass', icon: 'circle-check' };
    case 'partial':
      return { text: 'Partial', tone: 'warn', icon: 'triangle-alert' };
    case 'cancelled':
      return { text: 'Cancelled', tone: 'warn', icon: 'circle-stop' };
    case 'failed':
      return { text: 'Failed', tone: 'fail', icon: 'circle-x' };
    default:
      return { text: run.state === 'done' ? 'Finished' : run.state, tone: 'neutral', icon: 'circle-minus' };
  }
}

/** runAssertions words the run's assertion outcome ("2 of 2 passed", "1 of 2 failed"). */
export function runAssertions(run: Pick<Run, 'state' | 'report' | 'summary'>): Badge {
  // Per-outcome counts: the report's results, or else the summary's counts.
  let counts: Record<string, number> | undefined;
  if (run.report?.assertions) {
    counts = {};
    for (const r of run.report.assertions.results ?? []) counts[r.outcome] = (counts[r.outcome] ?? 0) + 1;
  } else if (!run.report && run.summary?.assertions) {
    const { pass, fail, inconclusive, error } = run.summary.assertions;
    counts = { pass, fail, inconclusive, error };
  }
  const outcome = runOutcome(run);
  if (!counts || outcome === undefined) {
    return run.state === 'done'
      ? { text: 'No assertions', tone: 'neutral', icon: 'circle-minus' }
      : { text: 'Not evaluated', tone: 'neutral', icon: 'circle-minus' };
  }
  const total = Object.values(counts).reduce((a, b) => a + b, 0);
  const count = (o: string) => counts[o] ?? 0;
  switch (outcome) {
    case 'pass':
      return { text: total ? `${count('pass')} of ${total} passed` : 'Passed', tone: 'pass', icon: 'circle-check' };
    case 'fail':
      return { text: total ? `${count('fail')} of ${total} failed` : 'Failed', tone: 'fail', icon: 'circle-x' };
    case 'inconclusive':
      return { text: total ? `${count('inconclusive')} of ${total} inconclusive` : 'Inconclusive', tone: 'warn', icon: 'circle-help' };
    default:
      return { text: total ? `${count('error')} of ${total} errored` : 'Not evaluated', tone: 'warn', icon: 'triangle-alert' };
  }
}

/** One row of the recent runs list. */
export interface RunRow {
  id: string;
  shortId: string;
  /** The scenario's name; a quick run has none. */
  scenario: string;
  when: string;
  execution: Badge;
  assertions: Badge;
}

/** newestFirst sorts runs by creation time; a run list is a snapshot, not a feed. */
export function newestFirst<T extends { created: string }>(items: T[]): T[] {
  return [...items].sort((a, b) => b.created.localeCompare(a.created));
}

/** runRow words one run for a list. */
export function runRow(run: Run, now: Date): RunRow {
  return {
    id: run.id,
    shortId: shortRunId(run.id),
    scenario: run.report?.scenario ?? run.summary?.scenario ?? '',
    when: whenText(run.created, now),
    execution: runExecution(run),
    assertions: runAssertions(run),
  };
}

/** recentRuns: the newest runs, at most `limit`. */
export function recentRuns(runs: Run[] | undefined, now: Date, limit = RECENT_LIMIT): RunRow[] {
  return newestFirst(runs ?? []).slice(0, limit).map((r) => runRow(r, now));
}

// ---------- Suites (recent test results) ----------

export interface SuiteRow {
  id: string;
  /** The configuration the suite ran, as the suite names it. */
  config: string;
  when: string;
  members: number;
  /** The suite's outcome as far as its member runs show it. */
  outcome: Badge;
}

/**
 * suiteOutcome derives a suite's outcome from its members' runs in the run
 * list. A member that failed to start, or whose run is not in the list or not
 * finished, leaves the suite undecided unless another member already failed.
 */
export function suiteOutcome(suite: Suite, runs: Map<string, Run>): Badge {
  const members = suite.members ?? [];
  if (members.length === 0) return { text: 'No scenarios', tone: 'neutral', icon: 'circle-minus' };
  let failed = 0;
  let passed = 0;
  for (const m of members) {
    const run = m.run_id ? runs.get(m.run_id) : undefined;
    if (m.error) failed++;
    else if (!run || runActive(run.state)) continue;
    else if (run.state === 'error' || run.state === 'cancelled' || run.state === 'lost' || runStatus(run) === 'failed') failed++;
    else if (runOutcome(run) === 'pass') passed++;
    else if (runOutcome(run) === 'fail') failed++;
  }
  if (failed > 0) return { text: `${failed} of ${members.length} failed`, tone: 'fail', icon: 'circle-x' };
  if (passed === members.length) return { text: `${passed} of ${members.length} passed`, tone: 'pass', icon: 'circle-check' };
  if (passed > 0) return { text: `${passed} of ${members.length} passed`, tone: 'neutral', icon: 'circle-minus' };
  return { text: 'Result not shown', tone: 'neutral', icon: 'circle-minus' };
}

/** recentSuites: the newest suites with outcomes derived from the run list. */
export function recentSuites(suites: Suite[] | undefined, runs: Run[] | undefined, now: Date, limit = SUITE_LIMIT): SuiteRow[] {
  const byId = new Map((runs ?? []).map((r) => [r.id, r]));
  return newestFirst(suites ?? [])
    .slice(0, limit)
    .map((s) => ({
      id: s.id,
      config: s.config ? baseName(s.config) : 'Configuration',
      when: whenText(s.created, now),
      members: s.members?.length ?? 0,
      outcome: suiteOutcome(s, byId),
    }));
}

// ---------- Saved work ----------

export type SavedKind = 'configuration' | 'scenario' | 'fixture';
export const SAVED_KINDS: SavedKind[] = ['configuration', 'scenario', 'fixture'];

const KIND_WORDS: Record<SavedKind, string> = { configuration: 'Configuration', scenario: 'Scenario', fixture: 'Fixture' };

export interface SavedRow {
  id: string;
  kind: SavedKind;
  label: string;
  /** "Configuration · rev 3" */
  detail: string;
  when: string;
  /** A configuration or scenario opens as a draft; a fixture opens in its source page. */
  draft: boolean;
  meta: ArtifactMetadata;
}

/** savedRows merges the saved kinds, newest update first. */
export function savedRows(pages: Partial<Record<SavedKind, ArtifactMetadata[]>>, now: Date, limit = RECENT_LIMIT): SavedRow[] {
  const all: SavedRow[] = [];
  for (const kind of SAVED_KINDS) {
    for (const m of pages[kind] ?? []) {
      all.push({
        id: m.id,
        kind,
        label: m.label || m.id,
        detail: `${KIND_WORDS[kind]} · rev ${m.generation}`,
        when: whenDay(m.updated, now),
        draft: kind !== 'fixture',
        meta: m,
      });
    }
  }
  return all.sort((a, b) => b.meta.updated.localeCompare(a.meta.updated)).slice(0, limit);
}

/** A status code that means the server cannot list saved work (see workspace/SavedSources.tsx). */
export const NO_WORKSPACE_STATUS = 503;

// ---------- Active work ----------

export interface ActiveCapture {
  id: string;
  title: string;
  detail: string;
  badge: Badge;
  /** Share of the capture's time spent, 0 to 100. */
  progress: number;
}

const CAPTURE_STATES: Record<string, Badge> = {
  pending: { text: 'Starting', tone: 'running', icon: 'loader-circle' },
  active: { text: 'Receiving', tone: 'running', icon: 'loader-circle' },
  stopping: { text: 'Stopping', tone: 'warn', icon: 'loader-circle' },
};

/** captureProgress is how much of the time between creation and expiry has passed. */
export function captureProgress(createdAt: string, expiresAt: string, now: Date): number {
  const start = Date.parse(createdAt);
  const end = Date.parse(expiresAt);
  if (!(end > start)) return 0;
  return Math.round(Math.min(1, Math.max(0, (now.getTime() - start) / (end - start))) * 100);
}

/** activeCaptures lists the capture sessions that have not finished, newest first. */
export function activeCaptures(page: CaptureSessionPage | undefined, now: Date): ActiveCapture[] {
  return newestFirst((page?.items ?? []).filter((s) => !s.terminal).map((s) => ({ ...s, created: s.created_at }))).map((s) => {
    const records = s.data?.received_records ?? 0;
    const problems = s.failed_count + s.uncertain_count;
    return {
      id: s.id,
      title: s.metadata?.name || `Capture ${s.id.slice(-8)}`,
      detail: [plural(s.target_count, 'Collector'), plural(records, 'record'), problems ? plural(problems, 'Collector') + ' not confirmed' : ''].filter(Boolean).join(' · '),
      badge: CAPTURE_STATES[s.state] ?? { text: s.state, tone: 'neutral', icon: 'circle-dashed' },
      progress: captureProgress(s.created_at, s.expires_at, now),
    };
  });
}

export interface ActiveBuild {
  id: string;
  title: string;
  detail: string;
  badge: Badge;
}

/** activeBuilds lists the runtime builds that are queued or running, newest first. The build list carries no log, so a row says only where the build is. */
export function activeBuilds(builds: RuntimeBuild[] | undefined): ActiveBuild[] {
  return newestFirst((builds ?? []).filter((b) => b.state === 'queued' || b.state === 'running')).map((b) => ({
    id: b.id,
    title: `${b.name} ${b.collector_version}`.trim(),
    detail: b.state === 'queued' ? 'Waiting for a builder' : 'Compiling on a builder',
    badge: b.state === 'queued' ? { text: 'Queued', tone: 'neutral', icon: 'clock' } : { text: 'Building', tone: 'running', icon: 'loader-circle' },
  }));
}

// ---------- Needs attention ----------

/** One setup problem or failure with the place that resolves it. */
export interface Attention {
  id: string;
  title: string;
  body: string;
  action: string;
  /** Where the action goes. */
  to: { kind: 'build'; id: string };
}

/**
 * attention lists what the endpoints expose as needing the user: a runtime
 * build that failed and was not built again since.
 */
export function attention(input: { builds?: RuntimeBuild[] }): Attention[] {
  const out: Attention[] = [];
  const builds = newestFirst(input.builds ?? []);
  const seen = new Set<string>();
  for (const b of builds) {
    if (seen.has(b.name)) continue;
    seen.add(b.name); // The newest build of a name decides whether it still needs the user.
    if (b.state !== 'failed') continue;
    out.push({
      id: `build-${b.id}`,
      title: `Build ${b.name} ${b.collector_version} failed`.replace(/\s+/g, ' '),
      body: b.error || 'The build stopped with an error. Its log says why.',
      action: 'Open build',
      to: { kind: 'build', id: b.id },
    });
  }
  return out;
}

// ---------- Dropped and pasted text ----------

const CONFIG_KEYS = ['receivers', 'processors', 'exporters', 'connectors', 'extensions', 'service'];
const SCENARIO_KEYS = ['inputs', 'assertions', 'mocks', 'execution', 'environment'];

/**
 * classifyText tells a Collector configuration from a scenario by its top-level
 * keys, so a dropped or pasted file lands in the right draft. Text that is
 * neither (or not YAML) has no kind.
 */
export function classifyText(text: string): 'configuration' | 'scenario' | undefined {
  let doc: unknown;
  try {
    doc = parse(text);
  } catch {
    return undefined;
  }
  if (!doc || typeof doc !== 'object' || Array.isArray(doc)) return undefined;
  const keys = Object.keys(doc);
  if (keys.some((k) => CONFIG_KEYS.includes(k))) return 'configuration';
  if (keys.some((k) => SCENARIO_KEYS.includes(k))) return 'scenario';
  return undefined;
}

/** isYamlName: the files the start state takes by drop. */
export function isYamlName(name: string): boolean {
  return /\.ya?ml$/i.test(name);
}

/** pasteShortcut names the paste key combination for a platform string. */
export function pasteShortcut(platform: string): string {
  return /mac|iphone|ipad/i.test(platform) ? '⌘V' : 'Ctrl+V';
}

// ---------- Saved lists ----------

/** One saved-source query result, reduced to what the section needs. */
export interface ListState {
  status: 'pending' | 'success' | 'error';
  /** HTTP status of a failed request. */
  httpStatus?: number;
  message?: string;
}

/**
 * savedStatus combines the per-kind saved queries. The section is "none" when
 * every request says the server cannot list saved work (503, as SavedSources reads
 * it), "ok" when any list loaded, and "error" when they failed otherwise.
 */
export function savedStatus(lists: ListState[]): { state: 'loading' | 'ok' | 'none' | 'error'; message?: string } {
  if (lists.some((l) => l.status === 'success')) return { state: 'ok' };
  if (lists.some((l) => l.status === 'pending')) return { state: 'loading' };
  const failures = lists.filter((l) => l.httpStatus !== NO_WORKSPACE_STATUS);
  if (failures.length === 0) return { state: 'none' };
  return { state: 'error', message: failures[0].message };
}
