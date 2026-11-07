import type { CollectorGroup, CollectorGroupPage, CollectorGroupMembersPage, CaptureMetadataUpdate } from './types';
import type { ComparisonRequest, ComparisonRuns, Evaluation, EvaluationInfo, RecommendationVerification, StatementTest, StatementTestRequest } from './types';
import type {
  CaptureStream, CaptureRecordsPage, CaptureDeriveRequest, CaptureFixture, CaptureScenarioRequest, CaptureScenario, CaptureExportRequest, CaptureExport,
  CaptureSelection,
  CaptureValidation,
  CaptureSession,
  CaptureSessionPage,
  Collector,
  CollectorPage,
  CapturePointPage,
  ArtifactMetadata,
  ArtifactPage,
  ArtifactRevisionData,
  ArtifactRevisionPage,
  ArtifactImport,
  AssertionReport,
  AssertionsRequest,
  Candidate,
  CandidateRequest,
  ConfigGraph,
  Inspection,
  InspectRequest,
  OTTLEvaluation,
  OTTLRequest,
  Report,
  Request,
  Run,
  RunComparison,
  RunInputs,
  RunLog,
  Sample,
  ScenarioFromRun,
  ScenarioFromRunRequest,
  ScenarioExport,
  Suite,
  SuiteRequest,
  SuiteStatus,
  TestReport,
  ExportRequest,
  SavedArtifact,
  SaveRequest,
  CollectorCatalog,
  CollectorSupport,
  ComposedManifest,
  UpgradeCheck,
  UpgradeCheckRequest,
  Bisection,
  ConfigBisectRequest,
  ManifestRequest,
  ManifestRetargetRequest,
  RetargetedManifest,
  Runtime,
  RuntimeBuild,
  RuntimeBuildPage,
  RuntimeBuildRequest,
  RuntimeDefault,
  RuntimePruneRequest,
  RuntimePruneResult,
  RuntimeStorage,
  RunPruneRequest,
  RunPruneResult,
  StorageUsage,
} from './types';
import type {
  CollectorGroupSave, FleetAgentConfig, FleetAgentDetail, FleetAgentPage, FleetAssignment, FleetAssignmentCreate, FleetAssignmentPage, FleetAssignmentPreviewPage,
  FleetConfig, FleetConfigCreate, FleetConfigPage, FleetConfigUpdate, FleetConfigVersion, FleetConfigVersionCreate, FleetConfigVersionPage,
  FleetEventPage, FleetOverview, FleetRollout, FleetRolloutPage, FleetRolloutStart, FleetRolloutTargetPage,
} from './types';
import type { AuditPage, RoleBindingRequest, RoleBindings, Session } from './types';
import type { CopyRequest, LibraryPage, ProjectRequest, Projects, SavedArtifact as Copied, Sharing } from './types';
import { withProject } from '../ui/project';
import type { components, operations } from './schema';

/** FleetAgentQuery is the agent list's fleet filters. */
export interface FleetAgentQuery { cursor?: string; connected?: boolean; healthy?: boolean; selector?: string; limit?: number }

/** query builds a query string from the set values. */
function query(values: Record<string, string | number | boolean | undefined>): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(values)) if (v !== undefined && v !== '') q.set(k, String(v));
  return q.size ? `?${q}` : '';
}

const fleet = (...parts: (string | number)[]) => ['/api/v1/fleet', ...parts.map((p) => encodeURIComponent(String(p)))].join('/');

/** RunQuery is the run list's server filters (scenario and runtime names, a result) and its paging (limit, cursor). */
export type RunQuery = NonNullable<operations['listRuns']['parameters']['query']>;

/** RunPage is one page of the run list, newest first; `next_cursor` is absent on the last page. */
export type RunPage = components['schemas']['RunPage'];

/** ApiError carries the API's {error} message, HTTP status and error body. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
    readonly body?: unknown,
  ) {
    super(message);
  }
}

// csrfToken is the web session's CSRF token from GET /api/v1/session, sent
// as X-CSRF-Token on requests that change something; empty without one.
let csrfToken = '';

/** setCSRFToken keeps the token a web session's requests send. */
export function setCSRFToken(token: string | undefined) {
  csrfToken = token ?? '';
}

// workProject is the project requests work in, sent as ?project= on every
// request; empty sends none, and the server uses the default project.
let workProject = '';

/** setWorkProject makes later requests work in project. */
export function setWorkProject(project: string) {
  workProject = project;
}

/** headers are a request's headers: JSON for a body, the CSRF token for a change. */
function headers(method: string, json: boolean): Record<string, string> {
  const h: Record<string, string> = json ? { 'Content-Type': 'application/json' } : {};
  if (method !== 'GET' && csrfToken) h['X-CSRF-Token'] = csrfToken;
  return h;
}

/** BuildQuery pages the build list. */
export interface BuildQuery { limit?: number; cursor?: string }

/** AuditQuery is the audit log's filters. */
export type AuditQuery = NonNullable<operations['listAuditEvents']['parameters']['query']>;

async function call<T>(method: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE', path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  // The API requires JSON on every JSON mutation (see internal/debugger/api ServeHTTP).
  const json = method === 'POST' || method === 'PUT' || method === 'PATCH';
  const init: RequestInit = { method, signal, headers: headers(method, json) };
  if (json) init.body = JSON.stringify(body ?? {});
  return send<T>(path, init);
}

/** send runs a prepared request and reads the API's JSON reply, or its {error}. */
async function send<T>(path: string, init: RequestInit): Promise<T> {
  const resp = await fetch(withProject(path, workProject), init);
  const text = await resp.text();
  let data: unknown;
  try {
    data = text ? JSON.parse(text) : undefined;
  } catch {
    throw new ApiError(resp.status, text || resp.statusText);
  }
  if (!resp.ok) {
    const msg = (data as { error?: string } | undefined)?.error ?? resp.statusText;
    throw new ApiError(resp.status, msg, data);
  }
  return data as T;
}

/** postForBlob sends a JSON mutation whose success is a file, not JSON. */
async function postForBlob(path: string, body: unknown): Promise<Blob> {
  const resp = await fetch(withProject(path, workProject), { method: 'POST', headers: headers('POST', true), body: JSON.stringify(body) });
  if (resp.ok) return resp.blob();
  const text = await resp.text();
  let data: unknown;
  try {
    data = text ? JSON.parse(text) : undefined;
  } catch {
    throw new ApiError(resp.status, text || resp.statusText);
  }
  throw new ApiError(resp.status, (data as { error?: string } | undefined)?.error ?? resp.statusText, data);
}

export const api = {
  /** session names the signed-in principal and its permissions; 401 when nobody is signed in. */
  session: () => call<Session>('GET', '/api/v1/session'),
  /** signOut ends the web session; the server needs the session's CSRF token. */
  signOut: () => call<void>('POST', '/auth/logout'),
  auditEvents: (q: AuditQuery = {}) => call<AuditPage>('GET', '/api/v1/audit-events' + query({ ...q })),
  roleBindings: () => call<RoleBindings>('GET', '/api/v1/role-bindings'),
  /** addRoleBinding grants a role at the managed bindings' revision; 409 when it moved. */
  addRoleBinding: (req: RoleBindingRequest) => call<RoleBindings>('POST', '/api/v1/role-bindings', req),
  removeRoleBinding: (id: string, revision: number) => call<RoleBindings>('DELETE', `/api/v1/role-bindings/${encodeURIComponent(id)}` + query({ revision })),
  fleetOverview: (groupBy = '') => call<FleetOverview>('GET', fleet() + query({ group_by: groupBy })),
  fleetAgents: (q: FleetAgentQuery = {}) => call<FleetAgentPage>('GET', fleet('agents') + query({ ...q })),
  fleetAgent: (id: string) => call<FleetAgentDetail>('GET', fleet('agents', id)),
  retryFleetAgentConfig: (id: string) => call<FleetAgentConfig>('POST', fleet('agents', id, 'config-retry')),
  fleetEvents: (cursor = '', limit?: number) => call<FleetEventPage>('GET', fleet('events') + query({ cursor, limit })),
  /** saveCollectorGroup creates a group without an ID, or replaces it at its revision. */
  saveCollectorGroup: ({ id, ...req }: CollectorGroupSave) =>
    id ? call<CollectorGroup>('PUT', `/api/v1/collector-groups/${encodeURIComponent(id)}`, req) : call<CollectorGroup>('POST', '/api/v1/collector-groups', req),
  deleteCollectorGroup: (id: string, revision: number) => call<{ deleted: boolean }>('DELETE', `/api/v1/collector-groups/${encodeURIComponent(id)}?revision=${revision}`),
  fleetConfigs: (cursor = '') => call<FleetConfigPage>('GET', fleet('configs') + query({ cursor })),
  fleetConfig: (id: string) => call<FleetConfig>('GET', fleet('configs', id)),
  createFleetConfig: (req: FleetConfigCreate) => call<FleetConfig>('POST', fleet('configs'), req),
  updateFleetConfig: (id: string, req: FleetConfigUpdate) => call<FleetConfig>('PATCH', fleet('configs', id), req),
  deleteFleetConfig: (id: string, revision: number) => call<{ deleted: boolean }>('DELETE', fleet('configs', id) + query({ revision })),
  fleetConfigVersions: (id: string, cursor = '') => call<FleetConfigVersionPage>('GET', fleet('configs', id, 'versions') + query({ cursor })),
  fleetConfigVersion: (id: string, version: number) => call<FleetConfigVersion>('GET', fleet('configs', id, 'versions', version)),
  createFleetConfigVersion: (id: string, req: FleetConfigVersionCreate) => call<FleetConfigVersion>('POST', fleet('configs', id, 'versions'), req),
  fleetAssignments: (cursor = '') => call<FleetAssignmentPage>('GET', fleet('assignments') + query({ cursor })),
  fleetAssignment: (id: string) => call<FleetAssignment>('GET', fleet('assignments', id)),
  /** createFleetAssignment assigns a config version and starts the rollout that introduces it. */
  createFleetAssignment: (req: FleetAssignmentCreate) => call<FleetAssignment>('POST', fleet('assignments'), req),
  previewFleetAssignment: (id: string, cursor = '') => call<FleetAssignmentPreviewPage>('GET', fleet('assignments', id, 'preview') + query({ cursor })),
  fleetRollouts: (cursor = '') => call<FleetRolloutPage>('GET', fleet('rollouts') + query({ cursor })),
  fleetRollout: (id: string) => call<FleetRollout>('GET', fleet('rollouts', id)),
  fleetRolloutTargets: (id: string, outcome = '', cursor = '') => call<FleetRolloutTargetPage>('GET', fleet('rollouts', id, 'targets') + query({ outcome, cursor })),
  startFleetRollout: (req: FleetRolloutStart) => call<FleetRollout>('POST', fleet('rollouts'), req),
  controlFleetRollout: (id: string, action: 'pause' | 'resume' | 'rollback' | 'cancel') => call<FleetRollout>('POST', fleet('rollouts', id, 'actions'), { action }),
  collectorGroups: (cursor = '') => call<CollectorGroupPage>('GET', `/api/v1/collector-groups?cursor=${encodeURIComponent(cursor)}`),
  collectorGroup: (id: string) => call<CollectorGroup>('GET', `/api/v1/collector-groups/${encodeURIComponent(id)}`),
  collectorGroupMembers: (id: string, cursor = '') => call<CollectorGroupMembersPage>('GET', `/api/v1/collector-groups/${encodeURIComponent(id)}/members?cursor=${encodeURIComponent(cursor)}`),
  updateCaptureMetadata: (id: string, req: CaptureMetadataUpdate) => call<CaptureSession>('PATCH', `/api/v1/captures/${encodeURIComponent(id)}`, req),
  captureRecords: (id: string, cursor = '', stream?: CaptureStream, limit = 25, signal?: AbortSignal) => { const query = new URLSearchParams({ cursor, limit: String(limit), ...stream }); return call<CaptureRecordsPage>('GET', `/api/v1/captures/${encodeURIComponent(id)}/records?${query}`, undefined, signal); },
  captureFixture: (id: string, req: CaptureDeriveRequest, signal?: AbortSignal) => call<CaptureFixture>('POST', `/api/v1/captures/${encodeURIComponent(id)}/fixtures`, req, signal),
  captureScenario: (id: string, req: CaptureScenarioRequest) => call<CaptureScenario>('POST', `/api/v1/captures/${encodeURIComponent(id)}/scenario`, req),
  captureExport: (id: string, req: CaptureExportRequest) => call<CaptureExport>('POST', `/api/v1/captures/${encodeURIComponent(id)}/export`, req),
  deleteCapture: (id: string) => call<{ deleted: boolean }>('DELETE', `/api/v1/captures/${encodeURIComponent(id)}`),
  validateCapture: (selection: CaptureSelection) => call<CaptureValidation>('POST', '/api/v1/captures/validate', selection),
  createCapture: (selection: CaptureSelection) => call<CaptureSession>('POST', '/api/v1/captures', selection),
  captures: (cursor = '') => call<CaptureSessionPage>('GET', `/api/v1/captures?cursor=${encodeURIComponent(cursor)}`),
  capture: (id: string, wait = false) => call<CaptureSession>('GET', `/api/v1/captures/${encodeURIComponent(id)}${wait ? '?wait=true' : ''}`),
  stopCapture: (id: string) => call<CaptureSession>('POST', `/api/v1/captures/${encodeURIComponent(id)}/stop`),
  collectors: (cursor = '') => call<CollectorPage>('GET', `/api/v1/collectors?cursor=${encodeURIComponent(cursor)}`),
  collector: (id: string) => call<Collector>('GET', `/api/v1/collectors/${encodeURIComponent(id)}`),
  capturePoints: (id: string, cursor = '') => call<CapturePointPage>('GET', `/api/v1/collectors/${encodeURIComponent(id)}/capture-points?cursor=${encodeURIComponent(cursor)}`),
  samples: () => call<Sample[]>('GET', '/api/v1/samples'),
  validate: (req: InspectRequest, signal?: AbortSignal) =>
    call<Inspection>('POST', '/api/v1/config/validate', req, signal),
  graph: (req: InspectRequest, signal?: AbortSignal) => call<ConfigGraph>('POST', '/api/v1/config/graph', req, signal),
  evaluateOTTL: (req: OTTLRequest, signal?: AbortSignal) =>
    call<OTTLEvaluation>('POST', '/api/v1/ottl/evaluate', req, signal),
  createRun: (req: Request) => call<Run>('POST', '/api/v1/runs', req),
  /**
   * listRuns lists one page of runs newest first with each finished run's summary; the filters are exact
   * matches. `limit` and `cursor` page it: pass the previous page's `next_cursor` with the same filters.
   */
  listRuns: (filter: RunQuery = {}) => {
    const query = new URLSearchParams(Object.entries(filter).filter(([, v]) => v !== undefined && v !== '').map(([k, v]) => [k, String(v)]));
    return call<RunPage>('GET', `/api/v1/runs${query.size ? `?${query}` : ''}`);
  },
  suites: () => call<Suite[]>('GET', '/api/v1/suites'),
  /** submitSuite queues 1 to 32 scenarios against one configuration (202). */
  submitSuite: (req: SuiteRequest) => call<Suite>('POST', '/api/v1/suites', req),
  /** suite reads a suite's members with their states and outcomes. */
  suite: (id: string) => call<SuiteStatus>('GET', `/api/v1/suites/${encodeURIComponent(id)}`),
  /** suiteReport reads a finished suite's test report (409 while members run). */
  suiteReport: (id: string) => call<TestReport>('GET', `/api/v1/suites/${encodeURIComponent(id)}/report`),
  /** rerunSuite queues a new suite from a finished suite's recorded inputs (202). */
  rerunSuite: (id: string) => call<Suite>('POST', `/api/v1/suites/${encodeURIComponent(id)}/rerun`),
  run: (id: string) => call<Run>('GET', `/api/v1/runs/${encodeURIComponent(id)}`),
  compareRuns: (before: string, after: string) =>
    call<RunComparison>('GET', `/api/v1/runs/${encodeURIComponent(before)}/compare/${encodeURIComponent(after)}`),
  runInputs: (id: string) => call<RunInputs>('GET', `/api/v1/runs/${encodeURIComponent(id)}/inputs`),
  runLog: (id: string) => call<RunLog>('GET', `/api/v1/runs/${encodeURIComponent(id)}/log`),
  /** submitComparison runs two configurations on one input; both runs are admitted or neither (202). */
  submitComparison: (req: ComparisonRequest) => call<ComparisonRuns>('POST', '/api/v1/comparisons', req),
  /** testStatementOnRun runs an edited statement on a run's recorded inputs (202 queued, 200 for an invalid candidate). */
  testStatementOnRun: (id: string, req: StatementTestRequest) =>
    call<StatementTest>('POST', `/api/v1/runs/${encodeURIComponent(id)}/statements/test`, req),
  /**
   * verifyRecommendation runs recommendation n (its position in the report)
   * of a finished run as a candidate on the run's inputs and waits for the
   * comparison (200); one more run. Swaps, removed statements or conditions
   * and options with a value verify.
   */
  verifyRecommendation: (id: string, n: number) =>
    call<RecommendationVerification>('POST', `/api/v1/runs/${encodeURIComponent(id)}/recommendations/${n}/verify`, {}),
  /** runEvaluations lists the assertion evaluations recorded beside a run (by MCP), oldest first, without results. */
  runEvaluations: (id: string) => call<EvaluationInfo[]>('GET', `/api/v1/runs/${encodeURIComponent(id)}/evaluations`),
  runEvaluation: (id: string, evaluation: string) =>
    call<Evaluation>('GET', `/api/v1/runs/${encodeURIComponent(id)}/evaluations/${encodeURIComponent(evaluation)}`),
  /** artifacts lists one page of saved artifacts of a kind ('' for every kind), 25 by default and at most 100 per page. */
  artifacts: (kind: string, cursor = '', limit?: number) =>
    call<ArtifactPage>('GET', `/api/v1/artifacts?kind=${encodeURIComponent(kind)}${limit ? `&limit=${limit}` : ''}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`),
  artifact: (id: string) => call<ArtifactMetadata>('GET', `/api/v1/artifacts/${encodeURIComponent(id)}`),
  artifactRevision: (id: string, revision: string) =>
    call<ArtifactRevisionData>('GET', `/api/v1/artifacts/${encodeURIComponent(id)}/revisions/${encodeURIComponent(revision)}`),
  /** artifactRevisions lists one page of an artifact's revision metadata, newest first. */
  artifactRevisions: (id: string, cursor = '', limit = 20) =>
    call<ArtifactRevisionPage>('GET', `/api/v1/artifacts/${encodeURIComponent(id)}/revisions?limit=${limit}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`),
  /** exportForCI builds the CI archive (zip) of a saved configuration and scenarios. */
  exportForCI: (configurationId: string, scenarioIds: string[]) =>
    postForBlob('/api/v1/artifacts/export', { configuration_id: configurationId, scenario_ids: scenarioIds }),
  /** importArtifacts saves a CI archive's files as new artifacts or revisions. */
  importArtifacts: (archiveBase64: string) => call<ArtifactImport>('POST', '/api/v1/artifacts/import', { archive_base64: archiveBase64 }),
  saveArtifact: (req: SaveRequest) => call<SavedArtifact>('POST', '/api/v1/artifacts', req),
  createCandidate: (req: CandidateRequest) => call<Candidate>('POST', '/api/v1/candidates', req),
  scenarioFromRun: (id: string, req: ScenarioFromRunRequest = {}) =>
    call<ScenarioFromRun>('POST', `/api/v1/runs/${encodeURIComponent(id)}/scenario`, req),
  exportScenario: (id: string, req: ExportRequest = {}) =>
    call<ScenarioExport>('POST', `/api/v1/runs/${encodeURIComponent(id)}/export`, req),
  report: (id: string) => call<Report>('GET', `/api/v1/runs/${encodeURIComponent(id)}/report`),
  cancelRun: (id: string) => call<Run>('POST', `/api/v1/runs/${encodeURIComponent(id)}/cancel`),
  deleteRun: (id: string) => call<void>('DELETE', `/api/v1/runs/${encodeURIComponent(id)}`),
  /**
   * keepRun protects a run from retention and prune. It is a PUT, so `call` sends Content-Type: application/json
   * and an empty `{}` body, which the server requires of every PUT. The run comes back without its report.
   */
  keepRun: (id: string) => call<Run>('PUT', `/api/v1/runs/${encodeURIComponent(id)}/keep`),
  /** unkeepRun makes a kept run subject to retention and prune again; the run comes back without `kept`. */
  unkeepRun: (id: string) => call<Run>('DELETE', `/api/v1/runs/${encodeURIComponent(id)}/keep`),
  /** storageUsage reads the run history's bytes against its limit, its run counts and the retention policy. */
  storageUsage: () => call<StorageUsage>('GET', '/api/v1/storage'),
  /** pruneRuns deletes the finished runs the filters select that retention does not keep; `dry_run` only counts them. Admins only. */
  pruneRuns: (req: RunPruneRequest) => call<RunPruneResult>('POST', '/api/v1/runs/prune', req),
  snapshot: (id: string, ref: string) =>
    call<unknown>('GET', `/api/v1/runs/${encodeURIComponent(id)}/snapshots/${encodeURIComponent(ref)}`),
  evaluateAssertions: (id: string, req: AssertionsRequest) =>
    call<AssertionReport>('POST', `/api/v1/runs/${encodeURIComponent(id)}/assertions`, req),
  runtimes: () => call<Runtime[]>('GET', '/api/v1/runtimes'),
  /** defaultRuntime reads the runtime requests that name none run on; `set` is absent until the team chooses one. */
  defaultRuntime: () => call<RuntimeDefault>('GET', '/api/v1/runtime-default'),
  /** setDefaultRuntime makes `built-in` or a catalog runtime the team's default (runtime.manage). */
  setDefaultRuntime: (name: string) => call<RuntimeDefault>('PUT', '/api/v1/runtime-default', { name }),
  deleteRuntime: (name: string) => call<void>('DELETE', `/api/v1/runtimes/${encodeURIComponent(name)}`),
  pushRuntime: (name: string, target: string) =>
    call<void>('POST', `/api/v1/runtimes/${encodeURIComponent(name)}/push`, { target }),
  importRuntime: (ref: string, pull: boolean) => call<Runtime>('POST', '/api/v1/runtimes/import', { ref, pull }),
  runtimeStorage: () => call<RuntimeStorage>('GET', '/api/v1/runtime-storage'),
  pruneRuntimes: (req: RuntimePruneRequest) => call<RuntimePruneResult>('POST', '/api/v1/runtime-storage/prune', req),
  collectorSupport: () => call<CollectorSupport>('GET', '/api/v1/collector-versions'),
  collectorCatalog: (version: string) =>
    call<CollectorCatalog>('GET', `/api/v1/collector-versions/${encodeURIComponent(version)}/catalog`),
  composeManifest: (req: ManifestRequest) => call<ComposedManifest>('POST', '/api/v1/manifests', req),
  /** checkUpgrade waits for the whole check; aborting it cancels the check's runs. */
  checkUpgrade: (req: UpgradeCheckRequest, signal?: AbortSignal) => call<UpgradeCheck>('POST', '/api/v1/upgrade-checks', req, signal),
  /** bisectConfig waits for the whole bisect; aborting it cancels the run it was waiting for. */
  bisectConfig: (req: ConfigBisectRequest, signal?: AbortSignal) => call<Bisection>('POST', '/api/v1/config-bisects', req, signal),
  eventsURL: (id: string) => withProject(`/api/v1/runs/${encodeURIComponent(id)}/events`, workProject),
  /** retargetManifest moves a manifest's core and contrib versions to a release; the rest of its text stays. */
  retargetManifest: (req: ManifestRetargetRequest) => call<RetargetedManifest>('POST', '/api/v1/manifests/retarget', req),
  /** runtimeBuilds pages the builds the caller may see, newest first and without their logs. */
  runtimeBuilds: (q: BuildQuery = {}) => call<RuntimeBuildPage>('GET', `/api/v1/runtime-builds${query({ limit: q.limit, cursor: q.cursor })}`),
  /** runtimeBuild reads a build with its log lines from `logFrom` on (the server keeps the last 2000). */
  runtimeBuild: (id: string, logFrom = 0) =>
    call<RuntimeBuild>('GET', `/api/v1/runtime-builds/${encodeURIComponent(id)}${query({ log_from: logFrom || undefined })}`),
  /** createRuntimeBuild queues a build whose manifest is text (or a saved revision) and that needs no uploaded sources. */
  createRuntimeBuild: (req: RuntimeBuildRequest) => call<RuntimeBuild>('POST', '/api/v1/runtime-builds', req),
  /**
   * uploadRuntimeBuild queues a build with local module sources: a multipart body whose "request" part is the
   * JSON request and whose other parts are the tar archives its sources name. The X-Linea-Upload header is what
   * the server requires of an upload (a cross-origin form post cannot send it); the browser sets the multipart
   * boundary itself.
   */
  uploadRuntimeBuild: (req: RuntimeBuildRequest, archives: Record<string, Blob>) => {
    const form = new FormData();
    form.append('request', new Blob([JSON.stringify(req)], { type: 'application/json' }), 'request.json');
    for (const [part, blob] of Object.entries(archives)) form.append(part, blob, `${part}.tar`);
    return send<RuntimeBuild>('/api/v1/runtime-builds', { method: 'POST', body: form, headers: { ...headers('POST', false), 'X-Linea-Upload': '1' } });
  },
  /** cancelRuntimeBuild stops a queued or running build; a finished one is unchanged. */
  cancelRuntimeBuild: (id: string) => call<RuntimeBuild>('POST', `/api/v1/runtime-builds/${encodeURIComponent(id)}/cancel`),
  /** buildEventsURL follows a build as server-sent events, its log from line `logFrom` on. */
  buildEventsURL: (id: string, logFrom = 0) =>
    withProject(`/api/v1/runtime-builds/${encodeURIComponent(id)}/events${query({ log_from: logFrom || undefined })}`, workProject),
  /** projects lists the projects the caller holds a permission in, with those permissions, and the library. */
  projects: () => call<Projects>('GET', '/api/v1/projects'),
  /** createProject adds a project at the project set's revision (admin); 409 when it moved. */
  createProject: (req: ProjectRequest) => call<Projects>('POST', '/api/v1/projects', req),
  /** updateProject replaces a project's name and settings (admin). */
  updateProject: (id: string, req: ProjectRequest) => call<Projects>('PUT', `/api/v1/projects/${encodeURIComponent(id)}`, req),
  /** setProjectArchived archives a project, which then takes no new work, or restores it (admin). */
  setProjectArchived: (id: string, archived: boolean, revision: number) =>
    call<Projects>('POST', `/api/v1/projects/${encodeURIComponent(id)}/${archived ? 'archive' : 'unarchive'}`, { revision }),
  /** library pages the library items shared with the current project; with project "library", everything to a publisher. */
  library: (q: { kind?: string; cursor?: string; limit?: number; project?: string } = {}) => call<LibraryPage>('GET', `/api/v1/library${query(q)}`),
  /** setLibraryAudience shares a library item with projects, or "*" alone (admin). */
  setLibraryAudience: (artifactId: string, projects: string[], revision: number) =>
    call<Sharing>('PUT', `/api/v1/library/${encodeURIComponent(artifactId)}/audience`, { revision, projects }),
  /** setRuntimeAvailability limits the projects a runtime runs work for; an empty list is every project (runtime.manage). */
  setRuntimeAvailability: (runtime: string, projects: string[], revision: number) =>
    call<Sharing>('PUT', `/api/v1/runtimes/${encodeURIComponent(runtime)}/availability`, { revision, projects }),
  /** copyArtifact copies a revision into the current project as a new artifact; into the library, it publishes it. */
  copyArtifact: (artifactId: string, req: CopyRequest = {}) => call<Copied>('POST', `/api/v1/artifacts/${encodeURIComponent(artifactId)}/copies`, req),
  /** copyArtifactTo copies into another project than the current one, such as the library. */
  copyArtifactTo: (artifactId: string, project: string, req: CopyRequest = {}) =>
    call<Copied>('POST', `/api/v1/artifacts/${encodeURIComponent(artifactId)}/copies?project=${encodeURIComponent(project)}`, req),
};
