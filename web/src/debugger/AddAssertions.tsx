import { useEffect, useMemo, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import type { Inspection } from '../api/types';
import type { RunView } from '../runs/useRun';
import type { Suggestion } from '../scenario/checks';
import { Icon } from '../ui/Icon';
import { getWorkspace, updateWorkspace } from '../workspace/store';
import { useReplaceDrafts } from '../workspace/useReplaceDrafts';
import { runLabel } from './runFacts';
import {
  baseYaml,
  destinationText,
  existingRows,
  newSuggestions,
  suggestionEvidence,
  suggestionText,
  testBase,
  testDraftPatch,
  withConfirmed,
} from './testDraft';

interface Props {
  runId: string;
  view: RunView;
  inspection?: Inspection;
  onClose(): void;
  /** The assertions are in the open scenario: go and review or run it. */
  onUsed(): void;
}

/**
 * AddAssertions adds assertions from what a finished run observed to the
 * scenario the run used (or, for a run without one, to a new scenario made
 * from the run), which becomes the open scenario. A suggestion is only a
 * proposal; it is added when confirmed. Nothing is saved or exported here:
 * Save does that (docs/glossary.md).
 */
export function AddAssertions({ runId, view, inspection, onClose, onUsed }: Props) {
  const ref = useRef<HTMLElement>(null);
  const inputs = useQuery({ queryKey: ['run-inputs', runId], queryFn: () => api.runInputs(runId), staleTime: Infinity, retry: false });
  const derived = useQuery({ queryKey: ['scenario-from-run', runId], queryFn: () => api.scenarioFromRun(runId), staleTime: Infinity, retry: false });
  const [confirmed, setConfirmed] = useState<string[]>([]);
  const [discarded, setDiscarded] = useState<string[]>([]);
  const replacer = useReplaceDrafts({ after: onUsed });

  useEffect(() => {
    ref.current?.focus();
    const key = (e: KeyboardEvent) => e.key === 'Escape' && onClose();
    document.addEventListener('keydown', key);
    return () => document.removeEventListener('keydown', key);
  }, [onClose]);

  const base = testBase(inputs.data);
  const yaml = baseYaml(base, inputs.data, derived.data);
  const report = view.run?.report;
  const topology = report?.topology ?? inspection?.topology;
  const suggestions = useMemo(() => (yaml ? newSuggestions(report, topology, yaml) : []), [yaml, report, topology]);
  const kept = suggestions.filter((s) => confirmed.includes(s.key));
  const open = suggestions.filter((s) => !confirmed.includes(s.key) && !discarded.includes(s.key));
  const working = useMemo(() => (yaml ? withConfirmed(yaml, kept) : undefined), [yaml, kept]);
  const text = working && 'yaml' in working ? working.yaml : undefined;
  const existing = useMemo(() => (yaml ? existingRows(yaml) : []), [yaml]);

  // The confirmed assertions are checked against the recorded run, without running again.
  const checked = useQuery({
    queryKey: ['turn-into-test-check', runId, text],
    queryFn: () => api.evaluateAssertions(runId, { scenario_yaml: text! }),
    enabled: !!text && existing.length + kept.length > 0,
    staleTime: Infinity,
    retry: false,
  });

  const toggle = (list: string[], set: (v: string[]) => void, key: string, on: boolean) =>
    set(on ? [...list.filter((k) => k !== key), key] : list.filter((k) => k !== key));
  const confirm = (s: Suggestion) => {
    toggle(discarded, setDiscarded, s.key, false);
    toggle(confirmed, setConfirmed, s.key, true);
  };
  const discard = (s: Suggestion) => {
    toggle(confirmed, setConfirmed, s.key, false);
    toggle(discarded, setDiscarded, s.key, true);
  };

  const start = () => {
    if (!inputs.data || text === undefined) return;
    const data = inputs.data;
    replacer.request(['scenario'], `The scenario from run ${runLabel(runId)}`, () => {
      const { patch } = testDraftPatch(getWorkspace(), base, text, data, derived.data?.required_environment);
      updateWorkspace(patch);
    });
  };

  const loading = inputs.isPending || (base === 'new' && derived.isPending);
  const problem = inputs.error?.message ?? (base === 'new' ? derived.error?.message : undefined) ?? (working && 'error' in working ? working.error : undefined);
  const outcome = checked.data;
  const total = existing.length + kept.length;
  return (
    <aside ref={ref} tabIndex={-1} className="od-drawer" aria-label="Add assertions" style={{ top: 48, width: 520 }}>
      <div className="od-drawer__head">
        <span className="t-heading">Add assertions from run {runLabel(runId)}</span>
        <span style={{ flex: 1 }} />
        <button type="button" className="od-btn od-btn--ghost od-btn--icon" aria-label="Close" onClick={onClose}>
          <Icon name="x" />
        </button>
      </div>
      <div style={{ flex: 1, overflow: 'auto', padding: '14px 16px', display: 'flex', flexDirection: 'column', gap: 14 }}>
        {loading && <p className="od-muted" role="status">Reading the run…</p>}
        {problem && <div className="od-callout od-callout--fail" role="alert"><Icon name="circle-x" /><div className="od-callout__main"><div className="od-callout__body">{problem}</div></div></div>}
        {inputs.data && !problem && <p className="od-muted" style={{ fontSize: 12.5, margin: 0 }}>{destinationText(base, inputs.data)}</p>}

        <div style={{ display: 'flex', alignItems: 'baseline', gap: 8 }}>
          <span className="od-section-label">Assertions</span>
          <span style={{ flex: 1 }} />
          <span className="od-subtle" style={{ fontSize: 12 }}>{existing.length + kept.length} kept · {open.length} suggested</span>
        </div>
        {existing.map((r) => (
          <div key={r.key} className={`od-assert od-assert--${outcomeClass(outcome?.results?.[Number(r.key.split(':')[1])]?.outcome)}`}>
            <Icon name={outcomeIcon(outcome?.results?.[Number(r.key.split(':')[1])]?.outcome)} size={18} />
            <div>
              <div className="od-assert__text">{r.text}</div>
              <div className="od-assert__evidence">Existing</div>
            </div>
          </div>
        ))}
        {kept.map((s) => (
          <div key={s.key} className="od-assert od-assert--unchecked">
            <Icon name="check" size={18} />
            <div>
              <div className="od-assert__text">{suggestionText(s)}</div>
              <div className="od-assert__evidence">Confirmed · {suggestionEvidence(s)}</div>
            </div>
            <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={() => discard(s)}>Undo</button>
          </div>
        ))}
        {open.length > 0 && (
          <div className="od-muted" style={{ fontSize: 12, display: 'flex', gap: 6 }}>
            <Icon name="info" size={14} />
            <span>Suggested from what run {runLabel(runId)} observed. Only the ones you confirm are added.</span>
          </div>
        )}
        {open.map((s) => (
          <div key={s.key} className="od-assert od-assert--suggested">
            <Icon name="circle-dashed" size={18} />
            <div>
              <div className="od-assert__text">{suggestionText(s)}</div>
              <div className="od-assert__evidence">{suggestionEvidence(s)}</div>
            </div>
            <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
              <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={() => discard(s)}>Discard</button>
              <button type="button" className="od-btn od-btn--sm" onClick={() => confirm(s)}>
                <Icon name="check" size={14} />
                <span>Confirm</span>
              </button>
            </div>
          </div>
        ))}
        {!loading && !existing.length && !suggestions.length && (
          <p className="od-muted" style={{ fontSize: 12.5 }}>
            The run recorded nothing to suggest. Open the scenario and write assertions in its Assertions tab.
          </p>
        )}
      </div>
      <div className="od-drawer__foot" style={{ flexDirection: 'column', alignItems: 'stretch', gap: 8 }}>
        {total > 0 && (
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 12.5 }} role="status">
            {checked.isPending ? (
              <span className="od-muted">Checking against run {runLabel(runId)}…</span>
            ) : outcome ? (
              <>
                <Icon name={outcome.outcome === 'pass' ? 'circle-check' : 'triangle-alert'} size={14} />
                <span>
                  Checked against run {runLabel(runId)} (recorded, not rerun): {outcome.outcome === 'pass' ? `all ${total} assertions pass.` : `the assertions ${outcome.outcome}.`}
                </span>
              </>
            ) : checked.error ? (
              <span className="od-muted">Could not check against the run: {checked.error.message}</span>
            ) : null}
          </div>
        )}
        {replacer.asking ? (
          replacer.confirm
        ) : (
          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
            <button type="button" className="od-btn od-btn--primary" disabled={text === undefined || !!problem} onClick={start}>
              <Icon name={kept.length ? 'plus' : 'pencil'} />
              <span>{kept.length ? `Add ${kept.length} assertion${kept.length === 1 ? '' : 's'}` : 'Open the scenario'}</span>
            </button>
            <span className="od-subtle" style={{ fontSize: 12 }}>Nothing is saved: use Save when you are ready.</span>
          </div>
        )}
        {open.length > 0 && <div className="od-subtle" style={{ fontSize: 11.5 }}>{open.length} suggestion{open.length === 1 ? '' : 's'} not confirmed will be discarded.</div>}
      </div>
    </aside>
  );
}

function outcomeClass(outcome: string | undefined): string {
  return outcome === 'pass' ? 'pass' : outcome === 'fail' ? 'fail' : outcome === 'inconclusive' ? 'inconclusive' : 'unchecked';
}

function outcomeIcon(outcome: string | undefined) {
  return outcome === 'pass' ? ('circle-check' as const) : outcome === 'fail' ? ('circle-x' as const) : outcome === 'inconclusive' ? ('triangle-alert' as const) : ('circle-dashed' as const);
}
