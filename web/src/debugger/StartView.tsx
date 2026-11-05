import { useEffect, useRef, useState, type DragEvent, type ReactNode, type RefObject } from 'react';
import { Link } from '@tanstack/react-router';
import { useQueries, useQuery } from '@tanstack/react-query';
import { api, ApiError } from '../api/client';
import type { Sample } from '../api/types';
import { openTextFile } from '../files';
import { Icon } from '../ui/Icon';
import { runListQuery } from '../runs/runList';
import { runCountText } from '../runs/runPages';
import { supportQuery } from '../runtimes/RuntimeSelect';
import { buildsQuery } from '../runtimes/pages/queries';
import { SAMPLE_DRAFTS, samplePatch, useSamples } from '../samples/SamplePicker';
import {
  decodeText,
  kindDraft,
  newPatch,
  openedPatch,
  type Draft,
  type DraftKind,
} from '../workspace/drafts';
import { getWorkspace, updateWorkspace, useWorkspace } from '../workspace/store';
import { useReplaceDrafts } from '../workspace/useReplaceDrafts';
import { CONFIG_TEMPLATES, type ConfigTemplate } from '../config/templates';
import { saveState, type SaveState } from './headModel';
import { evidenceFact } from './runFacts';
import { SaveIdent } from './SaveIdent';
import {
  activeBuilds,
  activeCaptures,
  attention,
  classifyText,
  draftCounts,
  draftSummary,
  editedText,
  hasDraft,
  isYamlName,
  newestFirst,
  pasteShortcut,
  recentRuns,
  recentSuites,
  runRow,
  SAVED_KINDS,
  savedRows,
  savedStatus,
  startMode,
  type Badge,
  type SavedKind,
  type SavedRow,
} from './start';
import { Permitted } from '../ui/Permitted';
import './start.css';

/**
 * StartView is the debugger's start state: the entry and resume screen shown
 * when no configuration is loaded and no run is selected, and reopened from the
 * debugger. First-time users get four starting actions and the bundled
 * examples; returning users get their draft, runs, test results, saved work,
 * what is running and what needs them. Every list loads and fails on its own:
 * a failing query never blocks the start actions.
 *
 * `onLeave` is called when the user continues to the workbench from here
 * (Resume, or after opening a configuration, example or saved source), so the
 * parent can stop showing the start state while a draft exists.
 */
export function StartView({ onLeave }: { onLeave?: () => void }) {
  const ws = useWorkspace();
  const now = useNow();
  const [showExamples, setShowExamples] = useState(false);
  const [showNew, setShowNew] = useState(false);
  const [notice, setNotice] = useState<string>();
  const [over, setOver] = useState(false);
  const [busy, setBusy] = useState<string>();
  const examplesRef = useRef<HTMLElement>(null);

  // ----- data: every list is its own query and fails on its own -----
  const runs = useQuery({ ...runListQuery, retry: false });
  const suites = useQuery({ queryKey: ['suites'], queryFn: api.suites, retry: false });
  const saved = useQueries({
    queries: SAVED_KINDS.map((kind) => ({ queryKey: ['artifacts', kind], queryFn: () => api.artifacts(kind), retry: false })),
  });
  const captures = useQuery({
    queryKey: ['captures', 'rail'],
    queryFn: () => api.captures(),
    refetchInterval: 10_000,
    refetchIntervalInBackground: false,
    retry: false,
  });
  const builds = useQuery(buildsQuery);
  const samples = useSamples();
  const support = useQuery({ ...supportQuery, retry: false });

  const savedList = savedStatus(saved.map((q) => ({
    status: q.status,
    httpStatus: q.error instanceof ApiError ? q.error.status : undefined,
    message: q.error?.message,
  })));
  const savedItems = savedRows(Object.fromEntries(SAVED_KINDS.map((k, i) => [k, saved[i].data?.items ?? []])), now);
  const runList = runs.data?.runs ?? (runs.data ? [] : undefined);
  const runRows = recentRuns(runList, now);
  const suiteRows = recentSuites(suites.data, runList, now);
  const liveCaptures = activeCaptures(captures.data, now);
  const liveBuilds = activeBuilds(builds.data?.builds);
  const problems = attention({ builds: builds.data?.builds });
  const draft = hasDraft(ws) ? draftSummary(ws) : undefined;
  // The resume card shows the draft's own last run; older drafts fall back to the newest run.
  const own = useQuery({ queryKey: ['run', ws.lastRunId], queryFn: () => api.run(ws.lastRunId!), enabled: !!ws.lastRunId, retry: false });
  const newest = runList ? newestFirst(runList)[0] : undefined;
  const latest = ws.lastRunId ? own.data : newest;

  const settled = (q: { isPending: boolean }, n: number) => (q.isPending ? undefined : n);
  const mode = startMode({
    draft: !!draft,
    runs: settled(runs, runList?.length ?? 0),
    suites: settled(suites, suites.data?.length ?? 0),
    saved: savedList.state === 'loading' ? undefined : savedItems.length,
    active: captures.isPending || builds.isPending ? undefined : liveCaptures.length + liveBuilds.length,
  });

  // ----- replacing drafts: always after an inline confirmation when dirty -----
  const leave = () => onLeave?.();
  const replacer = useReplaceDrafts({ after: leave });
  const request = (drafts: Draft[], what: string, apply: () => void, skipAsk = false) => {
    setNotice(undefined);
    replacer.request(drafts, what, apply, { skipAsk });
  };
  const take = (kind: DraftKind, text: string, label: string) =>
    request([kindDraft(kind)], label || `The pasted ${kind}`, () => updateWorkspace(openedPatch(getWorkspace(), kind, text, label)));
  const openFile = async () => {
    const f = await openTextFile('.yaml,.yml');
    if (f) take('configuration', f.text, f.name);
  };
  const takeUnknown = (text: string, label: string) => {
    const kind = classifyText(text);
    if (kind) take(kind, text, label);
    else setNotice(`${label || 'The pasted text'} is not a Collector configuration or a scenario.`);
  };
  const startNew = (t: ConfigTemplate) =>
    request(['config', 'scenario'], `A new ${t.label.toLowerCase()}`, () => updateWorkspace(newPatch(getWorkspace(), t.yaml)));
  const loadExample = (s: Sample) => {
    const cur = getWorkspace();
    const same = cur.configYAML === s.config_yaml && cur.scenarioYAML === s.scenario_yaml;
    request(SAMPLE_DRAFTS, `The example “${s.title}”`, () => updateWorkspace(samplePatch(getWorkspace(), s)), same);
  };
  const openSaved = async (row: SavedRow) => {
    const kind = row.kind as DraftKind;
    setBusy(row.id);
    setNotice(undefined);
    try {
      const data = await api.artifactRevision(row.id, row.meta.head_revision_id);
      const text = decodeText(data.content_base64 ?? '');
      request([kindDraft(kind)], `“${row.label}”`, () =>
        updateWorkspace(openedPatch(getWorkspace(), kind, text, row.label, { artifactId: row.id, revisionId: row.meta.head_revision_id })),
      );
    } catch (e) {
      setNotice(`${row.label} could not be opened: ${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setBusy(undefined);
    }
  };

  // Paste anywhere outside a field, as the drop card says.
  const paste = useRef(takeUnknown);
  paste.current = takeUnknown;
  useEffect(() => {
    const onPaste = (e: ClipboardEvent) => {
      const t = e.target as HTMLElement | null;
      if (t && (t.isContentEditable || /^(input|textarea|select)$/i.test(t.tagName))) return;
      const text = e.clipboardData?.getData('text/plain') ?? '';
      if (!text.trim()) return;
      e.preventDefault();
      paste.current(text, '');
    };
    document.addEventListener('paste', onPaste);
    return () => document.removeEventListener('paste', onPaste);
  }, []);

  const onDragOver = (e: DragEvent) => {
    if (!e.dataTransfer?.types.includes('Files')) return;
    e.preventDefault();
    setOver(true);
  };
  const onDrop = async (e: DragEvent) => {
    e.preventDefault();
    setOver(false);
    const file = e.dataTransfer?.files[0];
    if (!file) return;
    if (!isYamlName(file.name)) {
      setNotice(`${file.name} is not a YAML file.`);
      return;
    }
    takeUnknown(await file.text(), file.name);
  };

  // The first-time tile moves to the examples that sit below it.
  const focusExamples = () => {
    examplesRef.current?.scrollIntoView?.({ block: 'start' });
    examplesRef.current?.querySelector('button')?.focus();
  };

  const examples = (
    <ExamplesCard
      innerRef={examplesRef}
      samples={samples}
      onPick={loadExample}
    />
  );
  const newCard = showNew && <NewCard onPick={startNew} onClose={() => setShowNew(false)} />;
  const states = {
    configuration: saveState(ws, 'configuration', saved[SAVED_KINDS.indexOf('configuration')].data?.items),
    scenario: ws.scenarioYAML.trim() ? saveState(ws, 'scenario', saved[SAVED_KINDS.indexOf('scenario')].data?.items) : undefined,
  };
  const feedback = (replacer.asking || notice) && (
    <div className="od-start__stack-tight" style={{ maxWidth: mode === 'returning' ? 1160 : 920, width: '100%', margin: '0 auto 16px' }}>
      {replacer.confirm}
      {notice && (
        <div className="od-callout od-callout--fail" role="alert">
          <Icon name="octagon-alert" />
          <div className="od-callout__main">
            <div className="od-callout__body">{notice}</div>
          </div>
          <button type="button" className="od-btn od-btn--sm" onClick={() => setNotice(undefined)}>Dismiss</button>
        </div>
      )}
    </div>
  );

  return (
    <>
      <header className="od-wbhead">
        <span className="t-heading">Debugger</span>
        <span className="od-start__spacer" />
        <div className="od-start__line">
          {mode === 'returning' ? (
            <>
              <button type="button" className="od-btn" aria-expanded={showNew} onClick={() => setShowNew(!showNew)}>
                <Icon name="plus" />
                <span>New</span>
              </button>
              <button type="button" className="od-btn" onClick={() => void openFile()}>
                <Icon name="folder-open" />
                <span>Open configuration</span>
              </button>
              <button type="button" className="od-btn od-btn--ghost" aria-expanded={showExamples} onClick={() => setShowExamples(!showExamples)}>
                <Icon name="flask-conical" />
                <span>Try an example</span>
              </button>
            </>
          ) : (
            <Link to="/saved" className="od-btn od-btn--ghost">
              <Icon name="library" />
              <span>Saved work</span>
            </Link>
          )}
          <Link to="/runs" className="od-btn">
            <Icon name="history" />
            <span>Runs</span>
            {runs.data && <span className="od-btn__count">{runCountText(runs.data)}</span>}
          </Link>
        </div>
      </header>
      <div className="od-page" onDragOver={onDragOver} onDragLeave={() => setOver(false)} onDrop={(e) => void onDrop(e)}>
        {feedback}
        {mode === 'returning' ? (
          <div className="od-start">
            <div className="od-start__returning">
              <div className="od-start__col">
                {newCard}
                {draft && (
                  <ResumeCard
                    draft={draft}
                    states={states}
                    latest={latest && runRow(latest, now)}
                    own={!!ws.lastRunId}
                    evidence={latest && evidenceFact(latest)}
                    edited={editedText(ws.editedAt, now)}
                    onResume={onLeave}
                  />
                )}
                {showExamples && examples}
                <RunsCard rows={runRows} state={runs} />
                {suiteRows.length > 0 && <SuitesCard rows={suiteRows} />}
                {savedList.state !== 'none' && (
                  <SavedCard
                    rows={savedItems}
                    state={savedList}
                    busy={busy}
                    onOpen={(row) => void openSaved(row)}
                  />
                )}
              </div>
              <aside className="od-start__col" aria-label="Running work and problems">
                <ActiveCard captures={liveCaptures} builds={liveBuilds} capturesState={captures} buildsState={builds} />
                {problems.length > 0 && <AttentionCard problems={problems} />}
              </aside>
            </div>
          </div>
        ) : (
          <div className="od-start od-start--first">
            <div className="od-start__first">
              <div className="od-start__intro">
                <h1 className="t-display">Investigate a Collector pipeline</h1>
                <p className="od-muted od-start__lede">
                  Run your unchanged configuration on sample telemetry, follow every record through every stage, and see what changed and why.
                </p>
                {mode === 'checking' && <p className="od-subtle od-start__small" role="status">Looking for your earlier work…</p>}
              </div>
              <div className="od-start__tiles">
                <button type="button" className="od-action-tile od-action-tile--primary" aria-expanded={showNew} onClick={() => setShowNew(!showNew)}>
                  <span className="od-action-tile__icon"><Icon name="plus" /></span>
                  <span className="od-action-tile__title">New</span>
                  <span className="od-action-tile__body">Start from a configuration that runs as it is, with an empty scenario.</span>
                </button>
                <button type="button" className="od-action-tile" onClick={() => void openFile()}>
                  <span className="od-action-tile__icon"><Icon name="folder-open" /></span>
                  <span className="od-action-tile__title">Open a Collector configuration</span>
                  <span className="od-action-tile__body">Load a YAML file from disk. The file is never changed: you edit a copy.</span>
                </button>
                <button type="button" className="od-action-tile" onClick={focusExamples}>
                  <span className="od-action-tile__icon"><Icon name="flask-conical" /></span>
                  <span className="od-action-tile__title">Try an example</span>
                  <span className="od-action-tile__body">A ready-made configuration with sample input, mocks and assertions.</span>
                </button>
                <Permitted perm="capture.start"><Link to="/captures/new" className="od-action-tile">
                  <span className="od-action-tile__icon"><Icon name="radio" /></span>
                  <span className="od-action-tile__title">Capture telemetry from a Collector</span>
                  <span className="od-action-tile__body">Record real telemetry from a live Collector and use it as input.</span>
                </Link></Permitted>
                <Link to="/saved" className="od-action-tile">
                  <span className="od-action-tile__icon"><Icon name="library" /></span>
                  <span className="od-action-tile__title">Open saved work</span>
                  <span className="od-action-tile__body">Configurations, scenarios and fixtures saved on this machine.</span>
                </Link>
              </div>
              {newCard}
              <div className="od-start__split">
                {examples}
                <section className="od-start__side" aria-label="Before you start">
                  <div className={`od-card od-start__drop${over ? ' od-start__drop--over' : ''}`}>
                    <span className="od-action-tile__icon"><Icon name="file-code" /></span>
                    <div className="t-body-strong">Drop a configuration anywhere</div>
                    <div className="od-muted od-start__small">
                      A <code>.yaml</code> Collector configuration or a scenario file. Paste from the clipboard with{' '}
                      <span className="od-kbd">{pasteShortcut(typeof navigator === 'undefined' ? '' : navigator.platform)}</span>.
                    </div>
                  </div>
                  <ReadyCard builtIn={support.data?.built_in} verified={support.data?.verified} />
                </section>
              </div>
            </div>
          </div>
        )}
      </div>
    </>
  );
}

// ---------- Replacing drafts ----------



// ---------- Small pieces ----------

const CHIP: Record<Badge['tone'], string> = {
  pass: 'od-chip od-chip--pass',
  fail: 'od-chip od-chip--fail',
  warn: 'od-chip od-chip--warn',
  running: 'od-chip od-chip--running',
  neutral: 'od-chip',
};

function Chip({ badge }: { badge: Badge }) {
  return (
    <span className={CHIP[badge.tone]}>
      <Icon name={badge.icon} size={12} />
      {badge.text}
    </span>
  );
}

function Facet({ name, badge }: { name: string; badge: Badge }) {
  return (
    <div className="od-facet">
      <span className="od-facet__name">{name}</span>
      <span className={`od-facet__value od-facet__value--${badge.tone}`}>
        <Icon name={badge.icon} size={14} />
        {badge.text}
      </span>
    </div>
  );
}

function CardHead({ title, to, toLabel }: { title: string; to: '/runs' | '/saved' | '/saved/suites'; toLabel: string }) {
  return (
    <div className="od-card__head">
      <h2 className="od-card__title">{title}</h2>
      <span className="od-start__spacer" />
      <Link to={to} className="od-btn od-btn--link od-btn--sm">
        <span>{toLabel}</span>
        <Icon name="arrow-right" size={14} />
      </Link>
    </div>
  );
}

/** ListNote is the loading, error or empty line of a list. */
function ListNote({ children, error }: { children: ReactNode; error?: boolean }) {
  return (
    <div className={`od-start__note${error ? ' od-start__note--error' : ''}`} role={error ? 'alert' : 'status'}>
      {children}
    </div>
  );
}

function useNow(): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const id = setInterval(() => setNow(new Date()), 60_000);
    return () => clearInterval(id);
  }, []);
  return now;
}

// ---------- Examples ----------

function ExamplesCard({
  samples,
  onPick,
  innerRef,
}: {
  samples: ReturnType<typeof useSamples>;
  onPick(s: Sample): void;
  innerRef: RefObject<HTMLElement | null>;
}) {
  return (
    <section className="od-card" ref={innerRef} aria-label="Examples" style={{ overflow: 'hidden' }}>
      <div className="od-card__head">
        <h2 className="od-card__title">Examples</h2>
        <span className="od-subtle od-start__small">bundled · run offline</span>
      </div>
      {samples.error ? (
        <ListNote error>Examples could not be listed: {samples.error.message}</ListNote>
      ) : !samples.data ? (
        <ListNote>Loading examples…</ListNote>
      ) : samples.data.length === 0 ? (
        <ListNote>No examples are bundled with this build.</ListNote>
      ) : (
        <ul>
          {samples.data.map((s) => (
            <li key={s.id}>
              <button type="button" className="od-start__row" onClick={() => onPick(s)}>
                <span className="od-action-tile__icon"><Icon name="file-code" size={14} /></span>
                <span className="od-start__row-main">
                  <span className="od-start__row-title">
                    <span className="t-body-strong">{s.title}</span>
                    {s.expect === 'fail' && (
                      <span className="od-chip od-chip--warn">
                        <Icon name="bug" size={12} />
                        Has a bug
                      </span>
                    )}
                  </span>
                  <span className="od-start__row-body">{s.description}</span>
                </span>
                <Icon name="chevron-right" className="od-subtle" />
              </button>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function ReadyCard({ builtIn, verified }: { builtIn?: string; verified?: string[] }) {
  return (
    <div className="od-card od-start__ready">
      <div className="od-section-label">Ready on this machine</div>
      {builtIn && (
        <div className="od-start__ready-line od-start__ready-line--ok">
          <Icon name="circle-check" size={14} />
          <span>
            Built-in runtime <code>{builtIn}</code>
            {verified?.includes(builtIn) ? ' verified' : ''}
          </span>
        </div>
      )}
      <div className="od-start__ready-line">
        <Icon name="lock" size={14} />
        <span>Everything stays on this machine. No external calls.</span>
      </div>
    </div>
  );
}

// ---------- Returning ----------

function ResumeCard({
  draft,
  states,
  latest,
  own,
  evidence,
  edited,
  onResume,
}: {
  draft: ReturnType<typeof draftSummary>;
  states: { configuration: SaveState; scenario?: SaveState };
  latest?: ReturnType<typeof runRow>;
  /** `latest` is the last run started from this work, not just the newest run. */
  own: boolean;
  evidence?: Badge;
  edited: string;
  onResume?: () => void;
}) {
  return (
    <section className="od-card od-start__resume" aria-label="Where you left off">
      <div className="od-start__line">
        <span className="od-section-label">Continue where you left off</span>
        <span className="od-start__spacer" />
        <span className="od-subtle od-start__small">kept in this browser{edited && ` · ${edited}`}</span>
      </div>
      <div className="od-start__line od-start__line--wrap">
        <SaveIdent icon="file-code" what="Configuration" state={states.configuration} />
        {states.scenario ? (
          <>
            <SaveIdent icon="flask-conical" what="Scenario" state={states.scenario} />
            {draft.scenario !== undefined && <span className="od-muted" style={{ fontSize: 12.5 }}>{draftCounts(draft)}</span>}
          </>
        ) : (
          <span className="od-muted" style={{ fontSize: 12.5 }}>No scenario yet</span>
        )}
      </div>
      {latest && (
        <div className="od-start__last">
          <span className="od-subtle od-start__small">{own ? 'Last run' : 'Latest run'}</span>
          <span className="od-mono od-start__small">{latest.shortId}</span>
          <div className="od-facets od-facets--inline">
            <Facet name="Execution" badge={latest.execution} />
            {evidence && <Facet name="Evidence" badge={evidence} />}
            <Facet name="Assertions" badge={latest.assertions} />
          </div>
        </div>
      )}
      <div className="od-start__actions">
        {onResume && (
          <button type="button" className="od-btn od-btn--primary" onClick={onResume}>
            <Icon name="pencil" />
            <span>Continue</span>
          </button>
        )}
        {latest && (
          <Link to="/" search={{ run: latest.id }} className="od-btn">
            <span>Investigate run {latest.shortId}</span>
            <Icon name="arrow-right" size={14} />
          </Link>
        )}
      </div>
    </section>
  );
}

/** NewCard starts new work from a configuration template and an empty scenario. */
function NewCard({ onPick, onClose }: { onPick(t: ConfigTemplate): void; onClose(): void }) {
  return (
    <section className="od-card" aria-label="New" style={{ overflow: 'hidden' }}>
      <div className="od-card__head">
        <h2 className="od-card__title">New</h2>
        <span className="od-subtle od-start__small">a configuration that runs as it is · an empty scenario · nothing saved yet</span>
        <span className="od-start__spacer" />
        <button type="button" className="od-btn od-btn--ghost od-btn--sm od-btn--icon" aria-label="Close" onClick={onClose}>
          <Icon name="x" />
        </button>
      </div>
      <ul>
        {CONFIG_TEMPLATES.map((t) => (
          <li key={t.id}>
            <button type="button" className="od-start__row" onClick={() => onPick(t)}>
              <span className="od-action-tile__icon"><Icon name="file-code" size={14} /></span>
              <span className="od-start__row-main">
                <span className="t-body-strong">{t.label}</span>
                <span className="od-start__row-body">{t.description}</span>
              </span>
              <Icon name="chevron-right" className="od-subtle" />
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}

function RunsCard({ rows, state }: { rows: ReturnType<typeof recentRuns>; state: { isPending: boolean; error: Error | null } }) {
  return (
    <section className="od-card" aria-label="Recent runs" style={{ overflow: 'hidden' }}>
      <CardHead title="Recent runs" to="/runs" toLabel="All runs" />
      {state.error ? (
        <ListNote error>Runs could not be listed: {state.error.message}</ListNote>
      ) : state.isPending ? (
        <ListNote>Loading runs…</ListNote>
      ) : rows.length === 0 ? (
        <ListNote>No runs yet. Prepare an input and run it to see it here.</ListNote>
      ) : (
        <table className="od-table">
          <thead>
            <tr>
              <th>Run</th>
              <th>Scenario</th>
              <th>When</th>
              <th>Execution</th>
              <th>Assertions</th>
              <th><span className="sr-only">Open</span></th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.id}>
                <td className="mono">{r.shortId}</td>
                <td className="od-start__cell-name">{r.scenario || <span className="od-subtle">One input</span>}</td>
                <td className="od-subtle">{r.when}</td>
                <td><Chip badge={r.execution} /></td>
                <td><Chip badge={r.assertions} /></td>
                <td className="od-start__cell-end">
                  <Link to="/" search={{ run: r.id }} className="od-btn od-btn--link od-btn--sm" aria-label={`Investigate run ${r.shortId}`}>
                    <span>Investigate</span>
                    <Icon name="arrow-right" size={14} />
                  </Link>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

function SuitesCard({ rows }: { rows: ReturnType<typeof recentSuites> }) {
  return (
    <section className="od-card" aria-label="Recent test results" style={{ overflow: 'hidden' }}>
      <CardHead title="Recent test results" to="/saved/suites" toLabel="All results" />
      <table className="od-table">
        <tbody>
          {rows.map((s) => (
            <tr key={s.id}>
              <td className="od-start__cell-name">{s.config}</td>
              <td className="od-subtle">{s.members === 1 ? '1 scenario' : `${s.members} scenarios`}</td>
              <td className="od-subtle">{s.when}</td>
              <td><Chip badge={s.outcome} /></td>
              <td className="od-start__cell-end">
                <Link to="/saved/suites/$suiteId" params={{ suiteId: s.id }} className="od-btn od-btn--link od-btn--sm" aria-label={`Open test result of ${s.config}`}>
                  <span>Open</span>
                  <Icon name="arrow-right" size={14} />
                </Link>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

const KIND_ICON: Record<SavedKind, 'file-code' | 'flask-conical' | 'file-text'> = {
  configuration: 'file-code',
  scenario: 'flask-conical',
  fixture: 'file-text',
};

function SavedCard({
  rows,
  state,
  busy,
  onOpen,
}: {
  rows: SavedRow[];
  state: ReturnType<typeof savedStatus>;
  busy?: string;
  onOpen(row: SavedRow): void;
}) {
  return (
    <section className="od-card" aria-label="Recently used saved work" style={{ overflow: 'hidden' }}>
      <CardHead title="Recently used saved work" to="/saved" toLabel="Saved work" />
      {state.state === 'error' ? (
        <ListNote error>Saved work could not be listed: {state.message}</ListNote>
      ) : state.state === 'loading' ? (
        <ListNote>Loading saved work…</ListNote>
      ) : rows.length === 0 ? (
        <ListNote>Nothing is saved yet. Save a configuration or scenario from the debugger to find it here.</ListNote>
      ) : (
        <table className="od-table">
          <tbody>
            {rows.map((r) => (
              <tr key={r.id}>
                <td style={{ width: 28 }}><Icon name={KIND_ICON[r.kind]} className="od-subtle" /></td>
                <td className="mono od-start__cell-name">{r.label}</td>
                <td className="od-subtle">{r.detail}</td>
                <td className="od-subtle">{r.when}</td>
                <td className="od-start__cell-end">
                  {r.draft ? (
                    <button type="button" className="od-btn od-btn--link od-btn--sm" disabled={busy === r.id} onClick={() => onOpen(r)} aria-label={`Open ${r.label}`}>
                      <span>Open</span>
                    </button>
                  ) : (
                    <Link to="/saved/$artifactId" params={{ artifactId: r.id }} className="od-btn od-btn--link od-btn--sm" aria-label={`Details of ${r.label}`}>
                      <span>Details</span>
                    </Link>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

function ActiveCard({
  captures,
  builds,
  capturesState,
  buildsState,
}: {
  captures: ReturnType<typeof activeCaptures>;
  builds: ReturnType<typeof activeBuilds>;
  capturesState: { isPending: boolean; error: Error | null };
  buildsState: { isPending: boolean; error: Error | null };
}) {
  const empty = captures.length === 0 && builds.length === 0;
  const loading = capturesState.isPending || buildsState.isPending;
  return (
    <section className="od-card" aria-label="Active work">
      <div className="od-card__head">
        <h2 className="od-card__title">Active</h2>
      </div>
      <div className="od-card__body od-start__stack">
        {captures.map((c, i) => (
          <div key={c.id} className="od-start__stack">
            {i > 0 && <div className="od-hsep" />}
            <div className="od-start__active">
              <div className="od-start__active-head">
                <Icon name="radio" />
                <span className="t-body-strong">{c.title}</span>
                <span className="od-start__spacer" />
                <Chip badge={c.badge} />
              </div>
              <div className="od-muted od-start__active-detail">{c.detail}</div>
              <div className="od-progress" role="progressbar" aria-label="Capture time spent" aria-valuemin={0} aria-valuemax={100} aria-valuenow={c.progress}>
                <span style={{ width: `${c.progress}%` }} />
              </div>
              <div>
                <Link to="/captures/$captureId" params={{ captureId: c.id }} className="od-btn od-btn--link od-btn--sm">
                  <span>Open capture</span>
                  <Icon name="arrow-right" size={14} />
                </Link>
              </div>
            </div>
          </div>
        ))}
        {builds.map((b, i) => (
          <div key={b.id} className="od-start__stack">
            {(i > 0 || captures.length > 0) && <div className="od-hsep" />}
            <div className="od-start__active">
              <div className="od-start__active-head">
                <Icon name="hammer" />
                <span className="t-body-strong">Build {b.title}</span>
                <span className="od-start__spacer" />
                <Chip badge={b.badge} />
              </div>
              <div className="od-muted od-start__active-detail">{b.detail}</div>
              <div className="od-progress od-progress--indet" aria-hidden="true"><span /></div>
              <div>
                <Link to="/runtimes/builds/$buildId" params={{ buildId: b.id }} className="od-btn od-btn--link od-btn--sm">
                  <span>Open build</span>
                  <Icon name="arrow-right" size={14} />
                </Link>
              </div>
            </div>
          </div>
        ))}
        {empty && loading && <ListNote>Checking captures and builds…</ListNote>}
        {empty && !loading && !capturesState.error && !buildsState.error && <ListNote>No capture or runtime build is running.</ListNote>}
        {capturesState.error && <ListNote error>Captures could not be listed: {capturesState.error.message}</ListNote>}
        {buildsState.error && <ListNote error>Builds could not be listed: {buildsState.error.message}</ListNote>}
      </div>
    </section>
  );
}

function AttentionCard({ problems }: { problems: ReturnType<typeof attention> }) {
  return (
    <section className="od-card" aria-label="Needs attention">
      <div className="od-card__head">
        <h2 className="od-card__title">Needs attention</h2>
        <span className="od-tab__count od-tab__count--warn">{problems.length}</span>
      </div>
      <div className="od-card__body od-start__stack-tight">
        {problems.map((p) => (
          <div key={p.id} className="od-callout od-callout--warn" role="status">
            <Icon name="triangle-alert" />
            <div className="od-callout__main">
              <div className="od-callout__title">{p.title}</div>
              <div className="od-callout__body">{p.body}</div>
              <div className="od-callout__actions">
                <Link to="/runtimes/builds/$buildId" params={{ buildId: p.to.id }} className="od-btn od-btn--sm">
                  <span>{p.action}</span>
                </Link>
              </div>
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}
