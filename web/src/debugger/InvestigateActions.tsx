import { useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';
import { api } from '../api/client';
import type { Run } from '../api/types';
import { canDelete, DELETE_WARNING } from '../runs/deletion';
import { compareCandidates, finished } from '../runs/compare';
import { configComparisonRequest, type CompareTarget } from '../runs/configCompare';
import { runListQuery } from '../runs/runList';
import { firstPageRuns } from '../runs/runPages';
import { useDeleteRun } from '../runs/useDeleteRun';
import { Icon } from '../ui/Icon';
import { newLabel, runDraft, runExportDraft, runExportNeedsScenario } from '../workspace/drafts';
import { getWorkspace, updateWorkspace, useWorkspace } from '../workspace/store';
import { useReplaceDrafts } from '../workspace/useReplaceDrafts';
import { ExportForCI } from './ExportForCI';
import { runLabel, whenText } from './runFacts';
import { useDismiss } from './useDismiss';
import { useDenial } from '../ui/session';

/** How many earlier runs "Compare with…" lists before pointing to the full list. */
const PICKER_RUNS = 8;

/** How many saved configurations "Compare with…" offers to run the input with. */
const PICKER_CONFIGS = 5;

/**
 * CompareMenu picks the finished run the shown run is compared with
 * (`compare` in the URL); the full list stays one step away. It can also run
 * the run's input with another configuration, the open one or a saved one,
 * beside the run's own configuration (one comparison, `POST /comparisons`)
 * and open the comparison of those two new runs.
 */
export function CompareMenu({ runId, run, onPick }: { runId: string; run?: Run; onPick(id: string): void }) {
  const [open, setOpen] = useState(false);
  const host = useRef<HTMLDivElement>(null);
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const ws = useWorkspace();
  useDismiss(open, host, () => setOpen(false));
  const runs = useQuery({ ...runListQuery, enabled: open });
  const candidates = compareCandidates(firstPageRuns(runs.data), runId).slice(0, PICKER_RUNS);
  const ended = !!run && finished(run);
  const configs = useQuery({ queryKey: ['artifacts', 'configuration'], queryFn: () => api.artifacts('configuration'), enabled: open && ended, retry: false });
  const runDenied = useDenial('run.submit');
  const submit = useMutation({
    mutationFn: async (target: CompareTarget) => {
      const [inputs, scenario] = await Promise.all([api.runInputs(runId), api.scenarioFromRun(runId)]);
      return api.submitComparison(configComparisonRequest(run!, inputs, scenario, target));
    },
    onSuccess: (pair) => {
      void queryClient.invalidateQueries({ queryKey: ['runs'] });
      setOpen(false);
      void navigate({ to: '/', search: { run: pair.after.id, compare: pair.before.id } });
    },
  });
  return (
    <div ref={host} style={{ position: 'relative' }}>
      <button type="button" className="od-btn" aria-haspopup="menu" aria-expanded={open} onClick={() => setOpen(!open)}>
        <Icon name="git-compare" />
        <span>Compare with…</span>
      </button>
      {open && (
        <div className="od-menu od-wbhead__menu" style={{ top: 'calc(100% + 4px)', right: 0, width: 320 }} role="menu" aria-label="Compare with">
          <div className="od-menu__label">Compare run {runLabel(runId)} with</div>
          {runs.isPending && <div className="od-muted" style={{ padding: '6px 8px', fontSize: 12.5 }}>Loading runs…</div>}
          {runs.error && <div className="od-muted" style={{ padding: '6px 8px', fontSize: 12.5 }} role="alert">Runs unavailable: {runs.error.message}</div>}
          {runs.data && !candidates.length && <div className="od-muted" style={{ padding: '6px 8px', fontSize: 12.5 }}>No other finished run to compare with.</div>}
          {candidates.map((r) => (
            <button key={r.id} type="button" role="menuitem" className="od-menu__item" onClick={() => { setOpen(false); onPick(r.id); }}>
              <code style={{ fontSize: 12 }}>{runLabel(r.id)}</code>
              <span style={{ flex: 1 }} />
              <span className="od-subtle" style={{ fontSize: 12 }}>{whenText(r.created)} · {r.state}</span>
            </button>
          ))}
          <div className="od-menu__sep" />
          <button type="button" role="menuitem" className="od-menu__item" onClick={() => { setOpen(false); void navigate({ to: '/runs' }); }}>
            <span style={{ flex: 1 }}>All runs…</span>
            <Icon name="arrow-right" size={14} />
          </button>
          {ended && (
            <>
              <div className="od-menu__sep" />
              <div className="od-menu__label">Or run its input with another configuration</div>
              {ws.configYAML.trim() && (
                <button type="button" role="menuitem" className="od-menu__item" disabled={submit.isPending || !!runDenied}
                  title={runDenied ?? "Run this run's input with its own configuration and with your open configuration, then compare"}
                  onClick={() => submit.mutate({ kind: 'draft', yaml: ws.configYAML, label: ws.configPath ? newLabel(ws, 'configuration') : undefined })}>
                  <Icon name="file-code" />
                  <span style={{ flex: 1 }}>My open configuration</span>
                </button>
              )}
              {(configs.data?.items ?? []).slice(0, PICKER_CONFIGS).map((a) => (
                <button key={a.id} type="button" role="menuitem" className="od-menu__item" disabled={submit.isPending || !!runDenied}
                  title={runDenied ?? "Run this run's input with its own configuration and with this saved configuration's newest revision, then compare"}
                  onClick={() => submit.mutate({ kind: 'saved', artifact: a })}>
                  <Icon name="file-code" />
                  <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{a.label}</span>
                  <span className="od-subtle" style={{ fontSize: 12 }}>saved</span>
                </button>
              ))}
              {submit.isPending && <div className="od-muted" style={{ padding: '6px 8px', fontSize: 12.5 }} role="status">Starting both runs…</div>}
              {submit.error && <div className="od-field__error" style={{ padding: '6px 8px' }} role="alert">{submit.error.message}</div>}
            </>
          )}
        </div>
      )}
    </div>
  );
}

type Step = 'menu' | 'export' | 'open' | 'delete';

/**
 * MoreMenu holds the run's less frequent actions: Export for CI, open its
 * configuration and scenario, and delete it. Export for CI opens the run's
 * configuration and a scenario CI can run on its own (`runExportDraft`),
 * then shows the same export as the Save menu. Each action that replaces
 * unsaved work or loses something asks inline first.
 */
export function MoreMenu({ run, onDeleted }: { run: Run; onDeleted(): void }) {
  const [open, setOpen] = useState(false);
  const [step, setStep] = useState<Step>('menu');
  const replacer = useReplaceDrafts({ compact: true });
  const [notes, setNotes] = useState<string[]>();
  const host = useRef<HTMLDivElement>(null);
  const close = () => {
    setOpen(false);
    setStep('menu');
    replacer.cancel();
  };
  useDismiss(open, host, close);
  const deleteDenied = useDenial('run.delete');
  const del = useDeleteRun(() => {
    close();
    onDeleted();
  });
  const ws = useWorkspace();
  const inputs = useMutation({ mutationFn: () => api.runInputs(run.id) });
  const derive = useMutation({ mutationFn: () => api.scenarioFromRun(run.id) });

  const exportForCI = () =>
    inputs.mutate(undefined, {
      onSuccess: async (data) => {
        let derived;
        try {
          derived = runExportNeedsScenario(data) ? await derive.mutateAsync() : undefined;
        } catch {
          return;
        }
        replacer.request(runExportDraft(getWorkspace(), data, derived).drafts, `Run ${runLabel(run.id)}’s configuration and scenario`, () => {
          const d = runExportDraft(getWorkspace(), data, derived);
          updateWorkspace(d.patch);
          setNotes(d.notes);
          setStep('export');
        });
      },
    });

  // The draft is computed again when it is applied, so it fits the workspace as it is then.
  const openSources = () =>
    inputs.mutate(undefined, {
      onSuccess: (data) =>
        replacer.request(runDraft(getWorkspace(), data).drafts, `Run ${runLabel(run.id)}’s inputs`, () => {
          const d = runDraft(getWorkspace(), data);
          updateWorkspace(d.patch);
          setNotes(d.notes);
          if (!d.notes.length) close();
          else setStep('open');
        }),
    });
  const failure = inputs.error ?? derive.error ?? del.remove.error;

  return (
    <div ref={host} style={{ position: 'relative' }}>
      <button
        type="button"
        className="od-btn od-btn--icon"
        aria-label="More: export for CI, open the run's configuration and scenario, delete"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => (open ? close() : setOpen(true))}
      >
        <Icon name="ellipsis" />
      </button>
      {open && (
        <div className="od-menu od-wbhead__menu" style={{ top: 'calc(100% + 4px)', right: 0, width: step === 'export' ? 380 : 340, padding: step === 'export' ? 0 : undefined }} role="menu" aria-label="More">
          {replacer.asking ? (
            replacer.confirm
          ) : step === 'export' ? (
            <>
              <div className="od-muted" style={{ padding: '12px 14px 0', fontSize: 12 }} role="status">
                Opened run {runLabel(run.id)}’s configuration and scenario.
                {notes?.map((n, i) => <span key={i} style={{ display: 'block', marginTop: 4 }}>{n}</span>)}
              </div>
              <ExportForCI ws={ws} />
            </>
          ) : step === 'delete' ? (
            <Confirm label="Delete run" text={DELETE_WARNING} yes="Delete run" no="Keep" busy={del.remove.isPending} onYes={() => del.remove.mutate(run.id)} onNo={() => { del.cancel(); setStep('menu'); }} danger />
          ) : step === 'open' && notes ? (
            <div style={{ padding: 8, display: 'flex', flexDirection: 'column', gap: 6 }} role="status">
              <div className="t-body-strong">Opened</div>
              {notes.map((n, i) => <p key={i} className="od-muted" style={{ margin: 0, fontSize: 12.5 }}>{n}</p>)}
              <button type="button" className="od-btn od-btn--sm" onClick={close}>Done</button>
            </div>
          ) : (
            <>
              <button type="button" role="menuitem" className="od-menu__item" disabled={inputs.isPending || derive.isPending} onClick={exportForCI}>
                <Icon name="download" />
                <span style={{ flex: 1 }}>Export for CI</span>
                <span className="od-subtle" style={{ fontSize: 12 }}>configuration and scenario</span>
              </button>
              <button type="button" role="menuitem" className="od-menu__item" disabled={inputs.isPending} onClick={openSources}>
                <Icon name="pencil" />
                <span style={{ flex: 1 }}>Open the run's configuration and scenario</span>
              </button>
              <div className="od-menu__sep" />
              <button
                type="button"
                role="menuitem"
                className="od-menu__item"
                disabled={!canDelete(run) || !!deleteDenied}
                title={deleteDenied ?? (canDelete(run) ? 'Delete this run and its recorded evidence' : 'Cancel the run first')}
                onClick={() => { del.ask(run.id); setStep('delete'); }}
              >
                <Icon name="trash-2" />
                <span style={{ flex: 1 }}>Delete run</span>
              </button>
            </>
          )}
          {failure && <div className="od-field__error" role="alert" style={{ padding: '6px 8px' }}>{failure.message}</div>}
        </div>
      )}
    </div>
  );
}

function Confirm(p: { label: string; text: string; yes: string; no: string; busy?: boolean; danger?: boolean; onYes(): void; onNo(): void }) {
  return (
    <div role="alertdialog" aria-label={p.label} style={{ padding: 8, display: 'flex', flexDirection: 'column', gap: 8 }}>
      <span style={{ fontSize: 12.5, lineHeight: '18px' }}>{p.text}</span>
      <div style={{ display: 'flex', gap: 6 }}>
        <button type="button" className={`od-btn od-btn--sm ${p.danger ? 'od-btn--danger' : 'od-btn--primary'}`} disabled={p.busy} onClick={p.onYes}>{p.yes}</button>
        <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={p.onNo}>{p.no}</button>
      </div>
    </div>
  );
}
