import type { Report, Run } from '../api/types';

export interface Limitation {
  level: 'error' | 'warning' | 'info';
  text: string;
}

/**
 * limitations lists what a run's evidence does not show, so a
 * result is never read as stronger than it is: incomplete runs, components
 * that did not execute for real, unverified OTTL replays, unresolved stage
 * outcomes, mocked dependencies and a missing reference comparison.
 */
export function limitations(run: Run): Limitation[] {
  const out: Limitation[] = [];
  if (run.state === 'error') {
    out.push({ level: 'error', text: `The engine could not run the request: ${run.error ?? 'unknown error'}` });
    return out;
  }
  if (run.state === 'cancelled') {
    out.push({ level: 'warning', text: 'The run was cancelled before it started.' });
    return out;
  }
  const rep = run.report;
  if (!rep) return out;
  const res = rep.result;
  switch (res?.status) {
    case 'partial':
      out.push({
        level: 'warning',
        text: `The run hit its deadline${res.status_reason ? ` (${res.status_reason})` : ''}: observations are incomplete and missing output proves nothing.`,
      });
      break;
    case 'cancelled':
      out.push({ level: 'warning', text: 'The run was cancelled: observations are incomplete and missing output proves nothing.' });
      break;
    case 'failed':
      out.push({ level: 'error', text: `The Collector reported an error${res.status_reason ? `: ${res.status_reason}` : ''}.` });
      break;
  }
  for (const input of res?.inputs ?? []) {
    if (input.error) out.push({ level: 'warning', text: `Input to ${input.receiver} (${input.signal}) was rejected: ${input.error}` });
  }
  for (const c of rep.topology?.components ?? []) {
    // Stand-ins have their own section in the overview (standins.ts).
    if (c.mode !== 'real' && c.mode !== 'stand_in' && c.kind !== 'extension') {
      out.push({ level: 'info', text: `${c.kind} ${c.id} is ${c.mode.replace('_', ' ')}${c.note ? `: ${c.note}` : ''}.` });
    }
  }
  // What each mocked dependency supplied and where it differs from a real
  // deployment; warnings mean a fixture had no effect.
  for (const d of res?.mocks ?? []) {
    for (const w of d.warnings ?? []) out.push({ level: 'warning', text: `${d.component} (${d.provider} mock): ${w}.` });
    for (const l of d.limitations ?? []) out.push({ level: 'info', text: `${d.component} (${d.provider} mock): ${l}.` });
  }
  // Expressions reading the wall clock compare it with the input
  // timestamps: as recorded, the outcome depends on when the run happened.
  const ts = rep.timestamps;
  if (ts?.mode === 'rebased' && ts.offset) {
    out.push({ level: 'info', text: `Input timestamps were shifted by ${ts.offset} so the latest record falls at the run's start.` });
  }
  for (const c of ts?.wall_clock ?? []) {
    if (ts?.mode === 'rebased') continue;
    const age = ts?.offset ? ` and the latest input record was ${ts.offset} old` : '';
    out.push({
      level: 'warning',
      text: `${c.component} reads the wall clock (${c.expression})${age}: the outcome depends on when the run happened. Declare execution.timestamps: rebase to shift the inputs to the run's time.`,
    });
  }
  out.push(...outcomeLimitations(rep));
  for (const o of rep.ottl ?? []) {
    if (o.verification === 'disagrees') {
      out.push({ level: 'warning', text: `OTTL replay of ${o.processor} (input #${o.input}) disagrees with the observed output.` });
    } else if (o.verification === 'not_verifiable') {
      out.push({ level: 'info', text: `OTTL replay of ${o.processor} (input #${o.input}) could not be verified${o.notes?.length ? `: ${o.notes.join('; ')}` : ''}.` });
    }
  }
  if (rep.comparison && !rep.comparison.equal) {
    out.push({ level: 'error', text: `The uninstrumented reference run produced different output: ${(rep.comparison.differences ?? []).join('; ')}` });
  } else if (!rep.comparison) {
    out.push({ level: 'info', text: 'No reference comparison: enable Compare to check that instrumentation did not change the output.' });
  }
  return out;
}

function outcomeLimitations(rep: Report): Limitation[] {
  let deferred = 0;
  let unresolved = 0;
  for (const s of rep.stages ?? []) {
    for (const i of s.inputs ?? []) {
      if (i.outcome === 'deferred') deferred++;
      if (i.outcome === 'unresolved') unresolved++;
    }
  }
  const out: Limitation[] = [];
  if (deferred) {
    out.push({ level: 'info', text: `${deferred} stage input(s) were buffered and emitted later (for example by batch or tail_sampling); their processor compares everything it buffered with everything it emitted later.` });
  }
  if (unresolved) {
    out.push({ level: 'warning', text: `${unresolved} stage input(s) have no attributable output; their outcome is unknown.` });
  }
  return out;
}
