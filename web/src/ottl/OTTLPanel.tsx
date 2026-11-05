import { Fragment, useState } from 'react';
import { useMutation } from '@tanstack/react-query';
import { api } from '../api/client';
import type { OTTLEvaluation, OTTLGroup, OTTLReport, OTTLStatement } from '../api/types';
import { getWorkspace, updateWorkspace, useWorkspace } from '../workspace/store';
import { useStartRun } from '../runs/useStartRun';
import { ChangesTable } from '../inspect/ChangesTable';
import { SideBySide } from '../inspect/DiffViews';
import { pretty } from '../inspect/records';
import { runBlocker } from '../input/quickInput';
import { editProblem } from './candidates';
import { statementTestOutcome, statementTestRequest } from './statementTest';
import { runLabel } from '../debugger/runFacts';
import { runtimeField } from '../runtimes/runtimes';

const VERIFICATION: Record<string, { cls: string; text: string }> = {
  replay_verified: {
    cls: 'good',
    text: "The replay reproduces this processor's actual output, so the statement results below explain it.",
  },
  disagrees: {
    cls: 'bad',
    text: 'The replay produced something other than the processor did: do not read the statement results as attribution.',
  },
  not_verifiable: {
    cls: 'warn',
    text: 'Part of the configuration was not replayed, or there was no output to compare with: statement results are unconfirmed.',
  },
};

interface Props {
  report?: OTTLReport;
  /** The stage input and the processor's observed output (OTLP JSON). */
  input: unknown;
  observed: unknown;
  onRecord(entity: string | undefined): void;
  onRerun(runId: string): void;
  /** The shown run: with onCompare, an edit can be tested on its recorded inputs. */
  runId?: string;
  /** Compare the shown run with the given run. */
  onCompare?(runId: string): void;
}

/**
 * OTTLPanel explains a transform processor's work on one input statement by
 * statement, and lets a statement be edited, tried on this
 * input without a run, tested on the run's recorded inputs, written into
 * the configuration and rerun.
 */
export function OTTLPanel({ report, input, observed, onRecord, onRerun, runId, onCompare }: Props) {
  if (!report) {
    return (
      <p className="muted">
        No OTTL replay for this input: inputs that were deferred, or split into several outputs, are not replayed.
      </p>
    );
  }
  const v = VERIFICATION[report.verification] ?? { cls: 'idle', text: report.verification };
  return (
    <div className="ottl">
      <p>
        <span className={`status status-${v.cls}`}>{report.verification.replace('_', ' ')}</span> {v.text}
      </p>
      {report.error && <p className="error">Processing stopped: {report.error}</p>}
      {(report.notes ?? []).map((n, i) => (
        <p key={i} className="note">
          {n}
        </p>
      ))}
      {report.mismatch && (
        <p className="note">
          The replay differs from the observed output in {report.mismatch.changes?.length ?? 0} field(s)
          {report.mismatch.added?.length ? `, ${report.mismatch.added.length} added record(s)` : ''}
          {report.mismatch.removed?.length ? `, ${report.mismatch.removed.length} removed record(s)` : ''}.
        </p>
      )}
      {(report.groups ?? []).map((g) => (
        <GroupView key={g.index} report={report} group={g} input={input} observed={observed} onRecord={onRecord} onRerun={onRerun} runId={runId} onCompare={onCompare} />
      ))}
    </div>
  );
}

function GroupView({ report, group, ...rest }: { report: OTTLReport; group: OTTLGroup } & Omit<Props, 'report'>) {
  return (
    <section className="ottl-group">
      <h4>
        Group {group.index}
        {group.context && <span className="muted"> · context {group.context}</span>}
        <span className="muted"> · error_mode {group.error_mode}</span>{' '}
        <span className={`badge group-${group.status}`}>{group.status.replace('_', ' ')}</span>
      </h4>
      {group.reason && <p className="note">{group.reason}</p>}
      {(group.conditions ?? []).length > 0 && (
        <p className="note">
          Conditions: <code>{group.conditions!.join(' or ')}</code>
        </p>
      )}
      <table className="grid statements">
        <thead>
          <tr>
            <th>#</th>
            <th>Statement</th>
            <th title="Where clause true (or no where clause)">Matched</th>
            <th title="Where clause false">Not matched</th>
            <th title="Matched, but changed nothing">Unchanged</th>
            <th>Errors</th>
            <th>Changes</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {(group.statements ?? []).map((s) => (
            <StatementRow key={s.index} report={report} group={group} statement={s} {...rest} />
          ))}
        </tbody>
      </table>
    </section>
  );
}

function StatementRow({
  report,
  group,
  statement: s,
  input,
  observed,
  onRecord,
  onRerun,
  runId,
  onCompare,
}: { report: OTTLReport; group: OTTLGroup; statement: OTTLStatement } & Omit<Props, 'report'>) {
  const ws = useWorkspace();
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState<string | undefined>();
  const [problem, setProblem] = useState<string>();
  const start = useStartRun(ws, onRerun);
  // The server splices the edit into the configuration draft's text; the
  // statement there must still read as it did in this run.
  const proposal = useMutation({
    mutationFn: () =>
      api.createCandidate({
        config_yaml: getWorkspace().configYAML,
        ...runtimeField(getWorkspace().runtime),
        edit: { processor: report.processor, signal: report.signal, group: group.index, index: s.index, op: 'replace', text: draft ?? s.text, expected: s.text },
      }),
  });
  const edited = async () => {
    setProblem(undefined);
    try {
      const c = await proposal.mutateAsync();
      if (!c.valid) {
        const first = c.diagnostics?.find((d) => d.severity === 'error') ?? c.diagnostics?.[0];
        setProblem(`The edited configuration does not validate${first ? `: ${first.message}` : ''}.`);
      }
      return c.proposed_yaml;
    } catch (e) {
      setProblem(editProblem(e));
      return undefined;
    }
  };
  const tryIt = useMutation({
    mutationFn: (yaml: string) =>
      api.evaluateOTTL({
        config_yaml: yaml,
        scenario_yaml: ws.scenarioYAML.trim() ? ws.scenarioYAML : undefined,
        processor: report.processor,
        input,
        ...runtimeField(ws.runtime),
      }),
  });
  // The server edits the run's recorded configuration, so the editor's draft
  // plays no part, and runs it on the run's inputs and runtime.
  const test = useMutation({
    mutationFn: () => api.testStatementOnRun(runId!, statementTestRequest(report, group.index, s.index, s.text, draft ?? s.text)),
  });
  const tested = test.data && statementTestOutcome(test.data);
  const errors = s.errors ?? [];
  const changes = s.changes ?? [];
  const noisy = errors.length > 0;
  return (
    <Fragment>
      <tr className={noisy ? 'has-errors' : undefined}>
        <td>{s.index}</td>
        <td>
          <code title={s.effective ? `Parsed as: ${s.effective}` : undefined}>{s.text}</code>
        </td>
        <td>{s.matched}</td>
        <td>{s.not_matched}</td>
        <td>{s.matched_without_change ?? 0}</td>
        <td className={noisy ? 'error' : undefined}>{errors.length}</td>
        <td>{changes.length}</td>
        <td className="actions">
          {(errors.length > 0 || changes.length > 0) && (
            <button type="button" onClick={() => setOpen(!open)} aria-expanded={open}>
              {open ? 'Hide' : 'Details'}
            </button>
          )}
          <button type="button" onClick={() => setDraft(draft === undefined ? s.text : undefined)}>
            {draft === undefined ? 'Edit' : 'Close'}
          </button>
        </td>
      </tr>
      {open && (
        <tr className="expanded">
          <td />
          <td colSpan={7}>
            {errors.map((e, i) => (
              <div key={i} className="error">
                {e.phase} error on <code>{e.entity}</code>: {e.message}
              </div>
            ))}
            {changes.length > 0 && <ChangesTable changes={changes} onRecord={onRecord} />}
          </td>
        </tr>
      )}
      {draft !== undefined && (
        <tr className="expanded">
          <td />
          <td colSpan={7}>
            <div className="statement-edit">
              <textarea
                className="mono"
                value={draft}
                rows={Math.min(6, Math.max(2, Math.ceil(draft.length / 100)))}
                onChange={(e) => setDraft(e.target.value)}
                aria-label={`Statement ${s.index}`}
                spellCheck={false}
              />
              <div className="edit-actions">
                <button
                  type="button"
                  disabled={tryIt.isPending || proposal.isPending}
                  title="Show what the edited statement does to the data this processor received here. Instant; starts no run."
                  onClick={async () => {
                    const y = await edited();
                    if (y) tryIt.mutate(y);
                  }}
                >
                  Preview on this input
                </button>
                {runId && onCompare && (
                  <button
                    type="button"
                    disabled={test.isPending}
                    title="Run the whole pipeline again on all of this run's recorded inputs, with the edited statement in the run's configuration, and check its assertions. Your editor is not changed."
                    onClick={() => test.mutate()}
                  >
                    Test on the whole run
                  </button>
                )}
                <button
                  type="button"
                  disabled={proposal.isPending}
                  title="Write the edited statement into the configuration editor (the file on disk is not touched)"
                  onClick={async () => {
                    const y = await edited();
                    if (y) updateWorkspace({ configYAML: y });
                  }}
                >
                  Apply to config
                </button>
                <button
                  type="button"
                  className="primary"
                  disabled={start.isPending || proposal.isPending || !!runBlocker(ws, true)}
                  title="Apply to the configuration editor and run again"
                  onClick={async () => {
                    const y = await edited();
                    if (!y) return;
                    updateWorkspace({ configYAML: y });
                    start.mutate({ configYAML: y });
                  }}
                >
                  Apply and rerun
                </button>
              </div>
              {problem && <p className="error">{problem}</p>}
              {tryIt.error && <p className="error">{tryIt.error.message}</p>}
              {start.error && <p className="error">{start.error.message}</p>}
              {test.error && <p className="error">{test.error.message}</p>}
              {tested?.kind === 'invalid' && <p className="error">{tested.message}</p>}
              {tested?.kind === 'queued' && (
                <div className="try-result" role="status">
                  <p>Started test run {runLabel(tested.runId)}. {tested.note}</p>
                  <div className="edit-actions">
                    <button type="button" className="primary" onClick={() => onRerun(tested.runId)}>Open test run</button>
                    <button type="button" title="Compare the test run with this run, record by record" onClick={() => onCompare?.(tested.runId)}>Compare with this run</button>
                  </div>
                </div>
              )}
              {tryIt.data && <TryResult ev={tryIt.data} group={group.index} statement={s.index} observed={observed} onRecord={onRecord} />}
            </div>
          </td>
        </tr>
      )}
    </Fragment>
  );
}

function TryResult({
  ev,
  group,
  statement,
  observed,
  onRecord,
}: {
  ev: OTTLEvaluation;
  group: number;
  statement: number;
  observed: unknown;
  onRecord(e: string | undefined): void;
}) {
  const [compare, setCompare] = useState(false);
  if (!ev.valid) {
    return (
      <div className="try-result">
        <p className="error">The edited configuration is not valid:</p>
        <ul>
          {(ev.diagnostics ?? []).map((d, i) => (
            <li key={i} className="error">
              {d.message}
            </li>
          ))}
        </ul>
      </div>
    );
  }
  const st = ev.replay?.groups?.find((g) => g.index === group)?.statements?.find((x) => x.index === statement);
  return (
    <div className="try-result">
      {st && (
        <p>
          Edited statement: matched <strong>{st.matched}</strong>, not matched <strong>{st.not_matched}</strong>, unchanged{' '}
          {st.matched_without_change ?? 0}, errors <strong className={st.errors?.length ? 'error' : undefined}>{st.errors?.length ?? 0}</strong>,
          changes {st.changes?.length ?? 0}.
        </p>
      )}
      {(st?.errors ?? []).map((e, i) => (
        <div key={i} className="error">
          {e.phase} error on <code>{e.entity}</code>: {e.message}
        </div>
      ))}
      {ev.replay?.error && <p className="error">Processing stopped: {ev.replay.error}</p>}
      {(ev.replay?.notes ?? []).map((n, i) => (
        <p key={i} className="note">
          {n}
        </p>
      ))}
      {ev.output !== undefined && (
        <>
          <h5>
            What the processor would now change in this input{' '}
            <label className="inline">
              <input type="checkbox" checked={compare} onChange={(e) => setCompare(e.target.checked)} /> compare with the observed
              output
            </label>
          </h5>
          {compare ? (
            <SideBySide before={pretty(observed)} after={pretty(ev.output)} />
          ) : (
            <ChangesTable changes={ev.diff?.changes ?? []} onRecord={onRecord} />
          )}
        </>
      )}
    </div>
  );
}
