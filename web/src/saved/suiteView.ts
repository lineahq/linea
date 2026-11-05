import type { AssertionCheck, AssertionResult, Suite, SuiteMemberStatus, SuiteStatus, TestReport, ScenarioResult } from '../api/types';
import { runActive } from '../api/runState';
import type { IconName } from '../ui/Icon';
import { plural } from './format';

/**
 * View models for suites: the list row and the result page. A suite's status
 * (members with state and outcome) comes from GET /suites/{id}; the failing
 * and inconclusive assertions come from its report, which exists once every
 * member has finished.
 */

export type Tone = 'pass' | 'fail' | 'warn' | 'neutral' | 'running';

/** A chip or facet value: its tone, icon and words. */
export interface Badge {
  tone: Tone;
  icon: IconName;
  label: string;
}

/** outcomeBadge words an assertion outcome (pass, fail, inconclusive, error). */
export function outcomeBadge(outcome?: string): Badge {
  switch (outcome) {
    case 'pass':
      return { tone: 'pass', icon: 'circle-check', label: 'Passed' };
    case 'fail':
      return { tone: 'fail', icon: 'circle-x', label: 'Failed' };
    case 'inconclusive':
      return { tone: 'warn', icon: 'triangle-alert', label: 'Inconclusive' };
    case 'error':
      return { tone: 'fail', icon: 'circle-x', label: 'Error' };
    default:
      return { tone: 'neutral', icon: 'circle-dashed', label: 'Not finished' };
  }
}

/** stateBadge words a member's execution state; a member without a run was never admitted. */
export function stateBadge(state: string | undefined, runId: string | undefined): Badge {
  if (!runId) return { tone: 'fail', icon: 'circle-x', label: 'Not started' };
  switch (state) {
    case 'done':
      return { tone: 'pass', icon: 'circle-check', label: 'Completed' };
    case 'error':
      return { tone: 'fail', icon: 'circle-x', label: 'Run failed' };
    case 'cancelled':
      return { tone: 'warn', icon: 'ban', label: 'Cancelled' };
    case 'lost':
      return { tone: 'fail', icon: 'triangle-alert', label: 'Lost' };
    case 'assigned':
      return { tone: 'running', icon: 'loader-circle', label: 'Starting' };
    case 'running':
      return { tone: 'running', icon: 'loader-circle', label: 'Running' };
    case 'queued':
      return { tone: 'neutral', icon: 'clock', label: 'Queued' };
    default:
      return { tone: 'neutral', icon: 'circle-dashed', label: state ?? 'Unknown' };
  }
}

/** chipClass is the design-system class of a chip of this tone. */
export function chipClass(tone: Tone): string {
  return tone === 'neutral' ? 'od-chip' : `od-chip od-chip--${tone}`;
}

/** The members that have an outcome (their run finished). */
export function finishedMembers(members: SuiteMemberStatus[]): number {
  return members.filter((m) => m.outcome).length;
}

/** suiteResultBadge is the one-line result of a suite for the list: "Failed · 1 of 3". */
export function suiteResultBadge(suite: Suite, status?: SuiteStatus): Badge {
  const n = suite.members?.length ?? 0;
  if (!status) return { tone: 'neutral', icon: 'circle-dashed', label: 'Loading…' };
  if (!status.done) {
    return { tone: 'running', icon: 'loader-circle', label: `Running · ${finishedMembers(status.members ?? [])} of ${n} finished` };
  }
  const head = outcomeBadge(status.outcome);
  const count = (status.members ?? []).filter((m) => m.outcome === status.outcome).length;
  return { ...head, label: `${head.label} · ${count} of ${n}` };
}

/** A facet of the suite result's summary strip. */
export interface Facet extends Badge {
  name: string;
  detail: string;
}

/** assertionTotals counts every assertion of every member in a report. */
export function assertionTotals(report?: TestReport): { total: number; pass: number; fail: number; inconclusive: number; error: number } {
  const t = { total: 0, pass: 0, fail: 0, inconclusive: 0, error: 0 };
  for (const s of report?.scenarios ?? []) {
    for (const r of s.assertions?.results ?? []) {
      t.total++;
      if (r.outcome === 'pass') t.pass++;
      else if (r.outcome === 'fail') t.fail++;
      else if (r.outcome === 'inconclusive') t.inconclusive++;
      else t.error++;
    }
  }
  return t;
}

function joinParts(parts: (string | false | undefined)[]): string {
  return parts.filter(Boolean).join(' · ');
}

/**
 * suiteFacets summarizes a suite in up to three facets: the suite outcome,
 * how many members ran to completion, and (once the report is read) how many
 * assertions passed.
 */
export function suiteFacets(status: SuiteStatus, report?: TestReport): Facet[] {
  const members = status.members ?? [];
  const n = members.length;
  const count = (outcome: string) => members.filter((m) => m.outcome === outcome).length;

  let suite: Facet;
  if (!status.done) {
    suite = { name: 'Suite', tone: 'running', icon: 'loader-circle', label: 'Running', detail: `${finishedMembers(members)} of ${plural(n, 'member')} finished` };
  } else {
    const head = outcomeBadge(status.outcome);
    const bad = n - count('pass');
    suite = {
      name: 'Suite',
      ...head,
      detail:
        status.outcome === 'pass'
          ? `All ${plural(n, 'member')} passed`
          : joinParts([count('fail') > 0 && `${count('fail')} failed`, count('inconclusive') > 0 && `${count('inconclusive')} inconclusive`, count('error') > 0 && `${count('error')} with errors`]) ||
            `${bad} of ${plural(n, 'member')} did not pass`,
    };
  }

  const done = members.filter((m) => m.state === 'done').length;
  const failedRuns = members.filter((m) => m.state === 'error' || m.state === 'lost').length;
  const cancelled = members.filter((m) => m.state === 'cancelled').length;
  const notStarted = members.filter((m) => !m.run_id).length;
  const active = members.filter((m) => runActive(m.state)).length;
  const allDone = done === n;
  const execution: Facet = {
    name: 'Execution',
    tone: allDone ? 'pass' : active > 0 ? 'running' : 'warn',
    icon: allDone ? 'circle-check' : active > 0 ? 'loader-circle' : 'triangle-alert',
    label: `${done} of ${n} completed`,
    detail: joinParts([
      active > 0 && `${active} still running`,
      failedRuns > 0 && `${failedRuns} failed to run`,
      cancelled > 0 && `${cancelled} cancelled`,
      notStarted > 0 && `${notStarted} not started`,
    ]) || (allDone ? 'Every member ran' : ''),
  };

  const out = [suite, execution];
  const t = assertionTotals(report);
  if (report && t.total > 0) {
    const bad = t.fail + t.inconclusive + t.error;
    out.push({
      name: 'Assertions',
      tone: t.fail + t.error > 0 ? 'fail' : t.inconclusive > 0 ? 'warn' : 'pass',
      icon: t.fail + t.error > 0 ? 'circle-x' : t.inconclusive > 0 ? 'triangle-alert' : 'circle-check',
      label: `${t.pass} of ${t.total} passed`,
      detail: bad === 0 ? 'No failures' : joinParts([t.fail > 0 && `${t.fail} failed`, t.inconclusive > 0 && `${t.inconclusive} inconclusive`, t.error > 0 && `${t.error} with errors`]),
    });
  }
  return out;
}

/** One assertion that did not pass, with the evidence in words. */
export interface AssertionLine {
  tone: 'fail' | 'warn';
  name: string;
  target: string;
  evidence: string;
}

function checkEvidence(c: AssertionCheck): string {
  if (c.message) return c.message;
  return `${c.check}${c.key ? ` ${c.key}` : ''}: expected ${c.expected}${c.observed ? `, observed ${c.observed}` : ''}`;
}

/** assertionLine words an assertion that failed, was inconclusive or errored. */
export function assertionLine(r: AssertionResult): AssertionLine {
  const checks = (r.checks ?? []).filter((c) => c.outcome !== 'pass');
  return {
    tone: r.outcome === 'inconclusive' ? 'warn' : 'fail',
    name: r.name,
    target: r.target,
    evidence: checks.slice(0, 3).map(checkEvidence).join(' · ') + (checks.length > 3 ? ` · and ${checks.length - 3} more` : ''),
  };
}

/** One row of the Members table, with the assertions that need attention. */
export interface MemberRow {
  label: string;
  runId?: string;
  state: Badge;
  outcome: Badge;
  /** "3 of 4 failed", "2 of 2 passed"; empty when the member has no assertions. */
  assertions: string;
  /** Why a member without assertion results has an error outcome. */
  message?: string;
  attention: AssertionLine[];
}

/** scenarioFor finds the report entry of a member: by run, else by position. */
export function scenarioFor(report: TestReport | undefined, member: SuiteMemberStatus, index: number, count: number): ScenarioResult | undefined {
  const list = report?.scenarios ?? [];
  if (member.run_id) {
    const byRun = list.find((s) => s.run_id === member.run_id);
    if (byRun) return byRun;
  }
  return list.length === count ? list[index] : undefined;
}

function assertionsText(results: AssertionResult[]): string {
  if (!results.length) return '';
  const n = results.length;
  const count = (o: string) => results.filter((r) => r.outcome === o).length;
  const parts = [count('fail') > 0 && `${count('fail')} of ${n} failed`, count('inconclusive') > 0 && `${count('inconclusive')} inconclusive`, count('error') > 0 && `${count('error')} with errors`];
  return joinParts(parts) || `${n} of ${n} passed`;
}

/** memberRows builds the Members table; the report adds assertions once it is read. */
export function memberRows(status: SuiteStatus, report?: TestReport): MemberRow[] {
  const members = status.members ?? [];
  return members.map((m, i) => {
    const results = scenarioFor(report, m, i, members.length)?.assertions?.results ?? [];
    return {
      label: m.label,
      runId: m.run_id || undefined,
      state: stateBadge(m.state, m.run_id),
      outcome: outcomeBadge(m.outcome),
      assertions: assertionsText(results),
      message: results.length === 0 && m.outcome === 'error' ? m.message : undefined,
      attention: results.filter((r) => r.outcome !== 'pass').map(assertionLine),
    };
  });
}

/** firstFailing returns the first member that did not pass and has a run to open. */
export function firstFailing(rows: MemberRow[]): MemberRow | undefined {
  return rows.find((r) => r.runId && r.outcome.label !== 'Passed' && r.outcome.tone !== 'neutral');
}

/** The debugger link for a run (Investigate state). */
export function runLink(runId: string): { to: '/'; search: { run: string } } {
  return { to: '/', search: { run: runId } };
}

/**
 * rerunBlocker says why a suite cannot be rerun from its recorded inputs yet,
 * or undefined when it can: it must have finished, and every member
 * must have been admitted, since only an admitted member recorded inputs. The
 * server also refuses a member whose configuration read environment values.
 */
export function rerunBlocker(status: SuiteStatus): string | undefined {
  if (!status.done) return 'The suite is still running.';
  const missing = (status.members ?? []).find((m) => !m.run_id);
  if (missing) return `${missing.label} was never admitted, so it recorded no inputs to rerun.`;
  return undefined;
}

/** The JUnit XML download of a finished suite. */
export function junitHref(suiteId: string): string {
  return `/api/v1/suites/${encodeURIComponent(suiteId)}/junit`;
}
