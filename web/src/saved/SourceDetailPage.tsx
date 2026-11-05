import { useMemo, useState, type ReactNode } from 'react';
import { Link, useParams } from '@tanstack/react-router';
import { useInfiniteQuery, useQuery } from '@tanstack/react-query';
import { api, ApiError } from '../api/client';
import type { ArtifactMetadata, ArtifactRevision, ArtifactRevisionData } from '../api/types';
import { Icon, type IconName } from '../ui/Icon';
import { decodeText } from '../workspace/drafts';
import { clipText, historyOf, HISTORY_STEP, revisionNumber } from './history';
import { kindLabel, originRun, originText, plural, whenText } from './format';
import { canOpenAsDraft } from './openDraft';
import { NoWorkspace } from './NoWorkspace';
import { SavedHead } from './SavedHead';
import { scenarioSummary, type ScenarioSummary, type SummaryLine } from './scenarioView';
import { SuiteLauncher } from './SuiteLauncher';
import { useOpenAsDraft } from './useOpenAsDraft';
import { noWorkspace, revisionQuery, useSavedArtifacts } from './useSavedArtifacts';

type ScenarioTab = 'inputs' | 'mocks' | 'assertions' | 'settings' | 'yaml';

const KIND_ICON: Record<string, IconName> = {
  scenario: 'flask-conical',
  configuration: 'file-code',
  manifest: 'package',
};

/**
 * SourceDetailPage shows one saved source: its revisions (newest first, from
 * the revision list), the selected revision's content and origin, and what can
 * be done with it. Revisions cannot be edited here; a draft opened from one
 * is saved in the debugger as a new revision.
 */
export function SourceDetailPage() {
  const { artifactId = '' } = useParams({ strict: false });
  const meta = useQuery({ queryKey: ['saved', 'artifact', artifactId], queryFn: () => api.artifact(artifactId), retry: false, enabled: artifactId !== '' });
  const head = meta.data?.head_revision_id;
  const pages = useInfiniteQuery({
    queryKey: ['saved', 'history', artifactId, head],
    queryFn: ({ pageParam }) => api.artifactRevisions(artifactId, pageParam, HISTORY_STEP),
    initialPageParam: '',
    getNextPageParam: (last) => last.next_cursor || undefined,
    enabled: head !== undefined,
    retry: false,
  });
  const history = useMemo(() => historyOf(pages.data?.pages ?? []), [pages.data]);
  const [picked, setPicked] = useState<string>();
  const currentId = picked ?? head;
  const selected = useQuery({ ...revisionQuery(artifactId, currentId ?? ''), enabled: currentId !== undefined });

  if (meta.isPending) return <Shell><div className="od-empty" role="status"><div className="od-empty__title">Loading…</div></div></Shell>;
  if (meta.error) {
    if (noWorkspace(meta.error)) return <Shell><NoWorkspace /></Shell>;
    const missing = meta.error instanceof ApiError && meta.error.status === 404;
    return (
      <Shell>
        <div className="od-card">
          <div className="od-empty" role="alert">
            <div className="od-empty__title">{missing ? 'This saved source does not exist' : 'The saved source could not be read'}</div>
            <div className="od-empty__body">{missing ? 'It was never saved on this server, or it was removed.' : meta.error.message}</div>
            <div className="od-empty__actions"><Link to="/saved" className="od-btn">Back to Saved work</Link></div>
          </div>
        </div>
      </Shell>
    );
  }

  const m = meta.data;
  const revisions = history.revisions;
  const current = selected.data;

  return (
    <>
      <SavedHead />
      <div className="saved-source">
        <aside className="od-pane saved-source__revisions" aria-label="Revisions">
          <div className="od-pane__head"><span className="od-pane__title">Revisions</span></div>
          <div className="od-pane__body">
            {pages.isPending && <div className="od-muted" style={{ padding: 14 }} role="status">Reading revisions…</div>}
            {pages.error && <div className="od-callout od-callout--fail" role="alert" style={{ margin: 12 }}><div className="od-callout__body">{pages.error.message}</div></div>}
            {revisions.map((r) => (
              <RevisionItem key={r.ref.revision_id} revision={r} head={r.ref.revision_id === m.head_revision_id} selected={r.ref.revision_id === currentId} onPick={() => setPicked(r.ref.revision_id)} />
            ))}
            {history.more && (
              <div style={{ padding: 12 }}>
                <button type="button" className="od-btn od-btn--sm" disabled={pages.isFetchingNextPage} onClick={() => void pages.fetchNextPage()}>Show older revisions</button>
              </div>
            )}
          </div>
        </aside>
        <div className="od-page saved-page">
          {selected.error && <div className="od-callout od-callout--fail" role="alert"><div className="od-callout__body">The revision could not be read: {selected.error.message}</div></div>}
          {current ? <Detail meta={m} revision={current} revisions={revisions} complete={!history.more} /> : selected.isPending && <div className="od-empty" role="status">Reading the revision…</div>}
        </div>
      </div>
    </>
  );
}

function Shell({ children }: { children: ReactNode }) {
  return (
    <>
      <SavedHead />
      <div className="od-page">{children}</div>
    </>
  );
}

function RevisionItem({ revision: r, head, selected, onPick }: { revision: ArtifactRevision; head: boolean; selected: boolean; onPick(): void }) {
  return (
    <button type="button" className="saved-rev" aria-current={selected} onClick={onPick}>
      <span className="saved-rev__top">
        <span className="t-code-strong">rev {revisionNumber(r)}</span>
        {head && <span className="od-chip od-chip--accent od-chip--plain">Current</span>}
        <span className="saved-grow" />
        <span className="od-subtle" style={{ fontSize: 12 }} title={r.created}>{whenText(r.created)}</span>
      </span>
      <span className="od-muted" style={{ fontSize: 12 }}>{originText(r.origin)}</span>
    </button>
  );
}

function Detail({ meta, revision, revisions, complete }: { meta: ArtifactMetadata; revision: ArtifactRevisionData; revisions: ArtifactRevision[]; complete: boolean }) {
  const draft = useOpenAsDraft();
  const [running, setRunning] = useState(false);
  const saved = useSavedArtifacts(running);
  const rev = revisionNumber(revision);
  const revisionId = revision.revision.ref.revision_id;
  const isHead = revisionId === meta.head_revision_id;

  const decoded = useMemo((): { text: string } | { error: string } => {
    try {
      return { text: decodeText(revision.content_base64 ?? '') };
    } catch {
      return { error: 'The content is not valid UTF-8 text.' };
    }
  }, [revision]);
  const text = 'text' in decoded ? decoded.text : '';
  const summary = useMemo(() => (meta.kind === 'scenario' && 'text' in decoded ? scenarioSummary(decoded.text) : undefined), [meta.kind, decoded]);
  const parsed = summary && !('error' in summary) ? summary : undefined;
  const assertions = parsed?.assertions.length ?? 0;

  const origin = originText(revision.revision.origin);
  const first = complete ? revisions[revisions.length - 1] : undefined;
  const firstOrigin = first && first.ref.revision_id !== revision.revision.ref.revision_id ? originText(first.origin) : undefined;
  const run = originRun(revision.revision.origin);

  const member = useMemo(() => ({ ...meta, head_revision_id: revisionId }), [meta, revisionId]);
  const configs = useMemo(() => (saved.data?.items ?? []).filter((x) => x.kind === 'configuration'), [saved.data]);

  return (
    <>
      <div className="saved-row" style={{ gap: 10 }}>
        <Icon name={KIND_ICON[meta.kind] ?? 'file-text'} size={18} className="od-subtle" />
        <h2 className="t-title" style={{ margin: 0 }}><code style={{ fontSize: 20 }}>{meta.label || meta.id}</code></h2>
        <span className="od-chip od-chip--plain">{meta.kind === 'scenario' && assertions > 0 ? `Test · ${plural(assertions, 'assertion')}` : kindLabel(meta.kind)}</span>
        <span className="saved-grow" />
        {meta.kind === 'scenario' && (
          <button type="button" className="od-btn" aria-expanded={running} onClick={() => setRunning(!running)}>
            <Icon name="play" size={14} /><span>Run</span>
          </button>
        )}
        {canOpenAsDraft(meta.kind) && (
          <button type="button" className="od-btn od-btn--primary" disabled={draft.busy || 'error' in decoded} onClick={() => void draft.open(meta, revisionId)}>
            <Icon name="pencil" size={14} /><span>Open</span>
          </button>
        )}
      </div>

      {draft.confirm}
      {draft.error && (
        <div className="od-callout od-callout--fail" role="alert">
          <Icon name="circle-x" size={16} />
          <div className="od-callout__main"><div className="od-callout__body">The revision could not be opened: {draft.error}</div></div>
        </div>
      )}
      {running && (
        <SuiteLauncher
          members={[member]}
          configs={configs}
          heading="Run this scenario as a suite of one"
          note={`runs rev ${rev}${isHead ? ' (current)' : ''}`}
        />
      )}

      <dl className="od-kv">
        <dt>Kind</dt>
        <dd>{meta.kind === 'scenario' ? (assertions > 0 ? 'Scenario with assertions (a test)' : 'Scenario') : kindLabel(meta.kind)}</dd>
        <dt>Origin</dt>
        <dd>
          {run ? <>Derived from run <Link to="/" search={{ run }}><code>{run}</code></Link></> : origin.charAt(0).toUpperCase() + origin.slice(1)}
          {firstOrigin && ` · first revision ${firstOrigin}`}
        </dd>
        <dt>Saved</dt>
        <dd title={revision.revision.created}>{whenText(revision.revision.created)} · {revision.revision.content_bytes} bytes</dd>
        <dt>Revision</dt>
        <dd>rev {rev} of {meta.generation}{isHead ? '' : ' (not the current one)'} · revisions cannot be edited; {canOpenAsDraft(meta.kind) ? 'open it and save a new one' : 'save a changed copy as a new revision from the debugger'}</dd>
      </dl>

      {'error' in decoded ? (
        <div className="od-callout od-callout--fail" role="alert"><Icon name="circle-x" size={16} /><div className="od-callout__main"><div className="od-callout__body">{decoded.error}</div></div></div>
      ) : meta.kind === 'scenario' ? (
        <ScenarioContent summary={summary} text={text} />
      ) : (
        <section className="od-card" style={{ overflow: 'hidden' }}>
          <div className="od-card__head"><h2 className="od-card__title" style={{ margin: 0 }}>Content</h2></div>
          <Text text={text} />
        </section>
      )}
    </>
  );
}

function Text({ text }: { text: string }) {
  const clipped = clipText(text);
  return (
    <>
      <pre className="od-code od-code--flat saved-yaml" tabIndex={0} aria-label="Content">{clipped.text}</pre>
      {clipped.clipped && <div className="od-muted" style={{ padding: '8px 14px', fontSize: 12 }}>Only the first {clipped.text.length.toLocaleString()} characters are shown; download the source for all of it.</div>}
    </>
  );
}

function ScenarioContent({ summary, text }: { summary: ScenarioSummary | { error: string } | undefined; text: string }) {
  const parsed = summary && !('error' in summary) ? summary : undefined;
  const [tab, setTab] = useState<ScenarioTab>((parsed?.assertions.length ?? 0) > 0 ? 'assertions' : parsed ? 'inputs' : 'yaml');
  const tabs: { id: ScenarioTab; label: string; count?: number }[] = [
    { id: 'inputs', label: 'Inputs', count: parsed?.inputs.length },
    { id: 'mocks', label: 'Mocks', count: parsed?.mocks.length },
    { id: 'assertions', label: 'Assertions', count: parsed?.assertions.length },
    { id: 'settings', label: 'Settings' },
    { id: 'yaml', label: 'YAML' },
  ];
  return (
    <section className="od-card" style={{ overflow: 'hidden' }}>
      <div className="od-tabs" role="tablist" aria-label="Scenario content">
        {tabs.map((t) => (
          <button key={t.id} type="button" role="tab" className="od-tab" id={`tab-${t.id}`} aria-selected={tab === t.id} aria-controls="scenario-panel" onClick={() => setTab(t.id)}>
            {t.label}
            {t.count !== undefined && <span className="od-tab__count">{t.count}</span>}
          </button>
        ))}
      </div>
      <div id="scenario-panel" role="tabpanel" aria-labelledby={`tab-${tab}`}>
        {summary && 'error' in summary && tab !== 'yaml' && (
          <div className="od-callout od-callout--warn" role="status" style={{ margin: 14 }}>
            <Icon name="triangle-alert" size={16} />
            <div className="od-callout__main"><div className="od-callout__body">The scenario could not be read ({summary.error}); its text is under YAML.</div></div>
          </div>
        )}
        {parsed && tab === 'inputs' && <Lines lines={parsed.inputs} empty="This scenario has no inputs." />}
        {parsed && tab === 'mocks' && <Lines lines={parsed.mocks} empty="This scenario mocks nothing." />}
        {parsed && tab === 'assertions' && <Assertions lines={parsed.assertions} />}
        {parsed && tab === 'settings' && <Lines lines={parsed.settings} empty="No settings are set; the defaults apply." />}
        {tab === 'yaml' && <Text text={text} />}
      </div>
    </section>
  );
}

function Lines({ lines, empty }: { lines: SummaryLine[]; empty: string }) {
  if (lines.length === 0) return <div className="od-muted" style={{ padding: 14 }}>{empty}</div>;
  return (
    <div className="saved-list">
      {lines.map((l, i) => (
        <div key={i} className="saved-line">
          <span className="t-body-strong">{l.text}</span>
          {l.detail && <span className="saved-line__detail">{l.detail}</span>}
        </div>
      ))}
    </div>
  );
}

function Assertions({ lines }: { lines: SummaryLine[] }) {
  if (lines.length === 0) return <div className="od-muted" style={{ padding: 14 }}>This scenario has no assertions, so it is not a test.</div>;
  return (
    <div className="saved-list">
      {lines.map((l, i) => (
        <div key={i} className="od-assert od-assert--unchecked">
          <Icon name="circle-minus" size={16} />
          <div>
            <div className="od-assert__text">{l.text}</div>
            {l.detail && <div className="od-assert__evidence">{l.detail}</div>}
          </div>
          <div />
        </div>
      ))}
    </div>
  );
}
