import { useMemo, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';
import { workspaceRoute, type WorkspaceSearch } from '../router';
import { api } from '../api/client';
import { runActive } from '../api/runState';
import { configMarkers } from '../editor/markers';
import { LaneGraph } from '../graph/LaneGraph';
import { layoutGraph } from '../graph/layout';
import { runBlocker } from '../input/quickInput';
import { CaptureInputPicker } from '../capture/CaptureInputPicker';
import { END_LABELS, endMark, pathHighlight } from '../inspect/journey';
import { edgeStats, nodeStats } from '../runs/annotations';
import { useRun } from '../runs/useRun';
import { runtimesQuery } from '../runtimes/RuntimeSelect';
import { runtimeOptions } from '../runtimes/runtimes';
import { Icon } from '../ui/Icon';
import { runDraft } from '../workspace/drafts';
import { InputPane } from '../workspace/InputPane';
import { getWorkspace, updateWorkspace, useWorkspace } from '../workspace/store';
import { useValidation } from '../workspace/useValidation';
import { WorkspacePanes } from '../workspace/WorkspacePanes';
import { CaptureHandoffBar, useCaptureHandoff } from './CaptureHandoff';
import { ComparisonView } from './ComparisonView';
import { ComponentPane } from './ComponentPane';
import { draftDifferences, differenceSummary, draftsEmpty } from './draftChanges';
import { InvestigatePane } from './InvestigatePane';
import { runLabel } from './runFacts';
import { RunProgress } from './RunProgress';
import { RunsDrawer } from './RunsDrawer';
import { StartView } from './StartView';
import { debuggerState, readiness, type ProblemFix } from './state';
import { AddAssertions } from './AddAssertions';
import { WorkbenchHead } from './WorkbenchHead';
import './debugger.css';

/** The selection a run leaves behind when the debugger goes back to editing. */
const LEAVE_RUN: Partial<WorkspaceSearch> = {
  start: undefined, run: undefined, node: undefined, input: undefined, record: undefined, tab: undefined, view: undefined, trace: undefined, compare: undefined,
};

/**
 * Debugger is the workbench at `/`: Start (nothing loaded), Prepare (edit the
 * next experiment), Running and Investigate (a finished run's evidence),
 * all selected by the URL and the drafts (see state.ts). The header names what
 * is shown, the left pane holds the drafts or the run's findings, and the graph
 * and the selected component are on the right.
 */
export function Debugger() {
  const ws = useWorkspace();
  const qc = useQueryClient();
  const search = workspaceRoute.useSearch();
  const navigate = useNavigate({ from: '/' });
  const setSearch = (patch: Partial<WorkspaceSearch>) =>
    navigate({ search: (s: WorkspaceSearch) => ({ ...s, ...patch }), replace: true });
  const [captureOpen, setCaptureOpen] = useState(false);
  const [runsOpen, setRunsOpen] = useState(false);
  const [testOpen, setTestOpen] = useState(false);
  const [changesOpen, setChangesOpen] = useState(false);
  const [wrote, setWrote] = useState(false);
  const [editError, setEditError] = useState<string>();

  const validation = useValidation(ws);
  const inspection = validation.data;
  const markers = useMemo(() => configMarkers(ws.configYAML, inspection?.diagnostics ?? []), [ws.configYAML, inspection]);
  const view = useRun(search.run);
  const report = view.run?.report;
  const state = debuggerState({ run: view.run, runId: search.run, compare: search.compare, hasConfig: !!ws.configYAML.trim() || wrote, start: search.start });
  const investigating = state === 'investigate' || state === 'compare';
  const handoff = useCaptureHandoff();
  const handoffBar = handoff.handoff && (
    <CaptureHandoffBar handoff={handoff.handoff} hasConfig={!!ws.configYAML.trim()} onDone={handoff.clear} />
  );

  // The runtime beside Run: missing or unverified ones are problems named where Run is.
  const runtimes = useQuery(runtimesQuery);
  const chosen = ws.runtime ? runtimeOptions(runtimes.data, ws.runtime).find((o) => o.value === ws.runtime) : undefined;
  const yamlError = markers.find((m) => m.code === 'yaml');
  const inputBlocker = runBlocker(ws, !!inspection?.valid);
  const ready = readiness({
    configText: ws.configYAML,
    yamlError: yamlError ? { message: yamlError.message, line: yamlError.location.startLine } : undefined,
    inspection,
    validationError: validation.error?.message,
    inputBlocker,
    runtime: ws.runtime && !runtimes.isPending ? { name: ws.runtime, missing: chosen?.missing, unverified: chosen?.unverified } : undefined,
    running: state === 'running',
  });
  const canRun = !ready.problems.some((p) => p.blocking) && !!inspection?.valid && !inputBlocker;

  // What the shown run used, to say whether the drafts have moved on since.
  const inputs = useQuery({
    queryKey: ['run-inputs', search.run],
    queryFn: () => api.runInputs(search.run!),
    enabled: investigating && !!search.run && !!view.run && !runActive(view.run.state),
    staleTime: Infinity,
    retry: false,
  });
  const differences = useMemo(() => (inputs.data ? draftDifferences(ws, inputs.data) : []), [ws, inputs.data]);

  // A finished run is shown on the topology it ran with; edits since then apply to the next run.
  const topology = report?.topology ?? inspection?.topology;
  const layout = useMemo(() => (topology ? layoutGraph(topology) : undefined), [topology]);
  const selectedNode = layout?.nodes.find((n) => n.key === search.node);
  const nStats = useMemo(
    () => (search.run ? nodeStats(view.events, report?.stages, view.live) : undefined),
    [search.run, view.events, report, view.live],
  );
  const eStats = useMemo(() => (layout && nStats ? edgeStats(layout.edges, nStats) : undefined), [layout, nStats]);
  const followed = search.trace ? report?.lineage?.find((l) => l.id === search.trace) : undefined;
  const path = useMemo(
    () => (followed ? pathHighlight(followed, new Map(view.events.map((e) => [e.seq, e]))) : undefined),
    [followed, view.events],
  );

  const showRun = (run: string) => setSearch({ run, input: undefined, record: undefined, trace: undefined, compare: undefined });
  const clearForSample = () =>
    setSearch({ run: undefined, node: undefined, input: undefined, record: undefined, tab: undefined, trace: undefined, editor: undefined });
  const prepare = (editor?: string) => navigate({ search: (s: WorkspaceSearch) => ({ ...s, ...LEAVE_RUN, editor: editor ?? s.editor }) });

  // Edit and rerun: in a fresh session the drafts are empty, so the run's own sources become the drafts.
  const editAndRerun = async () => {
    setEditError(undefined);
    if (search.run && draftsEmpty(getWorkspace())) {
      try {
        const data = await qc.fetchQuery({ queryKey: ['run-inputs', search.run], queryFn: () => api.runInputs(search.run!), staleTime: Infinity });
        updateWorkspace(runDraft(getWorkspace(), data).patch);
      } catch (e) {
        setEditError(`The run's configuration and scenario could not be opened: ${e instanceof Error ? e.message : String(e)}`);
      }
    }
    void prepare();
  };
  const fix = (f: ProblemFix) => {
    if (f.action === 'config') void setSearch({ editor: 'config' });
    else if (f.action === 'inputs') void setSearch({ editor: 'inputs' });
    else if (f.action === 'capture') {
      void setSearch({ editor: 'inputs' });
      setCaptureOpen(true);
    } else void navigate({ to: '/runtimes', search: { from: `${location.pathname}${location.search}` } });
  };
  // Back to the start state; pushed so browser Back returns to the workbench.
  const openStart = () => navigate({ search: (s: WorkspaceSearch) => ({ ...s, ...LEAVE_RUN, start: true }) });

  if (state === 'start') {
    return (
      <>
        {handoffBar}
        <StartView
          onLeave={() => {
            setWrote(true);
            void setSearch({ start: undefined });
          }}
        />
      </>
    );
  }

  const bottom = selectedNode ? (
    <ComponentPane node={selectedNode} runId={search.run} view={view} inspection={inspection} search={search} setSearch={setSearch} />
  ) : state === 'running' && search.run ? (
    <RunProgress runId={search.run} view={view} />
  ) : undefined;

  const following = followed && (
    <div className="following" role="status">
      Following <strong>{followed.name || followed.id}</strong>
      {(followed.deliveries ?? []).map((d, i) => (
        <span key={i} className="end end-delivered">✓ {d.exporter}</span>
      ))}
      {(followed.stops ?? []).map((s, i) => (
        <span key={i} className={`end end-${s.end}`}>
          {endMark(s.end)} {END_LABELS[s.end] ?? s.end}
          {s.component && ` at ${s.component}`}
        </span>
      ))}
      <button type="button" className="od-btn od-btn--ghost od-btn--sm od-btn--icon" onClick={() => setSearch({ trace: undefined })} aria-label="Stop following" title="Stop following">
        <Icon name="x" size={14} />
      </button>
    </div>
  );
  const stamp = search.run && state !== 'prepare' ? `${state === 'running' ? 'Live' : 'Recorded'} · run ${runLabel(search.run)}${state === 'running' ? '' : ' · read-only'}` : undefined;

  const graph = (
    <div className="od-wbgraph">
      {topology ? (
        <LaneGraph
          graph={topology}
          selected={search.node}
          onSelect={(node) => setSearch({ node, input: undefined, record: undefined, tab: undefined, view: undefined })}
          nodeStats={nStats}
          edgeStats={eStats}
          path={path}
          stamp={stamp}
          toolbar={following || undefined}
          draft={!investigating && state !== 'running'}
        />
      ) : (
        <section className={`od-graph${investigating ? '' : ' od-graph--draft'}`} aria-label="Pipeline graph">
          <div className="od-empty">
            <div className="od-empty__title">{investigating ? 'No topology was recorded' : 'No graph yet'}</div>
            <div className="od-empty__body">The pipeline graph appears when the configuration is valid.</div>
          </div>
        </section>
      )}
      {state === 'prepare' && topology && inputBlocker === 'Add an input to run' && (
        <div className="od-graph__hint">
          <div className="od-callout od-callout--info" role="status">
            <Icon name="info" />
            <div className="od-callout__main">
              <div className="od-callout__title">No input yet</div>
              <div className="od-callout__body">The graph comes from the configuration alone. Add sample telemetry to run it.</div>
              <div className="od-callout__actions">
                <button type="button" className="od-btn od-btn--primary od-btn--sm" onClick={() => setSearch({ editor: 'inputs' })}>
                  <Icon name="plus" size={14} />
                  <span>Add input</span>
                </button>
                <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={() => fix({ action: 'capture', label: '' })}>
                  <span>Add captured input</span>
                </button>
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );

  return (
    <>
      <WorkbenchHead
        state={state}
        ws={ws}
        readiness={ready}
        canRun={canRun}
        runId={search.run}
        view={view}
        draftDiffers={differences.length > 0}
        onRun={showRun}
        onFix={fix}
        onOpenRuns={() => setRunsOpen((o) => !o)}
        onEdit={() => void editAndRerun()}
        onCompare={(compare) => setSearch({ compare })}
        onAddAssertions={() => setTestOpen(true)}
        onDeleted={() => setSearch(LEAVE_RUN)}
        onStart={() => void openStart()}
      />
      {handoffBar}
      <RunsDrawer open={runsOpen} onClose={() => setRunsOpen(false)} currentRunId={search.run} />
      {ws.sessionOnly && (
        <div className="od-callout od-callout--info od-callout--bar" role="status">
          <Icon name="lock" />
          <div className="od-callout__main">
            <span>Sensitive capture input: the open configuration and scenario stay in this browser session. Saving, exporting or submitting a run retains a separate copy.</span>
          </div>
        </div>
      )}
      {editError && (
        <div className="od-callout od-callout--warn od-callout--bar" role="alert">
          <Icon name="triangle-alert" />
          <div className="od-callout__main"><span>{editError}</span></div>
        </div>
      )}
      {investigating && differences.length > 0 && (
        <>
          <div className="od-callout od-callout--warn od-callout--bar" role="status">
            <Icon name="triangle-alert" />
            <div className="od-callout__main">
              <span className="od-callout__title">Your configuration or scenario has changed since this run</span>
              <span>{differenceSummary(differences)} Evidence below still describes run {search.run && runLabel(search.run)}.</span>
            </div>
            <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={() => void prepare()}>
              <Icon name="eye" size={14} />
              <span>Show my version</span>
            </button>
            <button type="button" className="od-btn od-btn--ghost od-btn--sm" aria-expanded={changesOpen} onClick={() => setChangesOpen(!changesOpen)}>
              <Icon name="file-diff" size={14} />
              <span>What changed</span>
            </button>
          </div>
          {changesOpen && <ChangedLines differences={differences} />}
        </>
      )}
      {captureOpen && (
        <CaptureInputPicker
          onClose={() => setCaptureOpen(false)}
          onAdded={() => {
            setCaptureOpen(false);
            void setSearch({ editor: 'inputs' });
          }}
        />
      )}
      {state === 'compare' && search.run && search.compare ? (
        <ComparisonView
          runId={search.run}
          compareId={search.compare}
          onChangeRuns={(run, compare) => navigate({ search: (s: WorkspaceSearch) => ({ ...s, run, compare }) })}
        />
      ) : (
        <div style={{ display: 'contents' }}>
          <WorkspacePanes
            left={
              investigating && search.run ? (
                <InvestigatePane runId={search.run} view={view} inspection={inspection} search={search} setSearch={setSearch} />
              ) : (
                <InputPane
                  ws={ws}
                  requested={search.editor}
                  onTab={(editor) => setSearch({ editor })}
                  inspection={inspection}
                  topology={inspection?.topology ?? report?.topology}
                  markers={markers}
                  runId={view.run?.state === 'done' ? search.run : undefined}
                  onRun={showRun}
                  onSampleLoaded={clearForSample}
                />
              )
            }
            graph={graph}
            bottom={bottom}
          />
        </div>
      )}
      {testOpen && investigating && search.run && (
        <AddAssertions
          runId={search.run}
          view={view}
          inspection={inspection}
          onClose={() => setTestOpen(false)}
          onUsed={() => {
            setTestOpen(false);
            void prepare('assertions');
          }}
        />
      )}
    </>
  );
}

/** ChangedLines lists the lines that differ between the draft and the run's sources. */
function ChangedLines({ differences }: { differences: ReturnType<typeof draftDifferences> }) {
  const LIMIT = 12;
  return (
    <div className="od-card" style={{ margin: '0 16px 8px', padding: '8px 12px', fontSize: 12.5 }} role="region" aria-label="What changed since the run">
      {differences.map((d) => (
        <div key={d.part} style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
          <span className="od-section-label">{d.label}</span>
          {d.removed.slice(0, LIMIT).map((l, i) => <code key={`r${i}`} className="sx-d" style={{ color: 'var(--fail)' }}>- {l.trim()}</code>)}
          {d.added.slice(0, LIMIT).map((l, i) => <code key={`a${i}`} style={{ color: 'var(--pass)' }}>+ {l.trim()}</code>)}
          {d.removed.length + d.added.length > 2 * LIMIT && <span className="od-subtle">and more lines</span>}
        </div>
      ))}
    </div>
  );
}
