import type { Bisection, BisectVersion, ConfigBisectRequest } from '../api/types';
import type { WorkspaceState } from '../workspace/store';

/**
 * The fleet config bisect view model: the page's search, the request
 * from the form and the scenario draft, and the bisection worded for the
 * page. The platform decides every verdict and status; this only words them.
 */

/** BisectSearch is the page's URL state: the agent and config to search. */
export interface BisectSearch {
  agent?: string;
  config?: string;
}

const text = (v: unknown): string | undefined => (typeof v === 'string' && v.trim() ? v.trim() : undefined);

/** parseBisectSearch is the bisect route's validateSearch. */
export function parseBisectSearch(raw: Record<string, unknown>): BisectSearch {
  const agent = text(raw.agent);
  const config = text(raw.config);
  return { ...(agent ? { agent } : {}), ...(config ? { config } : {}) };
}

/** BisectForm is what the user typed; versions are text until sent. */
export interface BisectForm {
  agent: string;
  config: string;
  good: string;
  bad: string;
}

const version = (v: string): number | undefined => {
  const t = v.trim();
  if (!t) return 0;
  return /^[1-9][0-9]{0,8}$/.test(t) ? Number(t) : undefined;
};

/** bisectProblem says why a bisect cannot start, or undefined. */
export function bisectProblem(form: BisectForm, scenarioYAML: string): string | undefined {
  if (!form.agent.trim()) return 'Name the fleet agent whose configuration broke.';
  const good = version(form.good);
  const bad = version(form.bad);
  if (good === undefined || bad === undefined) return 'Versions are whole numbers from 1; leave them empty for the defaults.';
  if (good && bad && good >= bad) return 'The good version must be older than the bad one.';
  if (!scenarioYAML.trim()) return 'Open a scenario with assertions that reproduce the problem first.';
  return undefined;
}

/**
 * bisectRequest searches the agent's config with the scenario draft as the
 * test, on the runtime chosen beside Run. Empty versions are the defaults:
 * the oldest kept version and the one fleet sends now.
 */
export function bisectRequest(form: BisectForm, ws: Pick<WorkspaceState, 'scenarioYAML' | 'scenarioPath' | 'runtime'>): ConfigBisectRequest {
  const req: ConfigBisectRequest = {
    agent: form.agent.trim(),
    scenarios: [{
      scenario_yaml: ws.scenarioYAML,
      ...(ws.scenarioPath ? { scenario_label: ws.scenarioPath } : {}),
    }],
  };
  if (form.config.trim()) req.config = form.config.trim();
  const good = version(form.good);
  const bad = version(form.bad);
  if (good) req.good = good;
  if (bad) req.bad = bad;
  if (ws.runtime) req.runtime = ws.runtime;
  return req;
}

export type Tone = 'pass' | 'fail' | 'warn' | 'neutral';

export interface Summary {
  tone: Tone;
  title: string;
  body: string;
}

/** bisectSummary states the verdict; inconclusive is never worded as an answer. */
export function bisectSummary(b: Bisection): Summary {
  switch (b.verdict) {
    case 'found':
      return { tone: 'fail', title: `Version ${b.first_bad} broke the scenario`, body: `Version ${b.last_good} passes and version ${b.first_bad}, the next one kept, fails.` };
    case 'no_regression':
      return { tone: 'pass', title: 'No regression in this range', body: `The scenario passes on version ${b.bad}, the bad bound, so no version in ${b.good}–${b.bad} broke it. The problem is outside this config, or the scenario does not reproduce it.` };
    case 'already_bad':
      return { tone: 'warn', title: 'Already failing at the good bound', body: `The scenario fails on version ${b.good} too. Search an older range, or check that the scenario passed before.` };
    case 'inconclusive':
      return b.first_bad && b.last_good
        ? { tone: 'warn', title: `Broken between versions ${b.last_good + 1} and ${b.first_bad}`, body: 'Versions in between could not be judged, so the first bad one is not certain.' }
        : { tone: 'warn', title: 'Inconclusive', body: 'A bound of the range could not be judged (not a pass).' };
  }
  return { tone: 'warn', title: b.verdict, body: '' };
}

export interface VersionRow {
  version: number;
  status: string;
  tone: Tone;
  culprit: boolean;
  lastGood: boolean;
  created: string;
  message: string;
  reason: string;
  rollouts: string;
  runs: { name: string; run: string; outcome: string }[];
}

const STATUS: Record<string, { label: string; tone: Tone }> = {
  good: { label: 'Passes', tone: 'pass' },
  bad: { label: 'Fails', tone: 'fail' },
  skipped: { label: 'Skipped', tone: 'warn' },
  untested: { label: 'Not tested', tone: 'neutral' },
};

/** versionRows lists the range newest first, as fleet lists versions. */
export function versionRows(b: Bisection): VersionRow[] {
  return [...(b.versions ?? [])].reverse().map((v: BisectVersion) => {
    const s = STATUS[v.status] ?? { label: v.status, tone: 'warn' as Tone };
    return {
      version: v.version,
      status: s.label,
      tone: s.tone,
      culprit: !!b.culprit && v.version === b.culprit.version,
      lastGood: !!b.culprit && v.version === b.culprit.last_good,
      created: `${v.created_at.slice(0, 16).replace('T', ' ')}${v.created_by ? ` · ${v.created_by}` : ''}`,
      message: v.message ?? '',
      reason: v.reason ?? '',
      rollouts: (v.rollouts ?? []).map((r) => `${r.state} ${r.created_at.slice(0, 10)}`).join(', '),
      runs: (v.scenarios ?? []).filter((r) => r.run).map((r) => ({ name: r.name, run: r.run as string, outcome: r.outcome })),
    };
  });
}

export interface DiffLine {
  kind: 'add' | 'del' | 'ctx' | 'hunk';
  text: string;
}

/** diffLines classifies the culprit's unified diff, without its file headers, in the kinds the shared `.unified` styles use. */
export function diffLines(diff: string): DiffLine[] {
  const out: DiffLine[] = [];
  for (const line of diff.split('\n')) {
    if (line.startsWith('---') || line.startsWith('+++')) continue;
    if (line.startsWith('@@')) out.push({ kind: 'hunk', text: line });
    else if (line.startsWith('+')) out.push({ kind: 'add', text: line.slice(1) });
    else if (line.startsWith('-')) out.push({ kind: 'del', text: line.slice(1) });
    else if (line) out.push({ kind: 'ctx', text: line.slice(1) });
  }
  return out;
}

/** BisectSubject is what a bisect was started on, kept to word the result. */
export interface BisectSubject {
  agent: string;
  scenarioLabel: string;
  startedAt: number;
}

/** LEAVE_TITLE and LEAVE_BODY are the banner while a bisect runs. */
export const LEAVE_TITLE = 'Leaving this page cancels the bisect';
export const LEAVE_BODY = 'A bisect is tied to this page: it runs the scenario on one version after another.';

/** leaveQuestion asks before a navigation that would cancel the running bisect. */
export function leaveQuestion(nextPath: string | undefined): string {
  return `Cancel the bisect and leave${nextPath ? ` for ${nextPath}` : ''}? The run in progress is cancelled and no result is kept.`;
}
