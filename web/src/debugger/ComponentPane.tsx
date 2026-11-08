import { useMemo, useState, type KeyboardEvent } from 'react';
import type { WorkspaceSearch } from '../router';
import type { Inspection } from '../api/types';
import type { LayoutNode } from '../graph/layout';
import type { RunView } from '../runs/useRun';
import { TelemetryInspector } from '../inspect/TelemetryInspector';
import { ExtensionActivity } from '../inspect/ExtensionActivity';
import { SamplingPanel } from '../inspect/SamplingPanel';
import { BatchingPanel } from '../inspect/BatchingPanel';
import { FilteringPanel } from '../inspect/FilteringPanel';
import { GroupingPanel } from '../inspect/GroupingPanel';
import { batchingFor } from '../inspect/batching';
import { filteringFor } from '../inspect/filtering';
import { groupingFor } from '../inspect/grouping';
import { samplingFor } from '../inspect/sampling';
import { modeInfo } from '../graph/lanes';
import { Icon, type IconName } from '../ui/Icon';
import { componentSelection } from '../workspace/inspectorTabs';
import { ConfigurationView } from './ConfigurationView';
import { HowItRunsView } from './HowItRunsView';
import { nothingReached } from './findings';
import { journeyPath } from './followPath';
import { fateOf, fateText } from './recordList';
import { statementCount } from './componentConfig';

/** Props of the bottom pane for the component selected in the graph. */
export interface ComponentPaneProps {
  node: LayoutNode;
  /** The selected finished or live run; absent in Prepare. */
  runId?: string;
  view: RunView;
  inspection?: Inspection;
  search: WorkspaceSearch;
  setSearch(patch: Partial<WorkspaceSearch>): void;
  /** Prepare: opens the component in the configuration editor. */
  onOpenYaml?(): void;
}

const KIND_ICONS: Record<string, IconName> = {
  receiver: 'node-receiver',
  processor: 'node-processor',
  exporter: 'node-exporter',
  connector: 'connector',
  extension: 'extension',
};

/**
 * ComponentPane shows the selected component's contextual views. In
 * Investigate the tabs follow the component's kind (Changes, Statements,
 * Decisions, Data, Routing, Aggregation, Activity, Configuration) and reuse
 * the evidence views; in Prepare they explain how the component will run and
 * show its effective configuration. A followed record's path is listed above
 * the view.
 */
export function ComponentPane({ node, runId, view, inspection, search, setSearch, onOpenYaml }: ComponentPaneProps) {
  const [expanded, setExpanded] = useState(false);
  const c = node.component;
  const report = view.run?.report;
  const sel = componentSelection({ tab: search.tab, view: search.view, run: runId }, c);
  const tab = sel.tab.id;
  const mode = modeInfo(c.mode);
  const followed = runId && search.trace ? report?.lineage?.find((l) => l.id === search.trace) : undefined;
  const path = useMemo(() => (followed ? journeyPath(followed, view.events) : []), [followed, view.events]);
  const statements = c.type === 'transform' ? statementCount(report?.ottl, c.id) : undefined;
  const height = expanded ? 'min(72vh, 640px)' : followed || tab === 'ottl' ? 360 : 280;

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(e.key)) return;
    const buttons = [...e.currentTarget.querySelectorAll<HTMLButtonElement>('button:not(:disabled)')];
    if (!buttons.length) return;
    const index = buttons.indexOf(e.target as HTMLButtonElement);
    const next = e.key === 'Home' ? 0 : e.key === 'End' ? buttons.length - 1 : (index + (e.key === 'ArrowLeft' ? -1 : 1) + buttons.length) % buttons.length;
    e.preventDefault();
    buttons[next].focus();
    buttons[next].click();
  };

  return (
    <section className="od-pane" aria-label={`${c.kind} ${c.id}`} style={{ height, flex: 'none', borderTop: '1px solid var(--border)' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '0 8px 0 12px', minHeight: 40, borderBottom: '1px solid var(--border-subtle)' }}>
        <span className="od-node__kind"><Icon name={KIND_ICONS[c.kind] ?? 'node-processor'} size={14} />{c.kind}</span>
        <span className="od-ident__name">{c.id}</span>
        {node.pipeline && <span className="od-muted" style={{ fontSize: 12 }}>in {node.pipeline}</span>}
        <span className={`od-mode od-mode--${mode.cls}`} title={c.note}><Icon name={mode.icon} size={12} />{mode.label}</span>
        <span style={{ flex: 1 }} />
        {!runId && onOpenYaml && (
          <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={onOpenYaml}><Icon name="file-code" size={14} /><span>Open in YAML</span></button>
        )}
        <button
          type="button"
          className="od-btn od-btn--ghost od-btn--sm od-btn--icon"
          aria-label={expanded ? 'Restore inspector' : 'Pop out inspector'}
          aria-pressed={expanded}
          onClick={() => setExpanded(!expanded)}
        >
          <Icon name="panel-right" size={14} />
        </button>
      </div>
      <div className="od-tabs" role="tablist" aria-label="Component views" onKeyDown={onKeyDown}>
        {sel.tabs.map((t) => (
          <button
            key={t.id}
            type="button"
            role="tab"
            id={`component-tab-${t.id}`}
            aria-controls="component-view"
            aria-selected={tab === t.id}
            tabIndex={tab === t.id ? 0 : -1}
            className="od-tab"
            title={t.purpose}
            onClick={() => setSearch({ tab: t.id, view: undefined })}
          >
            {t.label}
            {t.id === 'ottl' && statements !== undefined && <span className="od-tab__count">{statements}</span>}
          </button>
        ))}
      </div>
      {followed && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '8px 12px', borderBottom: '1px solid var(--border-subtle)', flexWrap: 'wrap' }} role="status">
          <span className="od-chip od-chip--accent"><Icon name="crosshair" size={12} />Following “{followed.name || followed.id}”</span>
          <div className="od-path" aria-label="Observed path">
            {path.map((s, i) => (
              <span key={s.key} style={{ display: 'contents' }}>
                {i > 0 && <span className="od-path__sep">›</span>}
                <button
                  type="button"
                  className={`od-path__step${s.status === 'stopped' ? ' od-path__step--stop' : s.key === node.key ? ' od-path__step--current' : ''}`}
                  style={{ border: 0, cursor: 'pointer' }}
                  onClick={() => setSearch({ node: s.key, input: undefined, record: undefined, tab: undefined, view: undefined })}
                >
                  {s.status === 'stopped' && <Icon name="ban" size={12} />}
                  {s.label}
                </button>
              </span>
            ))}
          </div>
          <span className={`od-fate od-fate--${fateOf(followed) === 'dropped' ? 'dropped' : fateOf(followed) === 'delivered' ? 'delivered' : 'nooutput'}`}>{fateText(followed)}</span>
          <span className="od-prov od-prov--observed"><Icon name="eye" size={12} />Observed</span>
          <span style={{ flex: 1 }} />
          <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={() => setSearch({ trace: undefined })}><Icon name="x" size={14} /><span>Stop following</span></button>
        </div>
      )}
      <div className="od-pane__body inspector-body" role="tabpanel" id="component-view" aria-labelledby={`component-tab-${tab}`}>
        {!runId ? (
          tab === 'how' ? <HowItRunsView node={node} graph={inspection?.topology} /> : <ConfigurationView node={node} inspection={inspection} />
        ) : tab === 'component' ? (
          <ConfigurationView node={node} inspection={inspection} runId={runId} />
        ) : !view.run ? (
          <div className="od-muted" style={{ padding: 14 }} role="status">{view.error ? view.error.message : 'Loading run…'}</div>
        ) : tab === 'activity' ? (
          <>
            {c.mode === 'simulated' && (
              <div className="od-callout od-callout--warn" role="status" style={{ margin: 12 }}>
                <Icon name="triangle-alert" size={16} />
                <div className="od-callout__main">
                  <div className="od-callout__title">Endpoint is simulated</div>
                  <div className="od-callout__body">{c.note ?? 'The debugger simulated this extension’s endpoints.'} Do not treat this as proof of production behavior.</div>
                </div>
              </div>
            )}
            <ExtensionActivity node={node} events={view.events} report={report} live={view.live} />
          </>
        ) : tab === 'batches' ? (
          <BatchingPanel batching={batchingFor(report?.batching, c.id, node.pipeline)} live={view.live} />
        ) : tab === 'grouping' ? (
          <GroupingPanel grouping={groupingFor(report?.grouping, c.id, node.pipeline)} live={view.live} search={search} setSearch={setSearch} />
        ) : tab === 'filter' ? (
          <FilteringPanel filtering={filteringFor(report?.filtering, c.id, node.pipeline)} live={view.live} search={search} setSearch={setSearch} />
        ) : tab === 'decisions' ? (
          <SamplingPanel sampling={samplingFor(report?.sampling, c.id, node.pipeline)} lineage={report?.lineage} live={view.live} setSearch={setSearch} />
        ) : c.kind === 'exporter' && tab === 'telemetry' && !view.live && report && !view.events.some((e) => e.kind === 'exporter.captured' && e.component === c.id) ? (
          <NothingCaptured node={node} view={view} setSearch={setSearch} />
        ) : (
          <TelemetryInspector
            key={`${node.key}:${tab}`}
            runId={runId}
            node={node}
            events={view.events}
            stages={report?.stages}
            ottl={report?.ottl}
            lineage={report?.lineage}
            aggregations={report?.aggregations}
            routing={report?.routing}
            filtering={report?.filtering}
            live={view.live}
            search={{
              ...search,
              view: ['changes', 'ottl', 'routes'].includes(tab) ? tab
                : ['table', 'side', 'unified', 'tree', 'json'].includes(search.view ?? '') ? search.view : 'explore',
            }}
            setSearch={setSearch}
          />
        )}
      </div>
    </section>
  );
}

/**
 * NothingCaptured is an exporter's Data view when the run recorded no output
 * there. After a completed run that means nothing reached it, and the stages
 * ahead of it that recorded removing records are named; after an early end it
 * proves nothing, and says so.
 */
function NothingCaptured({ node, view, setSearch }: { node: LayoutNode; view: RunView; setSearch(patch: Partial<WorkspaceSearch>): void }) {
  const c = node.component;
  const report = view.run!.report!;
  const completed = report.result?.status === 'completed';
  const n = nothingReached(report, c);
  return (
    <div className="od-card" style={{ margin: 14, borderStyle: 'dashed' }}>
      <div className="od-empty">
        <div className="od-empty__glyph"><Icon name="circle-dashed" size={20} /></div>
        <div className="od-empty__title">{completed ? `No records reached ${c.id}` : `Nothing was recorded at ${c.id}`}</div>
        <div className="od-empty__body">
          {!completed
            ? 'The run ended early, so this does not show that records were dropped.'
            : n.dropped.length
              ? n.dropped.map((u) => `${u.component} recorded dropping ${u.removed} of ${u.received} records in ${u.pipeline}`).join('; ') + ', so nothing was exported.'
              : 'No stage ahead of it recorded dropping records, and nothing was captured here.'}
        </div>
        {completed && (n.dropped.length > 0 || n.trace) && (
          <div className="od-empty__actions">
            {n.dropped[0] && (
              <button type="button" className="od-btn" onClick={() => setSearch({ node: n.dropped[0].node, input: undefined, record: undefined, tab: undefined, view: undefined })}>
                <span>Open {n.dropped[0].component}</span><Icon name="arrow-right" size={14} />
              </button>
            )}
            {n.trace && (
              <button type="button" className="od-btn od-btn--ghost" onClick={() => setSearch({ trace: n.trace, input: undefined, record: undefined })}>
                <Icon name="crosshair" size={14} /><span>Follow a dropped record</span>
              </button>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
