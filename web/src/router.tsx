import { createRootRoute, createRoute, createRouter, redirect } from '@tanstack/react-router';
import { parseNewCaptureSearch } from './capture/newCaptureSearch';
import { Debugger } from './debugger/Debugger';
import { AppShell } from './ui/AppShell';
import { RunListPage } from './debugger/RunListPage';
import { RunStoragePage } from './debugger/RunStoragePage';
import { parseRunListSearch } from './debugger/runListSearch';
import { CapturesPage } from './capture/pages/CapturesPage';
import { CaptureDetailPage } from './capture/pages/CaptureDetailPage';
import { NewCapturePage } from './capture/pages/NewCapturePage';
import { GroupsPage } from './fleet/pages/GroupsPage';
import { AgentsPage } from './fleet/pages/AgentsPage';
import { ConfigsPage } from './fleet/pages/ConfigsPage';
import { ConfigDetailPage } from './fleet/pages/ConfigDetailPage';
import { AssignmentsPage, parseAssignmentsSearch } from './fleet/pages/AssignmentsPage';
import { RolloutDetailPage, RolloutsPage } from './fleet/pages/RolloutsPage';
import { EventsPage } from './fleet/pages/EventsPage';
import { RuntimesPage } from './runtimes/pages/RuntimesPage';
import { BuildPage } from './runtimes/pages/BuildPage';
import { UpgradeCheckPage } from './runtimes/pages/UpgradeCheckPage';
import { ConfigBisectPage } from './fleet/pages/ConfigBisectPage';
import { parseBisectSearch } from './fleet/bisect';
import { StoragePage } from './runtimes/pages/StoragePage';
import { SavedWorkPage } from './saved/SavedWorkPage';
import { SourceDetailPage } from './saved/SourceDetailPage';
import { SuiteListPage } from './saved/SuiteListPage';
import { AuditPage } from './audit/AuditPage';
import { AccessPage } from './access/AccessPage';
import { parseAccessSearch } from './access/accessView';
import { ProjectsPage } from './access/ProjectsPage';
import { LibraryPage } from './saved/LibraryPage';
import { SuiteResultPage } from './saved/SuiteResultPage';
import { PlaygroundPage } from './playground/PlaygroundPage';

/**
 * Selection lives in the URL so a view can be reloaded or shared locally:
 * the run, the graph node, the stage input, the inspector tab, the telemetry
 * view, the selected record, the record journey being followed and the run
 * compared with.
 */
export interface WorkspaceSearch {
  run?: string;
  node?: string;
  input?: number;
  tab?: string;
  /** Telemetry view: explore, changes, table, ottl, routes, side, unified, tree or json. */
  view?: string;
  /** Selected record (a diff entity name). */
  record?: string;
  /** Followed record journey (a lineage ID), highlighted on the graph. */
  trace?: string;
  /**
   * Input pane tab (see workspace/editorTabs.ts). Older links name the
   * scenario form (form) and its YAML (scenario).
   */
  editor?: string;
  /** The run the selected run is compared with (the Compare view). */
  compare?: string;
  /** Show the debugger's start state even though a draft is loaded. */
  start?: boolean;
}

/**
 * Seven destinations share the rail (ui/AppShell): the debugger (/ and the
 * full run list), the OTTL playground (/playground), Live capture
 * (/captures), Fleet (/fleet), Runtimes, Saved work, the Audit log and Access.
 * Every durable object has its own URL.
 */
const rootRoute = createRootRoute({ component: AppShell });

export const workspaceRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  component: Debugger,
  validateSearch: (s: Record<string, unknown>): WorkspaceSearch => ({
    run: typeof s.run === 'string' ? s.run : undefined,
    node: typeof s.node === 'string' ? s.node : undefined,
    input: typeof s.input === 'number' ? s.input : undefined,
    tab: typeof s.tab === 'string' ? s.tab : undefined,
    view: typeof s.view === 'string' ? s.view : undefined,
    record: typeof s.record === 'string' ? s.record : undefined,
    trace: typeof s.trace === 'string' ? s.trace : undefined,
    editor: typeof s.editor === 'string' ? s.editor : undefined,
    compare: typeof s.compare === 'string' ? s.compare : undefined,
    start: s.start === true || s.start === 'true' ? true : undefined,
  }),
});

export const runListRoute = createRoute({ getParentRoute: () => rootRoute, path: '/runs', component: RunListPage, validateSearch: parseRunListSearch });
export const runStorageRoute = createRoute({ getParentRoute: () => rootRoute, path: '/runs/storage', component: RunStoragePage });
export const playgroundRoute = createRoute({ getParentRoute: () => rootRoute, path: '/playground', component: PlaygroundPage });
export const capturesRoute = createRoute({ getParentRoute: () => rootRoute, path: '/captures', component: CapturesPage });
export const captureDetailRoute = createRoute({ getParentRoute: () => rootRoute, path: '/captures/$captureId', component: CaptureDetailPage });
export const newCaptureRoute = createRoute({ getParentRoute: () => rootRoute, path: '/captures/new', component: NewCapturePage, validateSearch: parseNewCaptureSearch });
export const fleetRoute = createRoute({ getParentRoute: () => rootRoute, path: '/fleet', component: AgentsPage });
export const groupsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/fleet/groups', component: GroupsPage });
export const configsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/fleet/configs', component: ConfigsPage });
export const configDetailRoute = createRoute({ getParentRoute: () => rootRoute, path: '/fleet/configs/$configId', component: ConfigDetailPage });
export const assignmentsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/fleet/assignments', component: AssignmentsPage, validateSearch: parseAssignmentsSearch });
export const rolloutsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/fleet/rollouts', component: RolloutsPage });
export const rolloutDetailRoute = createRoute({ getParentRoute: () => rootRoute, path: '/fleet/rollouts/$rolloutId', component: RolloutDetailPage });
export const eventsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/fleet/events', component: EventsPage });
export const bisectRoute = createRoute({ getParentRoute: () => rootRoute, path: '/fleet/bisect', component: ConfigBisectPage, validateSearch: parseBisectSearch });

/** Live capture's old paths redirect, keeping their search. */
const moved = (from: string, to: string) =>
  createRoute({
    getParentRoute: () => rootRoute,
    path: from,
    beforeLoad: ({ search, params }) => {
      const id = (params as { captureId?: string }).captureId;
      throw redirect({ to: id ? to.replace('$captureId', id) : to, search: search as never, replace: true });
    },
  });
const movedRoutes = [
  moved('/collectors', '/fleet'),
  moved('/collectors/groups', '/fleet/groups'),
  moved('/collectors/bisect', '/fleet/bisect'),
  moved('/collectors/new', '/captures/new'),
  moved('/collectors/captures', '/captures'),
  moved('/collectors/captures/$captureId', '/captures/$captureId'),
];
/** Runtime pages may carry `from`, the in-app location to return to (runtimes/pages/returnPath.ts). */
const returnSearch = (s: Record<string, unknown>): { from?: string } => ({ from: typeof s.from === 'string' ? s.from : undefined });
/** /runtimes also takes `rebuild`, the build whose manifest and name seed the builder. */
const runtimesSearch = (s: Record<string, unknown>): { from?: string; rebuild?: string } => ({ ...returnSearch(s), rebuild: typeof s.rebuild === 'string' ? s.rebuild : undefined });
export const runtimesRoute = createRoute({ getParentRoute: () => rootRoute, path: '/runtimes', component: RuntimesPage, validateSearch: runtimesSearch });
export const buildRoute = createRoute({ getParentRoute: () => rootRoute, path: '/runtimes/builds/$buildId', component: BuildPage, validateSearch: returnSearch });
export const upgradeRoute = createRoute({ getParentRoute: () => rootRoute, path: '/runtimes/upgrade', component: UpgradeCheckPage });
export const storageRoute = createRoute({ getParentRoute: () => rootRoute, path: '/runtimes/storage', component: StoragePage });
/** Saved work's kind filter and search; filter changes replace the history entry. */
export interface SavedSearch { kind?: string; q?: string }
export const savedRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/saved',
  component: SavedWorkPage,
  validateSearch: (s: Record<string, unknown>): SavedSearch => ({
    kind: typeof s.kind === 'string' ? s.kind : undefined,
    q: typeof s.q === 'string' && s.q ? s.q : undefined,
  }),
});
export const libraryRoute = createRoute({ getParentRoute: () => rootRoute, path: '/saved/library', component: LibraryPage });
export const suiteListRoute = createRoute({ getParentRoute: () => rootRoute, path: '/saved/suites', component: SuiteListPage });
export const suiteResultRoute = createRoute({ getParentRoute: () => rootRoute, path: '/saved/suites/$suiteId', component: SuiteResultPage });
export const sourceDetailRoute = createRoute({ getParentRoute: () => rootRoute, path: '/saved/$artifactId', component: SourceDetailPage });
export const auditRoute = createRoute({ getParentRoute: () => rootRoute, path: '/audit', component: AuditPage });
export const accessRoute = createRoute({ getParentRoute: () => rootRoute, path: '/access', component: AccessPage, validateSearch: parseAccessSearch });
export const projectsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/access/projects', component: ProjectsPage });

export const router = createRouter({
  routeTree: rootRoute.addChildren([
    workspaceRoute, runListRoute, runStorageRoute, playgroundRoute,
    capturesRoute, captureDetailRoute, newCaptureRoute,
    fleetRoute, groupsRoute, configsRoute, configDetailRoute, assignmentsRoute, rolloutsRoute, rolloutDetailRoute, eventsRoute, bisectRoute,
    ...movedRoutes,
    runtimesRoute, buildRoute, upgradeRoute, storageRoute,
    savedRoute, libraryRoute, suiteListRoute, suiteResultRoute, sourceDetailRoute,
    auditRoute, accessRoute, projectsRoute,
  ]),
});

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router;
  }
}
