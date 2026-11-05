import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import type { AssertionReport, Diagnostic, Graph } from '../api/types';
import type { WorkspaceState } from '../workspace/store';
import {
  appendAssertion,
  ASSERTION_TEMPLATES,
  ATTRIBUTE_TYPES,
  attributeKeys,
  checkProblem,
  conflicts,
  describeAssertion,
  NEW_CHECKS,
  readChecks,
  sameCheck,
  setChecks,
  suggestFromRun,
  templateTarget,
  VALUE_TYPES,
  type AssertionTemplate,
  type AttrRule,
  type BoundOp,
  type Check,
  type Suggestion,
} from './checks';
import { Menu, type MenuItem } from './fields';
import {
  duplicateItem,
  moveItem,
  removeItem,
  setConditions,
  setField,
  setTarget,
  targetChoices,
  targetKey,
  type AssertionModel,
  type Edit,
  type TargetChoice,
  type TargetModel,
} from './scenarioDoc';
import { useDocEdit } from './useDocEdit';

type Result = NonNullable<AssertionReport['results']>[number];

interface Props {
  ws: WorkspaceState;
  topology?: Graph;
  /** The selected run: assertions are checked against it and suggested from it. */
  runId?: string;
  /** The inputs' payloads, for attribute key suggestions. */
  payloads: unknown[];
  /** Server validation problems in the assertions, which keep the scenario from applying. */
  diagnostics: Diagnostic[];
}

/**
 * AssertionBuilder edits what must hold for the scenario to pass: each
 * assertion is a target and rows of checks, started from a template or from
 * what the last run observed. The YAML stays the source of truth and remains
 * the editor for shapes the rows cannot show.
 */
export function AssertionBuilder({ ws, topology, runId, payloads, diagnostics }: Props) {
  const { read, apply, problem } = useDocEdit(ws.scenarioYAML);
  const check = useMutation({
    mutationFn: (v: { runId: string; yaml: string }) => api.evaluateAssertions(v.runId, { scenario_yaml: v.yaml }),
  });
  // The same query useRun follows, so the selected run is fetched once.
  const { data: run } = useQuery({ queryKey: ['run', runId], queryFn: () => api.run(runId!), enabled: !!runId });
  const report = run?.state === 'done' ? run.report : undefined;
  const choices = useMemo(() => targetChoices(topology, report), [topology, report]);
  const keys = useMemo(() => attributeKeys(payloads), [payloads]);
  // Rows a template added that still need a key: written once complete.
  const [pending, setPending] = useState<{ index: number; checks: Check[] }>();
  const [suggesting, setSuggesting] = useState(false);

  if ('error' in read)
    return (
      <div className="scenario-form">
        <p className="error">The scenario YAML cannot be read: {read.error}. Fix it in the YAML tab.</p>
      </div>
    );
  const assertions = read.model.assertions;
  const checked = check.data && check.variables?.yaml === ws.scenarioYAML ? check.data : undefined;
  const suggestions = suggestFromRun(report, choices, assertions);

  const fromTemplate = (t: AssertionTemplate) => {
    const target = templateTarget(t, choices) ?? (t.needs === 'exporter' ? { exporter: '', signal: 'logs' } : { processor: '', signal: 'logs' });
    const ready = t.checks.filter((c) => !checkProblem(c, target));
    const later = t.checks.filter((c) => checkProblem(c, target));
    if (apply(appendAssertion(target, ready))) setPending(later.length ? { index: assertions.length, checks: later } : undefined);
  };
  const empty = () => {
    const target = choices[0]?.target ?? { exporter: '', signal: 'logs' };
    if (apply(appendAssertion(target, []))) setPending({ index: assertions.length, checks: [] });
  };
  const addItems: MenuItem[] = [
    ...ASSERTION_TEMPLATES.map((t, i) => ({
      group: i === 0 ? 'Templates' : undefined,
      label: t.label,
      detail: t.detail,
      disabled: !templateTarget(t, choices),
      onSelect: () => fromTemplate(t),
    })),
    {
      group: 'Or',
      label: 'Suggest from last run…',
      detail: report ? 'Counts and drops the selected run observed' : 'Run the scenario or an input first',
      disabled: !report,
      onSelect: () => setSuggesting(true),
    },
    { label: 'Empty assertion', detail: 'Pick a target and add checks', onSelect: empty },
  ];

  return (
    <div className="scenario-form assertion-builder">
      <datalist id="attribute-keys">
        {keys.attributes.map((k) => (
          <option key={k} value={k} />
        ))}
      </datalist>
      <datalist id="resource-keys">
        {keys.resource_attributes.map((k) => (
          <option key={k} value={k} />
        ))}
      </datalist>
      <div className="builder-bar">
        <h3>Assertions</h3>
        {checked ? (
          <span className={`chip outcome-${checked.outcome}`} title={`Checked against run ${check.variables!.runId.slice(0, 8)}`}>
            {checked.outcome}
          </span>
        ) : (
          assertions.length > 0 && <span className="tab-count plain">{assertions.length}</span>
        )}
        <span className="grow" />
        <button
          type="button"
          disabled={!runId || !assertions.length || check.isPending}
          title={runId ? "Evaluate these assertions against the selected run's recorded telemetry, without running again" : 'Run first'}
          onClick={() => runId && check.mutate({ runId, yaml: ws.scenarioYAML })}
        >
          Check against run
        </button>
        <Menu label="+ Assertion ▾" className="primary-outline" items={addItems} align="right" />
      </div>
      {problem && <p className="error">{problem}</p>}
      {diagnostics.map((d, i) => (
        <p key={i} className="warn-text">
          {d.message.replace(/^scenario not applied: (invalid request: )?(scenario_yaml: )?/, '')}
        </p>
      ))}
      {check.error && <p className="error">{check.error.message}</p>}
      {suggesting && report && (
        <SuggestPanel
          suggestions={suggestions}
          onAdd={(picked) => {
            apply((doc) => picked.forEach((s) => appendAssertion(s.target, s.checks)(doc)));
            setSuggesting(false);
          }}
          onClose={() => setSuggesting(false)}
        />
      )}
      {!assertions.length && !suggesting && (
        <div className="start-here">
          <p className="muted">
            Assertions turn runs into tests: each says what must hold at an exporter or a processor. Without any, a run shows what happened but cannot pass or
            fail.
          </p>
          <div className="template-grid">
            {ASSERTION_TEMPLATES.map((t) => (
              <button key={t.id} type="button" className="template-card" disabled={!templateTarget(t, choices)} onClick={() => fromTemplate(t)}>
                <strong>{t.label}</strong>
                <span className="muted small">{templateTarget(t, choices) ? t.detail : `The configuration has no ${t.needs} to target`}</span>
              </button>
            ))}
            <button type="button" className="template-card" disabled={!report} onClick={() => setSuggesting(true)}>
              <strong>Suggest from last run…</strong>
              <span className="muted small">{report ? 'Assert the counts and drops the selected run observed' : 'Run the scenario or an input first'}</span>
            </button>
          </div>
        </div>
      )}
      {assertions.map((a, i) => (
        <AssertionCard
          key={i}
          index={i}
          count={assertions.length}
          assertion={a}
          choices={choices}
          apply={apply}
          result={checked?.results?.[i]}
          initialPending={pending?.index === i ? pending.checks : undefined}
          onPendingTaken={() => setPending(undefined)}
        />
      ))}
      <p className="muted small">Changes here edit the YAML tab, which can also express everything by hand.</p>
    </div>
  );
}

function SuggestPanel({ suggestions, onAdd, onClose }: { suggestions: Suggestion[]; onAdd(s: Suggestion[]): void; onClose(): void }) {
  const [picked, setPicked] = useState(() => new Set(suggestions.filter((s) => !s.present).map((s) => s.key)));
  const chosen = suggestions.filter((s) => picked.has(s.key));
  return (
    <div className="suggest-panel" role="dialog" aria-label="Suggest from last run">
      <strong>From the selected run</strong>
      {suggestions.length ? (
        <ul>
          {suggestions.map((s) => (
            <li key={s.key}>
              <label className="inline">
                <input
                  type="checkbox"
                  checked={picked.has(s.key)}
                  onChange={(e) => {
                    const next = new Set(picked);
                    if (e.target.checked) next.add(s.key);
                    else next.delete(s.key);
                    setPicked(next);
                  }}
                />
                {describeAssertion(s.target, s.checks)}
                {s.present && <span className="muted small"> · already asserted</span>}
              </label>
            </li>
          ))}
        </ul>
      ) : (
        <p className="muted">The run captured nothing at this configuration's exporters and no processor dropped records.</p>
      )}
      <div className="form-actions">
        <button type="button" className="primary" disabled={!chosen.length} onClick={() => onAdd(chosen)}>
          Add {chosen.length === 1 ? 'assertion' : `${chosen.length} assertions`}
        </button>
        <button type="button" onClick={onClose}>
          Cancel
        </button>
      </div>
    </div>
  );
}

function AssertionCard({
  index: i,
  count,
  assertion: a,
  choices,
  apply,
  result,
  initialPending,
  onPendingTaken,
}: {
  index: number;
  count: number;
  assertion: AssertionModel;
  choices: TargetChoice[];
  apply(e: Edit): boolean;
  result?: Result;
  initialPending?: Check[];
  onPendingTaken(): void;
}) {
  useEffect(() => {
    if (initialPending) onPendingTaken();
    // Taken once, when the card mounts.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const read = readChecks(a.conditions);
  const [yaml, setYaml] = useState(false);
  const [pending, setPending] = useState<Check[]>(initialPending ?? []);
  const rows = 'checks' in read ? read.checks : [];
  const all = [...rows, ...pending];
  const clash = conflicts(all);
  const commit = (checks: Check[]) => apply(setChecks(i, checks));
  const tkey = targetKey(a.target);
  const known = choices.some((c) => targetKey(c.target) === tkey);
  const menu: MenuItem[] = [
    { label: yaml ? 'Edit as rows' : 'Edit as YAML', disabled: !('checks' in read) && yaml, onSelect: () => setYaml(!yaml) },
    { label: 'Duplicate', onSelect: () => apply(duplicateItem('assertions', i)) },
    { label: 'Move up', disabled: i === 0, onSelect: () => apply(moveItem('assertions', i, i - 1)) },
    { label: 'Move down', disabled: i === count - 1, onSelect: () => apply(moveItem('assertions', i, i + 1)) },
    { label: 'Remove', onSelect: () => apply(removeItem('assertions', i)) },
  ];
  const addCheck: MenuItem[] = NEW_CHECKS.filter((n) => !n.processorOnly || a.target.processor !== undefined).map((n) => ({
    label: n.label,
    detail: n.detail,
    onSelect: () => (checkProblem(n.check, a.target) ? setPending([...pending, n.check]) : commit([...rows, n.check])),
  }));
  const failed = (result?.checks ?? []).filter((c) => c.outcome !== 'pass');
  return (
    <div className={`assertion-card${result ? ` outcome-${result.outcome}` : ''}`}>
      <div className="form-line">
        {result && <span className="badge">{result.outcome}</span>}
        <NameInput
          value={a.name}
          placeholder={'checks' in read ? describeAssertion(a.target, rows) : `assertions[${i}]`}
          onCommit={(v) => apply(setField(['assertions', i, 'name'], v))}
        />
        <Menu label="⋯" items={menu} align="right" title="More actions" />
      </div>
      <div className="form-line">
        <span className="muted small">At</span>
        <select
          className="grow"
          value={tkey}
          aria-label="Target"
          onChange={(e) => {
            const c = choices.find((x) => targetKey(x.target) === e.target.value);
            if (c) apply(setTarget(i, c.target));
          }}
        >
          {!known && <option value={tkey}>{targetLabel(a.target)} (not in the configuration)</option>}
          <optgroup label="Exporters: what leaves the Collector">
            {choices
              .filter((c) => c.target.exporter)
              .map((c) => (
                <option key={targetKey(c.target)} value={targetKey(c.target)}>
                  {c.label}
                </option>
              ))}
          </optgroup>
          <optgroup label="Processors: what one stage emits">
            {choices
              .filter((c) => c.target.processor)
              .map((c) => (
                <option key={targetKey(c.target)} value={targetKey(c.target)}>
                  {c.label}
                </option>
              ))}
          </optgroup>
          {choices.some((c) => c.target.endpoint) && (
            <optgroup label="Simulated endpoints: what a component sent">
              {choices
                .filter((c) => c.target.endpoint)
                .map((c) => (
                  <option key={targetKey(c.target)} value={targetKey(c.target)}>
                    {c.label}
                  </option>
                ))}
            </optgroup>
          )}
        </select>
      </div>
      {yaml || !('checks' in read) ? (
        <>
          {'unsupported' in read && <p className="muted small">{read.unsupported} Edit it here or in the YAML tab.</p>}
          <ConditionsEditor key={a.conditions} index={i} text={a.conditions} apply={apply} />
        </>
      ) : (
        <div className="check-rows">
          {all.map((c, j) => (
            <CheckRow
              key={j}
              check={c}
              target={a.target}
              conflict={clash.get(j)}
              autoFocus={j >= rows.length}
              onChange={(next) => {
                if (j < rows.length) commit(rows.map((r, k) => (k === j ? next : r)));
                else {
                  commit([...rows, next]);
                  setPending(pending.filter((_, k) => k !== j - rows.length));
                }
              }}
              onRemove={() => (j < rows.length ? commit(rows.filter((_, k) => k !== j)) : setPending(pending.filter((_, k) => k !== j - rows.length)))}
            />
          ))}
          {!all.length && <p className="warn-text small">No checks yet: an assertion needs at least one.</p>}
          <Menu label="+ check" items={addCheck} className="add-check" />
        </div>
      )}
      {failed.map((c, j) => (
        <div key={j} className="check">
          {c.check}
          {c.key ? ` ${c.key}` : ''}: expected {c.expected}
          {c.observed !== undefined ? `, observed ${c.observed}` : ''}
          {c.message ? ` — ${c.message}` : ''}
        </div>
      ))}
    </div>
  );
}

function targetLabel(t: TargetModel): string {
  if (t.exporter !== undefined) return `exporter ${t.exporter} (${t.signal})`;
  if (t.endpoint) return `endpoint ${t.endpoint.path} of ${t.endpoint.component} (${t.signal})`;
  return `processor ${t.processor ?? ''}${t.pipeline ? ` in ${t.pipeline}` : ''}`;
}

function NameInput({ value, placeholder, onCommit }: { value: string; placeholder: string; onCommit(v: string): void }) {
  const [draft, setDraft] = useState(value);
  useEffect(() => setDraft(value), [value]);
  return (
    <input
      className="grow assertion-name"
      value={draft}
      placeholder={placeholder}
      aria-label="Assertion name"
      title="The name in test reports; empty uses the sentence shown"
      onChange={(e) => setDraft(e.target.value)}
      onKeyDown={(e) => e.key === 'Enter' && e.currentTarget.blur()}
      onBlur={() => draft !== value && onCommit(draft)}
    />
  );
}

const OPS: { op: BoundOp; label: string }[] = [
  { op: 'equals', label: 'exactly' },
  { op: 'min', label: 'at least' },
  { op: 'max', label: 'at most' },
  { op: 'between', label: 'between' },
];
const RULES: { rule: AttrRule; label: string }[] = [
  { rule: 'present', label: 'is set' },
  { rule: 'absent', label: 'is removed' },
  { rule: 'equals', label: 'equals' },
  { rule: 'types', label: 'has type' },
];

/**
 * CheckRow edits one check as a sentence. It writes a change once the row
 * is complete; an incomplete row says what is missing and stays local.
 */
function CheckRow({
  check,
  target,
  conflict,
  autoFocus,
  onChange,
  onRemove,
}: {
  check: Check;
  target: TargetModel;
  conflict?: string;
  autoFocus?: boolean;
  onChange(c: Check): void;
  onRemove(): void;
}) {
  const [draft, setDraft] = useState(check);
  useEffect(() => {
    if (!sameCheck(draft, check)) setDraft(check);
    // Only a change from outside resets the draft.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [check]);
  const problem = checkProblem(draft, target);
  const set = (c: Check) => {
    setDraft(c);
    if (!checkProblem(c, target) && !sameCheck(c, check)) onChange(c);
  };
  const num = (v: string) => (v === '' ? NaN : Number(v));
  return (
    <div className={`check-row${problem ? ' incomplete' : ''}`}>
      {draft.kind !== 'attribute' ? (
        <>
          <span className="check-subject">{draft.kind === 'count' ? 'Records reaching it' : 'Records it drops'}</span>
          <select value={draft.bound.op} aria-label="Bound" onChange={(e) => set({ ...draft, bound: { ...draft.bound, op: e.target.value as BoundOp } })}>
            {OPS.map((o) => (
              <option key={o.op} value={o.op}>
                {o.label}
              </option>
            ))}
          </select>
          <input
            type="number"
            min={0}
            className="short"
            aria-label="Number"
            value={Number.isNaN(draft.bound.n) ? '' : draft.bound.n}
            onChange={(e) => set({ ...draft, bound: { ...draft.bound, n: num(e.target.value) } })}
          />
          {draft.bound.op === 'between' && (
            <>
              <span className="muted">and</span>
              <input
                type="number"
                min={0}
                className="short"
                aria-label="Upper number"
                value={draft.bound.m === undefined || Number.isNaN(draft.bound.m) ? '' : draft.bound.m}
                onChange={(e) => set({ ...draft, bound: { ...draft.bound, m: num(e.target.value) } })}
              />
            </>
          )}
        </>
      ) : (
        <>
          <select
            className="check-subject"
            value={draft.scope}
            aria-label="Attribute level"
            onChange={(e) => set({ ...draft, scope: e.target.value as typeof draft.scope })}
          >
            <option value="attributes">Attribute</option>
            <option value="resource_attributes">Resource attribute</option>
          </select>
          <input
            className="mono key"
            value={draft.key}
            placeholder="key, e.g. user.email"
            aria-label="Attribute key"
            list={draft.scope === 'attributes' ? 'attribute-keys' : 'resource-keys'}
            autoFocus={autoFocus && !draft.key}
            spellCheck={false}
            onChange={(e) => set({ ...draft, key: e.target.value })}
          />
          <select
            value={draft.rule}
            aria-label="Condition"
            onChange={(e) => {
              const rule = e.target.value as AttrRule;
              set({
                ...draft,
                rule,
                value: rule === 'types' ? 'string' : rule === 'equals' ? '' : undefined,
                valueType: rule === 'equals' ? 'string' : undefined,
              });
            }}
          >
            {RULES.map((r) => (
              <option key={r.rule} value={r.rule}>
                {r.label}
              </option>
            ))}
          </select>
          {draft.rule === 'equals' &&
            (draft.valueType === 'bool' ? (
              <select value={draft.value} aria-label="Value" onChange={(e) => set({ ...draft, value: e.target.value })}>
                <option value="">choose…</option>
                <option value="true">true</option>
                <option value="false">false</option>
              </select>
            ) : (
              <input
                className="mono value"
                value={draft.value ?? ''}
                placeholder="value"
                aria-label="Value"
                spellCheck={false}
                onChange={(e) => set({ ...draft, value: e.target.value })}
              />
            ))}
          {draft.rule === 'equals' && (
            <select
              value={draft.valueType}
              aria-label="Value type"
              title='The type must match too: 200 does not equal "200"'
              onChange={(e) => set({ ...draft, valueType: e.target.value as typeof draft.valueType, value: e.target.value === 'bool' ? '' : draft.value })}
            >
              {VALUE_TYPES.map((t) => (
                <option key={t} value={t}>
                  {t}
                </option>
              ))}
            </select>
          )}
          {draft.rule === 'types' && (
            <select value={draft.value} aria-label="Type" onChange={(e) => set({ ...draft, value: e.target.value })}>
              {ATTRIBUTE_TYPES.map((t) => (
                <option key={t} value={t}>
                  {t}
                </option>
              ))}
            </select>
          )}
        </>
      )}
      <button type="button" className="icon" aria-label="Remove check" onClick={onRemove}>
        ✕
      </button>
      {(problem || conflict) && <span className="check-note small">{problem ?? conflict}</span>}
    </div>
  );
}

/** ConditionsEditor edits the conditions as YAML; it commits on blur. */
function ConditionsEditor({ index, text, apply }: { index: number; text: string; apply(e: Edit): boolean }) {
  const [draft, setDraft] = useState(text);
  const [error, setError] = useState<string>();
  return (
    <>
      <textarea
        className="mono conditions"
        rows={Math.max(3, draft.split('\n').length)}
        value={draft}
        aria-label={`Conditions of assertion ${index}`}
        spellCheck={false}
        placeholder={'count: {equals: 1}\nattributes:\n  absent: [user.email]'}
        onChange={(e) => setDraft(e.target.value)}
        onBlur={() => {
          if (draft === text) return;
          setError(apply(setConditions(index, draft)) ? undefined : 'Not applied: see the message above.');
        }}
      />
      {error && <p className="error">{error}</p>}
    </>
  );
}
