import { useEffect, useReducer, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link, useBlocker } from '@tanstack/react-router';
import { api } from '../../api/client';
import type { UpgradeCheck } from '../../api/types';
import { Icon, type IconName } from '../../ui/Icon';
import { getWorkspace, useWorkspace } from '../../workspace/store';
import { runtimesQuery, supportQuery } from '../RuntimeSelect';
import { problemOf } from '../runtimes';
import { scenarioRows, upgradeProblem, upgradeRequest, upgradeSides } from '../upgrade';
import { formatElapsed } from './elapsed';
import { RuntimesHead } from './RuntimesHead';
import {
  blocksLeaving, checkReducer, initialCheck, isAbort, isRunning, LEAVE_BODY, LEAVE_TITLE, leaveQuestion, type CheckSubject,
} from './upgradeMachine';
import { checkedLine, compareSearch, diffRows, firstDifference, sideLine, summaryOf, upgradeFacets, type Facet } from './upgradeView';

const TONE_CHIP = { good: 'pass', bad: 'fail', warn: 'warn' } as const;
const TONE_ICON: Record<'good' | 'bad' | 'warn', IconName> = { good: 'circle-check', bad: 'circle-x', warn: 'triangle-alert' };

/**
 * UpgradeCheckPage checks the configuration draft, and the scenario draft
 * when there is one, on a baseline and a candidate runtime and words
 * the verdict, the differences and what is uncertain. The check is one request
 * bound to this page: while it runs, leaving asks first and cancels it. The two
 * pickers name the sides of the check only; runs keep the runtime chosen
 * beside Run.
 */
export function UpgradeCheckPage() {
  const ws = useWorkspace();
  const runtimes = useQuery(runtimesQuery);
  const support = useQuery(supportQuery);
  const sides = upgradeSides(runtimes.data, support.data?.built_in);
  const [pickedFrom, setFrom] = useState<string>();
  const [pickedTo, setTo] = useState<string>();
  const from = pickedFrom ?? ws.runtime ?? '';
  const to = pickedTo ?? sides.at(-1)?.value ?? '';
  const [problem, setProblem] = useState<string>();
  const [state, dispatch] = useReducer(checkReducer<UpgradeCheck, CheckSubject>, initialCheck);
  const abort = useRef<AbortController | undefined>(undefined);
  // The request is bound to this page: unmounting aborts it, which cancels the check's runs.
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
  // A check that ends while the question is open leaves nothing to cancel: stay.
  useEffect(() => {
    if (blocker.status === 'blocked' && !running) blocker.reset();
  }, [blocker, running]);

  const start = () => {
    const w = getWorkspace();
    const p = upgradeProblem(w.configYAML, from, to);
    setProblem(p);
    if (p || running) return;
    const req = upgradeRequest(w, from, to);
    const ctrl = new AbortController();
    abort.current = ctrl;
    const id = state.id + 1;
    const subject: CheckSubject = { configLabel: w.configPath, scenarios: req.scenarios?.length ?? 0, from, to, startedAt: Date.now() };
    dispatch({ type: 'start', subject });
    api.checkUpgrade(req, ctrl.signal).then(
      (check) => dispatch({ type: 'finish', id, check, now: Date.now() }),
      (e: unknown) => { if (!isAbort(e)) dispatch({ type: 'fail', id, failure: problemOf(e) }); },
    );
  };

  const draft = checkedLine({ configLabel: ws.configPath, scenarios: ws.scenarioYAML.trim() ? 1 : 0, from, to, startedAt: 0 });
  const side = (label: string, role: string, value: string, onChange: (v: string) => void) => {
    const known = sides.find((s) => s.value === value);
    return (
      <div className="od-cmp__side">
        <span className="od-cmp__role">{role}</span>
        <div className="od-field">
          <select className="od-select" aria-label={`${label} runtime`} value={value} disabled={running} onChange={(e) => onChange(e.target.value)}>
            {sides.map((s) => <option key={s.value} value={s.value}>{s.label}</option>)}
          </select>
        </div>
        <span className="od-subtle rt-note">{known ? (known.unverified ? 'unverified release' : 'verified release') : 'not found'}</span>
      </div>
    );
  };

  return (
    <>
      <RuntimesHead />
      {blocker.status === 'blocked' && running ? (
        <div className="od-callout od-callout--warn od-callout--bar" role="alertdialog" aria-label="Leave the upgrade check">
          <Icon name="triangle-alert" size={16} />
          <div className="od-callout__main">
            <span className="od-callout__title">{leaveQuestion(blocker.next.pathname)}</span>
          </div>
          <div className="od-callout__actions">
            <button type="button" className="od-btn od-btn--sm" onClick={() => blocker.reset()}>Keep checking</button>
            <button type="button" className="od-btn od-btn--danger od-btn--sm" onClick={() => { stop(); blocker.proceed(); }}>Cancel the check and leave</button>
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
          <section aria-label="Check upgrade" className="rt-stack">
            <div className="od-cmp">
              {side('From', 'Baseline · current runtime', from, setFrom)}
              <div className="od-cmp__mid"><Icon name="arrow-right" size={20} /></div>
              {side('To', 'Candidate · target runtime', to, setTo)}
            </div>
            <div className="od-card rt-row rt-checkbar">
              <span className="od-section-label">Checks</span>
              <code>{draft.config}</code>
              <span className="od-subtle">{draft.scenarios}</span>
              <span className="rt-grow" />
              {running ? (
                <button type="button" className="od-btn od-btn--sm" onClick={stop}>
                  <Icon name="square" size={14} /><span>Stop the check</span>
                </button>
              ) : null}
              <button type="button" className="od-btn od-btn--primary od-btn--sm" disabled={running} onClick={start}>
                <Icon name={running ? 'loader-circle' : 'git-compare'} size={14} /><span>{running ? 'Checking…' : state.phase === 'idle' ? 'Check upgrade' : 'Check again'}</span>
              </button>
            </div>
            {problem && <div className="od-field__error" role="alert"><Icon name="circle-x" size={14} />{problem}</div>}
            {state.phase === 'idle' && !problem && (
              <p className="od-muted rt-note">
                Validates the open configuration on both runtimes and runs the open scenario on both with the same input. Build the target release first on the Runtimes page.
              </p>
            )}
          </section>

          {running && (
            <div className="od-card" role="status">
              <div className="od-card__body rt-diag">
                <div className="od-progress od-progress--indet" role="progressbar" aria-label="Checking" aria-valuetext="Checking"><span /></div>
                <span className="od-muted rt-note">Checking {draft.config} {draft.scenarios} on both runtimes. This can take a minute.</span>
              </div>
            </div>
          )}
          {state.phase === 'failed' && (
            <div className="od-callout od-callout--fail" role="alert">
              <Icon name="circle-x" size={16} />
              <div className="od-callout__main">
                <span className="od-callout__title">The check failed</span>
                <div className="od-callout__body">{state.failure.message}</div>
                {state.failure.details.length > 0 && <ul>{state.failure.details.map((d, i) => <li key={i}>{d}</li>)}</ul>}
              </div>
            </div>
          )}
          {state.phase === 'cancelled' && (
            <div className="od-callout od-callout--warn" role="status">
              <Icon name="circle-minus" size={16} />
              <div className="od-callout__main">
                <span className="od-callout__title">Check cancelled</span>
                <div className="od-callout__body">No result was kept. Its runs were cancelled.</div>
              </div>
            </div>
          )}
          {state.phase === 'done' && <Result check={state.check} subject={state.subject} elapsedMs={state.elapsedMs} />}
        </div>
      </div>
    </>
  );
}

function Result({ check, subject, elapsedMs }: { check: UpgradeCheck; subject: CheckSubject; elapsedMs: number }) {
  const summary = summaryOf(check);
  const facets = upgradeFacets(check);
  const rows = scenarioRows(check);
  const diffs = diffRows(check);
  const open = firstDifference(check);
  const checked = checkedLine(subject);
  return (
    <>
      <div className="od-cmp" aria-label="Runtimes checked">
        <div className="od-cmp__side">
          <span className="od-cmp__role">Baseline</span>
          <div className="t-code-strong">{check.from.name} {check.from.collector_version}</div>
          <span className="od-subtle rt-note">{sideLine(check.from)}</span>
        </div>
        <div className="od-cmp__mid"><Icon name="arrow-right" size={20} /></div>
        <div className="od-cmp__side">
          <span className="od-cmp__role">Candidate</span>
          <div className="t-code-strong">{check.to.name} {check.to.collector_version}</div>
          <span className="od-subtle rt-note">{sideLine(check.to)}</span>
        </div>
      </div>

      <div className="od-card rt-row rt-checkbar">
        <span className="od-section-label">Checked</span>
        <code>{checked.config}</code>
        <span className="od-subtle">{checked.scenarios}</span>
        <span className="rt-grow" />
        <span className="od-chip od-chip--pass"><Icon name="circle-check" size={12} />Completed</span>
        <span className="od-subtle rt-note">{formatElapsed(elapsedMs)} · {new Date(subject.startedAt).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}</span>
      </div>

      <div className="od-facets" role="list" aria-label="What the check found">
        {facets.map((f) => <FacetCell key={f.name} f={f} />)}
      </div>

      <div className={`od-callout od-callout--${summary.tone}`} role="status">
        <Icon name={summary.tone === 'pass' ? 'circle-check' : summary.tone === 'fail' ? 'circle-x' : 'triangle-alert'} size={16} />
        <div className="od-callout__main">
          <div className="od-callout__title">{summary.title}</div>
          {summary.body && <div className="od-callout__body">{summary.body}</div>}
          {(check.reasons ?? []).map((r) => <div key={r.code} className="od-callout__body"><code>{r.code}</code> {r.message}</div>)}
          {open && (
            <div className="od-callout__actions">
              <Link className="od-btn od-btn--link od-btn--sm" to="/" search={open.search}>
                <span>Open the difference in {open.name}</span><Icon name="arrow-right" size={14} />
              </Link>
            </div>
          )}
        </div>
      </div>

      <section className="od-card" style={{ overflow: 'hidden' }} aria-label="Scenarios">
        <div className="od-card__head"><h2 className="od-card__title">Scenarios</h2></div>
        {rows.length === 0 ? (
          <div className="od-card__body od-muted rt-note">No scenario was compared: the check validated the configuration only.</div>
        ) : (
          <table className="od-table">
            <thead><tr><th>Scenario</th><th>Result</th><th>Assertions</th><th>Inputs</th><th /></tr></thead>
            <tbody>
              {rows.map((s, i) => {
                const search = compareSearch(s.runs);
                return (
                  <tr key={i}>
                    <td className="mono">{s.name}</td>
                    <td><span className={`od-chip od-chip--${TONE_CHIP[s.cls]}`}><Icon name={TONE_ICON[s.cls]} size={12} />{s.result}</span></td>
                    <td>{s.assertions}</td>
                    <td>{s.inputs}</td>
                    <td style={{ textAlign: 'right' }}>
                      {search && (
                        <Link className="od-btn od-btn--link od-btn--sm" to="/" search={search}>
                          <span>Open comparison</span><Icon name="arrow-right" size={14} />
                        </Link>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </section>

      <section className="od-card" style={{ overflow: 'hidden' }} aria-label="Feature gates and diagnostics">
        <div className="od-card__head"><h2 className="od-card__title">Feature gates and diagnostics</h2></div>
        <div className="od-card__body">
          {diffs.length === 0 ? (
            <p className="od-muted rt-note">
              {check.feature_gates.unknown?.length
                ? `Feature gates were not compared: the ${check.feature_gates.unknown.join(' and ')} runtime does not list them. Both runtimes report the same diagnostics.`
                : 'Both runtimes report the same diagnostics, and no feature gate of the configuration\'s components, or of the Collector itself, changed.'}
            </p>
          ) : (
            <table className="od-diff">
              <tbody>
                {diffs.map((d, i) => (
                  <tr key={i} className={`od-diff__row--${d.kind}`}>
                    <td><Icon name={d.kind === 'added' ? 'diff-added' : d.kind === 'removed' ? 'diff-removed' : 'diff-changed'} size={14} title={d.kind} /></td>
                    <td className="od-diff__path">{d.path}</td>
                    <td className="od-diff__before">{d.before}</td>
                    <td className="od-diff__after">{d.after}</td>
                    <td className="od-diff__by">{d.by}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </section>
      {(check.notes ?? []).map((n, i) => <p key={i} className="od-muted rt-note">{n}</p>)}
    </>
  );
}

function FacetCell({ f }: { f: Facet }) {
  return (
    <div className="od-facet" role="listitem">
      <span className="od-facet__name">{f.name}</span>
      <span className={`od-facet__value od-facet__value--${f.tone}`}><Icon name={f.icon} size={14} />{f.value}</span>
      <span className="od-facet__detail">{f.detail}</span>
    </div>
  );
}
