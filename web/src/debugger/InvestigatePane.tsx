import { useEffect, useState, type KeyboardEvent } from 'react';
import type { WorkspaceSearch } from '../router';
import type { Inspection } from '../api/types';
import type { RunView } from '../runs/useRun';
import { ComparePanel } from '../runs/ComparePanel';
import { EventsList } from '../inspect/EventsList';
import { INVESTIGATE_TABS, RUN_TABS } from '../workspace/inspectorTabs';
import { RunOverview } from './RunOverview';
import { AssertionsTab } from './AssertionsTab';
import { assertionRows } from './overview';
import { RecordsTab } from './RecordsTab';
import { RecommendationsTab } from './RecommendationsTab';

/** Props of the Investigate state's left pane (Overview, Assertions, Records, Timeline, Recommendations). */
export interface InvestigatePaneProps {
  runId: string;
  view: RunView;
  inspection?: Inspection;
  search: WorkspaceSearch;
  setSearch(patch: Partial<WorkspaceSearch>): void;
}

const RUN_TAB_IDS = RUN_TABS.map((t) => t.id);

/**
 * InvestigatePane is the left pane of a selected run: whole-run views only.
 * Overview is the run's outcome and cards, Assertions lists what was checked, Records follows record
 * journeys, Timeline lists the recorded events and Recommendations lists the
 * report's suggestions; component views live in
 * the bottom pane. The URL's `tab` names one of them when no component tab is
 * chosen; the pane remembers its own tab while a component tab is.
 */
export function InvestigatePane({ runId, view, search, setSearch }: InvestigatePaneProps) {
  const urlTab = RUN_TAB_IDS.includes(search.tab ?? '') ? search.tab : undefined;
  const [tab, setTab] = useState(urlTab ?? 'run');
  useEffect(() => {
    if (urlTab) setTab(urlTab);
  }, [urlTab]);

  const records = view.run?.report?.lineage?.length;
  const recommendations = view.live ? undefined : view.run?.report?.recommendations?.length;
  const asr = assertionRows(view.live ? undefined : view.run?.report?.assertions);
  const assertionCount = asr.length;
  // A failing run counts its failures in red; otherwise the badge is the total.
  const assertionsFailed = asr.filter((a) => a.outcome === 'fail' || a.outcome === 'error').length;
  const choose = (id: string) => {
    setTab(id);
    // A component tab in the URL belongs to the bottom pane; leave it alone.
    if (!search.node || urlTab) setSearch({ tab: id, view: undefined });
  };
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
  const tabs = tab === 'compare' ? RUN_TABS : INVESTIGATE_TABS;

  return (
    <section className="od-pane" aria-label="Run" style={{ width: 380, flex: 'none', borderRight: '1px solid var(--border)' }}>
      <div className="od-tabs" role="tablist" aria-label="Whole run views" onKeyDown={onKeyDown}>
        {tabs.map((t) => (
          <button
            key={t.id}
            type="button"
            role="tab"
            id={`investigate-tab-${t.id}`}
            aria-controls="investigate-view"
            aria-selected={tab === t.id}
            tabIndex={tab === t.id ? 0 : -1}
            className="od-tab"
            title={t.purpose}
            onClick={() => choose(t.id)}
          >
            {t.label}
            {t.id === 'assertions' && assertionCount > 0 && (assertionsFailed
              ? <span className="od-tab__count od-tab__count--fail" title={`${assertionsFailed} of ${assertionCount} failed`}>{assertionsFailed}</span>
              : <span className="od-tab__count">{assertionCount}</span>)}
            {t.id === 'journey' && records !== undefined && <span className="od-tab__count">{records}</span>}
            {t.id === 'recommendations' && !!recommendations && <span className="od-tab__count">{recommendations}</span>}
          </button>
        ))}
      </div>
      <div className="od-pane__body" role="tabpanel" id="investigate-view" aria-labelledby={`investigate-tab-${tab}`}>
        {tab === 'assertions' ? (
          <AssertionsTab view={view} />
        ) : tab === 'journey' ? (
          <RecordsTab view={view} search={search} setSearch={setSearch} />
        ) : tab === 'events' ? (
          <EventsList events={view.events} onStep={(s) => setSearch({ node: s.node, input: s.input, record: undefined, tab: 'telemetry', view: undefined })} />
        ) : tab === 'recommendations' ? (
          <RecommendationsTab view={view} setSearch={setSearch} />
        ) : tab === 'compare' ? (
          <ComparePanel runId={runId} other={search.compare} onOther={(compare) => setSearch({ compare })} />
        ) : (
          <RunOverview view={view} setSearch={setSearch} />
        )}
      </div>
    </section>
  );
}
