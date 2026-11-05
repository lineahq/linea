import { useEffect, useMemo, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import type { OTTLEvaluation } from '../api/types';
import { YamlEditor } from '../editor/YamlEditor';
import { RecordChanges } from '../inspect/RecordChanges';
import { recordViews } from '../inspect/recordViews';
import { JSON_LANGUAGE } from '../monaco';
import { runtimeField, UNVERIFIED } from '../runtimes/runtimes';
import { Icon } from '../ui/Icon';
import { PageHead } from '../ui/PageHead';
import { useDebounced } from '../useDebounced';
import { useWorkspace } from '../workspace/store';
import { WorkspacePanes } from '../workspace/WorkspacePanes';
import {
  checkInput, ERROR_MODES, evaluatedOn, EXAMPLES, exampleText, PROCESSOR, playgroundConfig, processorYAML, recordSummary, replacesWork,
  statementLines, statementResults, type ErrorMode, type Signal,
} from './playground';
import './playground.css';

const STORE = 'od.playground';
const PANES = 'od.playground.panes';
// Statements are a few lines; the result needs the room.
const PANE_DEFAULTS = { left: 0.34, upper: 0.3 };

interface Draft {
  input: string;
  statements: string;
  errorMode: ErrorMode;
}

const first: Draft = { input: exampleText(EXAMPLES[0]), statements: EXAMPLES[0].statements, errorMode: 'propagate' };

// The draft is a per-browser convenience: without storage the page starts
// from the first example.
function loadDraft(): Draft {
  try {
    const d = JSON.parse(localStorage.getItem(STORE) ?? 'null') as Partial<Draft> | null;
    if (d && typeof d.input === 'string' && typeof d.statements === 'string') {
      return { input: d.input, statements: d.statements, errorMode: ERROR_MODES.includes(d.errorMode as ErrorMode) ? (d.errorMode as ErrorMode) : 'propagate' };
    }
  } catch {
    // Unavailable or unreadable storage.
  }
  return first;
}

/**
 * PlaygroundPage evaluates OTTL statements on a pasted OTLP JSON payload
 * (`POST /ottl/evaluate`). It is not tied to the open configuration, a
 * scenario or a run: it writes a configuration holding one transform
 * processor with the statements and evaluates it as the edits settle, on
 * the runtime selected beside Run. Nothing is run or saved. It is laid out
 * like the debugger: the input on the left, the statements above the result.
 */
export function PlaygroundPage() {
  const [draft, setDraft] = useState(loadDraft);
  const [asking, setAsking] = useState<(typeof EXAMPLES)[number]>();
  const [copied, setCopied] = useState(false);
  const ws = useWorkspace();
  const update = (patch: Partial<Draft>) => setDraft((d) => ({ ...d, ...patch }));

  useEffect(() => {
    try {
      localStorage.setItem(STORE, JSON.stringify(draft));
    } catch {
      // A draft that cannot be kept is only lost on reload.
    }
  }, [draft]);

  const settled = useDebounced(draft, 400);
  const input = useMemo(() => checkInput(settled.input), [settled.input]);
  const lines = useMemo(() => statementLines(settled.statements), [settled.statements]);
  const config = input.ok && lines.length ? playgroundConfig(input.signal, lines.map((l) => l.text), settled.errorMode) : undefined;
  const evaluation = useQuery({
    queryKey: ['ottl-playground', config, settled.input, ws.runtime ?? ''],
    queryFn: ({ signal }) =>
      api.evaluateOTTL({ config_yaml: config!, processor: PROCESSOR, input: input.ok ? input.value : undefined, ...runtimeField(ws.runtime) }, signal),
    enabled: !!config,
    retry: false,
    placeholderData: keepPreviousData,
  });
  const ev = config ? evaluation.data : undefined;
  const results = statementResults(lines, ev?.valid ? ev : undefined);
  const stale = evaluation.isFetching || settled !== draft;
  const on = evaluatedOn(ev?.runtime, ws.runtime);

  const pick = (e: (typeof EXAMPLES)[number]) => {
    if (replacesWork(draft.input, draft.statements)) setAsking(e);
    else load(e);
  };
  const load = (e: (typeof EXAMPLES)[number]) => {
    setAsking(undefined);
    update({ input: exampleText(e), statements: e.statements });
  };
  const copy = async () => {
    const signal = checkInput(draft.input);
    if (!signal.ok) return;
    try {
      await navigator.clipboard.writeText(processorYAML(signal.signal, statementLines(draft.statements).map((l) => l.text), draft.errorMode));
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      setCopied(false);
    }
  };

  return (
    <>
      <PageHead
        title="OTTL playground"
        actions={
          <div className="pg-head-actions">
            <span className="od-muted">Example</span>
            <div className="od-seg" role="group" aria-label="Load an example">
              {EXAMPLES.map((e) => (
                <button key={e.signal} type="button" onClick={() => pick(e)}>{e.label}</button>
              ))}
            </div>
            <button type="button" className="od-btn" disabled={!checkInput(draft.input).ok || !statementLines(draft.statements).length} onClick={() => void copy()}
              title="Copy the transform processor with these statements, to paste under processors: in a configuration">
              <Icon name={copied ? 'circle-check' : 'file-code'} />
              <span>{copied ? 'Copied' : 'Copy processor'}</span>
            </button>
          </div>
        }
      />
      <div className="pg-page">
        {asking && (
          <div className="od-callout od-callout--warn pg-ask" role="alertdialog" aria-label="Replace with the example">
            <Icon name="triangle-alert" size={16} />
            <div className="od-callout__main">
              <span className="od-callout__title">Replace your input and statements with the {asking.label} example?</span>
              <div className="od-callout__body">What you wrote here is not kept anywhere else.</div>
            </div>
            <div className="od-callout__actions">
              <button type="button" className="od-btn od-btn--sm od-btn--primary" onClick={() => load(asking)}>Replace</button>
              <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={() => setAsking(undefined)}>Keep mine</button>
            </div>
          </div>
        )}
        <WorkspacePanes
          storageKey={PANES}
          defaults={PANE_DEFAULTS}
          upperLabel="Resize statements and result panes"
          left={
            <section className="od-pane pg-input" aria-label="Input">
              <div className="od-pane__head">
                <span className="od-pane__title">Input</span>
                <span className="od-muted">OTLP JSON{input.ok ? ` · ${input.signal}` : ''}</span>
              </div>
              <div className="editor-host">
                <YamlEditor path="playground-input.json" language={JSON_LANGUAGE} value={draft.input} onChange={(text) => update({ input: text })} />
              </div>
            </section>
          }
          graph={
            <section className="od-pane pg-statements" aria-label="Statements">
              <div className="od-pane__head">
                <span className="od-pane__title">Statements</span>
                <span className="od-muted pg-hint">one per line · # starts a comment</span>
                <span style={{ flex: 1 }} />
                <label className="pg-mode">
                  <span className="od-muted">error_mode</span>
                  <select className="od-select" value={draft.errorMode} onChange={(e) => update({ errorMode: e.target.value as ErrorMode })}>
                    {ERROR_MODES.map((m) => <option key={m} value={m}>{m}</option>)}
                  </select>
                </label>
              </div>
              <div className="editor-host">
                <YamlEditor path="playground-statements.ottl" language="plaintext" value={draft.statements} onChange={(text) => update({ statements: text })} />
              </div>
            </section>
          }
          bottom={
            <section className="od-pane pg-result" aria-label="Result" aria-busy={stale}>
              <div className="od-pane__head">
                <span className="od-pane__title">Result</span>
                <span className="od-muted pg-result__note">
                  {stale ? 'Evaluating…' : on.text}
                </span>
                {!stale && on.unverified && <span className="badge unverified" title={UNVERIFIED}>unverified</span>}
              </div>
              <div className="od-pane__body pg-result__body">
                <Result
                  inputProblem={input.ok ? undefined : input.message}
                  noStatements={!lines.length}
                  error={config ? evaluation.error?.message : undefined}
                  ev={ev}
                  results={results}
                  signal={input.ok ? input.signal : undefined}
                  before={input.ok ? input.value : undefined}
                />
              </div>
            </section>
          }
        />
      </div>
    </>
  );
}

/**
 * Result says what the statements did: one line on the input records, each
 * statement's counts keyed as the records' By column names them, and the
 * records themselves with their changed fields, as the debugger shows a
 * transform's Changes.
 */
function Result(p: {
  inputProblem?: string;
  noStatements: boolean;
  error?: string;
  ev?: OTTLEvaluation;
  results: ReturnType<typeof statementResults>;
  signal?: Signal;
  before: unknown;
}) {
  const ev = p.ev;
  const views = useMemo(() => (ev?.valid ? recordViews(ev.diff, p.before, ev.output, ev.replay) : undefined), [ev, p.before]);
  if (p.inputProblem) return <p className="od-muted">{p.inputProblem}</p>;
  if (p.noStatements) return <p className="od-muted">Write a statement, for example <code>set(log.attributes["seen"], true)</code>.</p>;
  if (p.error) return <p className="od-field__error" role="alert">{p.error}</p>;
  if (!ev) return null;
  if (!ev.valid) {
    return (
      <div role="alert">
        <p className="od-field__error">The statements do not compile:</p>
        <ul className="pg-diagnostics">
          {(ev.diagnostics ?? []).map((d, i) => <li key={i}>{d.message}</li>)}
        </ul>
      </div>
    );
  }
  const errors = p.results.reduce((n, r) => n + r.errors.length, 0);
  return (
    <>
      <div className="pg-summary">
        {p.signal && views && <span className="od-chip od-chip--plain">{recordSummary(p.signal, views.records.map((r) => r.state))}</span>}
        {errors > 0 && <span className="od-chip od-chip--fail">{errors} {errors === 1 ? 'error' : 'errors'}</span>}
      </div>
      {(ev.diagnostics ?? []).map((d, i) => <p key={i} className="note">{d.message}</p>)}
      {ev.replay?.error && <p className="od-field__error" role="alert">Processing stopped: {ev.replay.error}</p>}
      <table className="grid statements pg-table">
        <thead>
          <tr>
            <th>Statement</th>
            <th title="Where clause true (or no where clause)">Matched</th>
            <th title="Where clause false">Not matched</th>
            <th title="Matched, but changed nothing">Unchanged</th>
            <th>Changes</th>
            <th>Errors</th>
          </tr>
        </thead>
        <tbody>
          {p.results.map((r) => (
            <tr key={r.line}>
              <td>
                <span className="pg-key" title={`Line ${r.line} of the statements`}>{r.key}</span>
                <code>{r.text}</code>
                {r.errors.map((e, i) => <div key={i} className="error">{e.entity}: {e.message}</div>)}
              </td>
              <td>{r.matched}</td>
              <td>{r.notMatched}</td>
              <td>{r.unchanged}</td>
              <td>{r.changes}</td>
              <td className={r.errors.length ? 'error' : undefined}>{r.errors.length}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <RecordChanges diff={ev.diff} before={p.before} after={ev.output} report={ev.replay} />
    </>
  );
}
