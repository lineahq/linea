import { useEffect, useReducer, useRef, useState } from 'react';
import { Link, useBlocker, useSearch } from '@tanstack/react-router';
import { api } from '../../api/client';
import type { Bisection } from '../../api/types';
import { Icon, type IconName } from '../../ui/Icon';
import { getWorkspace, useWorkspace } from '../../workspace/store';
import { problemOf } from '../../runtimes/runtimes';
import { formatElapsed } from '../../runtimes/pages/elapsed';
import { checkReducer, initialCheck, isAbort, isRunning, blocksLeaving } from '../../runtimes/pages/upgradeMachine';
import { compareSearch } from '../../runtimes/pages/upgradeView';
import {
  bisectProblem, bisectRequest, bisectSummary, diffLines, LEAVE_BODY, LEAVE_TITLE, leaveQuestion, parseBisectSearch, versionRows,
  type BisectForm, type BisectSubject, type Tone,
} from '../bisect';
import { FleetHead } from './FleetHead';

const TONE_ICON: Record<Tone, IconName> = { pass: 'circle-check', fail: 'circle-x', warn: 'triangle-alert', neutral: 'circle-minus' };
const TONE_CHIP: Record<Tone, string> = { pass: 'pass', fail: 'fail', warn: 'warn', neutral: 'pending' };

/**
 * ConfigBisectPage finds the fleet config version that made the open
 * scenario fail on an agent. The platform rebuilds the agent's
 * configuration for each probed version and runs the scenario; the page
 * words its verdict, the versions it judged and the culprit's diff. The
 * bisect is one request bound to this page: while it runs, leaving asks
 * first and cancels it.
 */
export function ConfigBisectPage() {
  const search = parseBisectSearch(useSearch({ strict: false }) as Record<string, unknown>);
  const ws = useWorkspace();
  const [form, setForm] = useState<BisectForm>({ agent: search.agent ?? '', config: search.config ?? '', good: '', bad: '' });
  const [problem, setProblem] = useState<string>();
  const [state, dispatch] = useReducer(checkReducer<Bisection, BisectSubject>, initialCheck);
  const abort = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => abort.current?.abort(), []);
  const running = isRunning(state);

  const blocker = useBlocker({
    shouldBlockFn: () => blocksLeaving(state),
    disabled: !running,
    enableBeforeUnload: running,
    withResolver: true,
  });
  const stop = () => {
    abort.current?.abort();
    dispatch({ type: 'cancel' });
  };
  useEffect(() => {
    if (blocker.status === 'blocked' && !running) blocker.reset();
  }, [blocker, running]);

  const start = () => {
    const w = getWorkspace();
    const p = bisectProblem(form, w.scenarioYAML);
    setProblem(p);
    if (p || running) return;
    const ctrl = new AbortController();
    abort.current = ctrl;
    const id = state.id + 1;
    dispatch({ type: 'start', subject: { agent: form.agent.trim(), scenarioLabel: w.scenarioPath, startedAt: Date.now() } });
    api.bisectConfig(bisectRequest(form, w), ctrl.signal).then(
      (check) => dispatch({ type: 'finish', id, check, now: Date.now() }),
      (e: unknown) => { if (!isAbort(e)) dispatch({ type: 'fail', id, failure: problemOf(e) }); },
    );
  };
  const field = (key: keyof BisectForm, label: string, placeholder: string, width: number) => (
    <label className="od-field" style={{ width }}>
      <span className="od-field__label">{label}</span>
      <input className="od-input mono" value={form[key]} placeholder={placeholder} disabled={running}
        onChange={(e) => setForm((f) => ({ ...f, [key]: e.target.value }))} />
    </label>
  );

  return (
    <>
      <FleetHead />
      {blocker.status === 'blocked' && running ? (
        <div className="od-callout od-callout--warn od-callout--bar" role="alertdialog" aria-label="Leave the config bisect">
          <Icon name="triangle-alert" size={16} />
          <div className="od-callout__main">
            <span className="od-callout__title">{leaveQuestion(blocker.next.pathname)}</span>
          </div>
          <div className="od-callout__actions">
            <button type="button" className="od-btn od-btn--sm" onClick={() => blocker.reset()}>Keep searching</button>
            <button type="button" className="od-btn od-btn--danger od-btn--sm" onClick={() => { stop(); blocker.proceed(); }}>Cancel the bisect and leave</button>
          </div>
        </div>
      ) : running ? (
        <div className="od-callout od-callout--warn od-callout--bar" role="status">
          <Icon name="triangle-alert" size={16} />
          <div className="od-callout__main">
            <span className="od-callout__title">{LEAVE_TITLE}</span>
            <div className="od-callout__body">{LEAVE_BODY}</div>
          </div>
        </div>
      ) : null}

      <div className="od-page">
        <div className="rt-stack">
          <section aria-label="Find the breaking config version" className="rt-stack">
            <div className="od-card rt-row rt-checkbar" style={{ alignItems: 'flex-end', flexWrap: 'wrap' }}>
              {field('agent', 'Fleet agent', 'agent ID', 300)}
              {field('config', 'Config', 'only layer', 200)}
              {field('good', 'Good version', 'oldest kept', 120)}
              {field('bad', 'Bad version', 'sent now', 120)}
              <span className="rt-grow" />
              {running ? (
                <button type="button" className="od-btn od-btn--sm" onClick={stop}>
                  <Icon name="square" size={14} /><span>Stop</span>
                </button>
              ) : null}
              <button type="button" className="od-btn od-btn--primary od-btn--sm" disabled={running} onClick={start}>
                <Icon name={running ? 'loader-circle' : 'git-compare'} size={14} /><span>{running ? 'Searching…' : 'Find the version'}</span>
              </button>
            </div>
            <p className="od-muted rt-note">
              Tests with {ws.scenarioPath ? <code>{ws.scenarioPath}</code> : 'the open scenario'}{ws.runtime ? <> on runtime <code>{ws.runtime}</code></> : null}: a version is bad when one of its assertions fails.
              Each probed version of the config is merged with the agent's other fleet layers as the OpAMP supervisor merges them.
            </p>
            {problem && <div className="od-field__error" role="alert"><Icon name="circle-x" size={14} />{problem}</div>}
          </section>

          {running && (
            <div className="od-card" role="status">
              <div className="od-card__body rt-diag">
                <div className="od-progress od-progress--indet" role="progressbar" aria-label="Searching" aria-valuetext="Searching"><span /></div>
                <span className="od-muted rt-note">Running the scenario on the config's versions, halving the range each time. This can take a few minutes.</span>
              </div>
            </div>
          )}
          {state.phase === 'failed' && (
            <div className="od-callout od-callout--fail" role="alert">
              <Icon name="circle-x" size={16} />
              <div className="od-callout__main">
                <span className="od-callout__title">The bisect failed</span>
                <div className="od-callout__body">{state.failure.message}</div>
                {state.failure.details.length > 0 && <ul>{state.failure.details.map((d, i) => <li key={i}>{d}</li>)}</ul>}
              </div>
            </div>
          )}
          {state.phase === 'cancelled' && (
            <div className="od-callout od-callout--warn" role="status">
              <Icon name="circle-minus" size={16} />
              <div className="od-callout__main">
                <span className="od-callout__title">Bisect cancelled</span>
                <div className="od-callout__body">No result was kept. The run in progress was cancelled.</div>
              </div>
            </div>
          )}
          {state.phase === 'done' && <Result b={state.check} subject={state.subject} elapsedMs={state.elapsedMs} />}
        </div>
      </div>
    </>
  );
}

function Result({ b, subject, elapsedMs }: { b: Bisection; subject: BisectSubject; elapsedMs: number }) {
  const summary = bisectSummary(b);
  const rows = versionRows(b);
  const lines = b.culprit ? diffLines(b.culprit.diff) : [];
  return (
    <>
      <div className="od-card rt-row rt-checkbar">
        <span className="od-section-label">Searched</span>
        <code>{b.target.config_name || b.target.config_id}</code>
        <span className="od-subtle">layer <code>{b.target.entry}</code> · versions {b.good}–{b.bad} · {b.probes} probed</span>
        <span className="rt-grow" />
        <span className="od-subtle rt-note">{formatElapsed(elapsedMs)} · {new Date(subject.startedAt).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}</span>
      </div>

      <div className={`od-callout od-callout--${summary.tone === 'neutral' ? 'warn' : summary.tone}`} role="status">
        <Icon name={TONE_ICON[summary.tone]} size={16} />
        <div className="od-callout__main">
          <div className="od-callout__title">{summary.title}</div>
          <div className="od-callout__body">{summary.body}</div>
          {(b.reasons ?? []).map((r) => <div key={r.code} className="od-callout__body"><code>{r.code}</code> {r.message}</div>)}
        </div>
      </div>

      {b.culprit && (
        <section className="od-card" style={{ overflow: 'hidden' }} aria-label="What changed">
          <div className="od-card__head"><h2 className="od-card__title">Version {b.culprit.last_good} → {b.culprit.version}</h2></div>
          <div className="od-card__body rt-stack">
            {lines.length === 0 ? <p className="od-muted rt-note">The two versions' bodies are identical.</p> : (
              <div className="unified mono">
                {lines.map((l, i) => (
                  <div key={i} className={`u-${l.kind}`}>
                    <span className="sign">{{ add: '+', del: '-', ctx: ' ', hunk: '' }[l.kind]}</span>
                    {l.text}
                  </div>
                ))}
              </div>
            )}
            {(b.culprit.scenarios ?? []).map((s) => {
              const cmp = compareSearch([s.good, s.bad]);
              return (
                <div key={s.name} className="rt-row">
                  <code>{s.name}</code>
                  <span className="od-subtle">assertions {s.good_assertions} → {s.bad_assertions}{s.error ? ` · not compared: ${s.error}` : s.inputs_identical ? '' : ' · inputs not proven identical'}</span>
                  <span className="rt-grow" />
                  {cmp && !s.error && (
                    <Link className="od-btn od-btn--link od-btn--sm" to="/" search={cmp}>
                      <span>Open comparison</span><Icon name="arrow-right" size={14} />
                    </Link>
                  )}
                </div>
              );
            })}
          </div>
        </section>
      )}

      <section className="od-card" style={{ overflow: 'hidden' }} aria-label="Versions">
        <div className="od-card__head"><h2 className="od-card__title">Versions</h2></div>
        <table className="od-table">
          <thead><tr><th>Version</th><th>Result</th><th>Created</th><th>Message</th><th>Rollouts</th><th>Runs</th></tr></thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.version} aria-current={r.culprit ? 'true' : undefined}>
                <td className="num">{r.version}{r.culprit ? ' · first bad' : r.lastGood ? ' · last good' : ''}</td>
                <td>
                  <span className={`od-chip od-chip--${TONE_CHIP[r.tone]}`} title={r.reason || undefined}><Icon name={TONE_ICON[r.tone]} size={12} />{r.status}</span>
                  {r.reason && <div className="od-subtle rt-note">{r.reason}</div>}
                </td>
                <td className="mono od-subtle">{r.created}</td>
                <td>{r.message}</td>
                <td className="od-subtle">{r.rollouts || '—'}</td>
                <td>
                  {r.runs.map((run) => (
                    <Link key={run.run} className="od-btn od-btn--link od-btn--sm" to="/" search={{ run: run.run }}>
                      <span>{run.outcome}</span><Icon name="arrow-right" size={14} />
                    </Link>
                  ))}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>
      {(b.notes ?? []).map((n, i) => <p key={i} className="od-muted rt-note">{n}</p>)}
    </>
  );
}
