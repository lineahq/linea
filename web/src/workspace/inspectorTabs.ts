import type { WorkspaceSearch } from '../router';
import type { TopologyComponent } from '../api/types';

export interface InspectorTab { id: string; label: string; purpose: string }
export const RUN_TABS: InspectorTab[] = [
  { id: 'run', label: 'Overview', purpose: 'Run outcome, captured totals, checks and evidence limits across all pipelines.' },
  { id: 'journey', label: 'Records', purpose: 'Where records went across the whole run. Open a record to follow its path.' },
  { id: 'events', label: 'Timeline', purpose: 'Recorded execution order across all components. Open an event to inspect its payload.' },
  { id: 'compare', label: 'Compare runs', purpose: 'Compare this run with another run: the outcome, each exporter’s records and the injected records that ended or changed differently.' },
];
/** The three views of the Investigate left pane; Compare runs is reached from the run header. */
export const INVESTIGATE_TABS: InspectorTab[] = RUN_TABS.filter((t) => t.id !== 'compare');

/** Before any run the component's views explain what the next run will do. */
export const PREPARE_TABS: InspectorTab[] = [
  { id: 'how', label: 'How it will run', purpose: 'How the debugger will execute this component in the next run: real code, mocked environment, captured output or a stand-in.' },
  { id: 'component', label: 'Effective configuration', purpose: 'This component’s configuration with defaults applied and secrets redacted, as the next run will see it.' },
];

export function componentTabs(component: Pick<TopologyComponent, 'kind' | 'type'>): InspectorTab[] {
  const tabs = [
    { id: 'telemetry', label: 'Data', purpose: 'Browse the actual recorded signals at this component. Expand a row for its fields.' },
  ];
  if (component.kind === 'processor') {
    tabs.splice(0, 1, { id: 'changes', label: 'Changes', purpose: 'Resource and record attributes before and after this component, with changed values highlighted.' });
    if (component.type === 'transform') tabs.push({ id: 'ottl', label: 'Statements', purpose: 'How individual OTTL statements produced the observed changes. Replay verification is shown separately.' });
    // tail_sampling opens on its decisions (owner decision 2026-10-23); Changes only reconciles the buffer.
    // batch opens on its sends: Changes shows no attribute change for it.
    if (component.type === 'batch') tabs.unshift({ id: 'batches', label: 'Batches', purpose: 'When the processor sent what it held and why: size, timer, split or shutdown, and how long records waited.' });
    // groupbyattrs opens on its regrouping: Changes cannot attribute resource changes across merged or split blocks.
    if (component.type === 'groupbyattrs') tabs.unshift({ id: 'grouping', label: 'Grouping', purpose: 'Which resource blocks came in and went out, the attributes moved from records to resources and where each record went.' });
    if (component.type === 'tail_sampling') tabs.unshift({ id: 'decisions', label: 'Decisions', purpose: 'Which traces were kept or dropped, the policy that decided each one and what every policy found in the trace.' });
  }
  if (component.kind === 'extension') {
    tabs.splice(0, 1,
      { id: 'activity', label: 'Activity', purpose: 'What this extension did: when it started and stopped, what it logged, the metrics it recorded and its simulated endpoints.' },
      { id: 'telemetry', label: 'Data', purpose: 'What this extension sent to its simulated endpoints. Expand a row for its fields.' });
  }
  // A connector opens on what it does with records; its input and output data follow.
  if (component.kind === 'connector') tabs.unshift(component.type === 'count'
    ? { id: 'routes', label: 'Aggregation', purpose: 'Which metric points this input contributed to, with source-record attribution and verification.' }
    : { id: 'routes', label: 'Routing', purpose: 'Which downstream pipelines received each record in this input, including copies and unrouted records.' });
  tabs.push({ id: 'component', label: 'Configuration', purpose: 'This component’s execution mode and effective configuration, with defaults applied and secrets redacted.' });
  return tabs;
}

/**
 * componentSelection is the view of a selected component: always a component
 * view, whatever whole-run tab the URL names. Bookmarks that name a telemetry
 * view resolve to its tab, and a tab that does not fit the component falls
 * back to its first. Without a run the views explain the next run.
 */
export function componentSelection(search: Pick<WorkspaceSearch, 'tab' | 'view' | 'run'>, component: Pick<TopologyComponent, 'kind' | 'type'>) {
  let id = search.tab;
  if (id === 'telemetry' && ['changes', 'ottl', 'routes'].includes(search.view ?? '')) id = search.view;
  if (component.kind === 'processor' && id === 'telemetry') id = 'changes';
  const tabs = search.run ? componentTabs(component) : PREPARE_TABS;
  return { scope: 'component' as const, tabs, tab: tabs.find((t) => t.id === id) ?? tabs[0] };
}

/** Preserve existing bookmarked views, while rejecting tabs that do not fit a component. */
export function inspectorSelection(search: WorkspaceSearch, component?: Pick<TopologyComponent, 'kind' | 'type'>) {
  if (component && !RUN_TABS.some((t) => t.id === search.tab)) return componentSelection(search, component);
  const tab = RUN_TABS.find((t) => t.id === search.tab) ?? RUN_TABS[0];
  return { scope: 'run' as const, tabs: RUN_TABS, tab };
}
