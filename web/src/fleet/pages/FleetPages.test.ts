// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AgentsPage } from './AgentsPage';
import { AssignmentsPage, parseAssignmentsSearch } from './AssignmentsPage';
import { ConfigDetailPage } from './ConfigDetailPage';
import { ConfigsPage } from './ConfigsPage';
import { GroupsPage } from './GroupsPage';
import { RolloutDetailPage } from './RolloutsPage';

vi.mock('../../editor/YamlEditor', () => ({
  YamlEditor: ({ value, onChange, readOnly }: { value: string; onChange: (v: string) => void; readOnly?: boolean }) =>
    createElement('textarea', { 'aria-label': readOnly ? 'Version content' : 'Draft content', value, readOnly, onChange: (e: { target: { value: string } }) => onChange(e.target.value) }),
}));

const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status });
const A1 = 'a1'.repeat(16);
const A2 = 'a2'.repeat(16);
const C1 = 'c1'.repeat(16);
const G1 = 'd1'.repeat(16);
const R1 = 'e1'.repeat(16);
// Role permissions as GET /api/v1/session lists them (internal/debugger/authz).
const VIEWER = ['run.read', 'artifact.read', 'capture.read', 'fleet.read', 'runtime.read', 'token.own'];
const ENGINEER = [...VIEWER, 'run.submit', 'run.delete', 'artifact.write', 'capture.start'];
const overview = { enabled: true, config_management: true, total: 2, connected: 1, disconnected: 1 };

interface Call { method: string; url: string; body: unknown }
let calls: Call[];
/** routes answers "METHOD /path" exactly, ignoring the query string. */
let routes: Record<string, (call: Call) => Response>;
let host: HTMLDivElement, root: Root, client: QueryClient;

beforeEach(() => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
  vi.stubGlobal('scrollTo', () => undefined);
  calls = [];
  routes = { 'GET /api/v1/fleet': () => json(overview), 'GET /api/v1/collector-groups': () => json({ items: [] }) };
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    const call = { method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined };
    calls.push(call);
    const hit = routes[`${method} ${url.split('?')[0]}`];
    if (hit) return hit(call);
    throw new Error(`Unexpected request: ${method} ${url}`);
  }));
  client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  host = document.createElement('div');
  document.body.append(host);
  root = createRoot(host);
});
afterEach(() => {
  act(() => root.unmount());
  client.clear();
  host.remove();
  vi.unstubAllGlobals();
});

async function settle(until: () => boolean) {
  for (let i = 0; i < 200 && !until(); i++) await act(() => new Promise((r) => setTimeout(r, 5)));
  expect(until()).toBe(true);
}
async function render(entry: string) {
  const rootRoute = createRootRoute({ component: Outlet });
  const page = (path: string, component: () => unknown, validateSearch?: (s: Record<string, unknown>) => unknown) =>
    createRoute({ getParentRoute: () => rootRoute, path, component: component as never, validateSearch: validateSearch as never }) as never;
  const tree = rootRoute.addChildren([
    page('/fleet', AgentsPage),
    page('/fleet/groups', GroupsPage),
    page('/fleet/configs', ConfigsPage),
    page('/fleet/configs/$configId', ConfigDetailPage),
    page('/fleet/assignments', AssignmentsPage, parseAssignmentsSearch),
    page('/fleet/rollouts/$rolloutId', RolloutDetailPage),
    page('/captures/new', () => createElement('p', null, 'New capture')),
  ]);
  const router = createRouter({ routeTree: tree, history: createMemoryHistory({ initialEntries: [entry] }) });
  await act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(RouterProvider, { router: router as never }))));
  return router;
}
const text = () => document.body.textContent ?? '';
const button = (label: string) => [...document.querySelectorAll('button')].find((b) => b.textContent?.trim() === label) as HTMLButtonElement | undefined;
const click = async (el: Element | null | undefined) => act(async () => (el as HTMLElement).click());
const type = async (selector: string, value: string) => {
  const el = document.querySelector(selector) as HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement;
  const proto = el instanceof HTMLSelectElement ? HTMLSelectElement.prototype : el instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
  await act(async () => {
    Object.getOwnPropertyDescriptor(proto, 'value')!.set!.call(el, value);
    el.dispatchEvent(new Event(el instanceof HTMLSelectElement ? 'change' : 'input', { bubbles: true }));
  });
};
const sent = (method: string, path: string) => calls.filter((c) => c.method === method && c.url.split('?')[0] === path);

describe('Fleet', () => {
  it('says how to connect fleet when serve has no fleet', async () => {
    routes['GET /api/v1/fleet'] = () => json({ enabled: false, config_management: false, total: 0, connected: 0, disconnected: 0 });
    await render('/fleet');
    await settle(() => text().includes('Fleet is not connected'));
    expect(text()).toContain('--fleet-url');
  });
});

describe('Agents', () => {
  const agent = (id: string, patch: Record<string, unknown> = {}) => ({
    id, connected: true, connected_at: '2026-10-27T10:00:00Z', first_seen_at: '2026-10-20T10:00:00Z', identity_conflict: false,
    attributes: { 'service.name': id === A1 ? 'gateway' : 'edge' }, ...patch,
  });

  it('marks the agents capture can use, and retries an agent config', async () => {
    routes['GET /api/v1/fleet/agents'] = () => json({ items: [
      agent(A1, { collector_ids: ['gw-0'], health: { healthy: true, components_unhealthy: 0, observed_at: '' }, remote_config: { status: 'applied' } }),
      agent(A2, { connected: false, remote_config: { status: 'failed' } }),
    ] });
    routes[`GET /api/v1/fleet/agents/${A1}`] = () => json({ ...agent(A1, { collector_ids: ['gw-0'] }), capture_linked: true, config: { accepts_remote_config: true, credential_remote_config: true, in_sync: false } });
    routes['GET /api/v1/collectors/gw-0'] = () => json({ id: 'gw-0', generation: 1, agent_id: A1, credential_id: 'c', pod: 'gw-0', cluster: 'prod', namespace: 'obs', collector_group: '', enabled: true, point_count: 2, raw_point_count: 0 });
    routes[`POST /api/v1/fleet/agents/${A1}/config-retry`] = () => json({ accepts_remote_config: true, credential_remote_config: true, in_sync: true });
    await render('/fleet');
    await settle(() => !!document.querySelector('.od-table tbody tr'));
    const rows = [...document.querySelectorAll('.od-table tbody tr')].map((r) => r.textContent);
    expect(rows[0]).toContain('gateway');
    expect(rows[0]).toContain('Healthy');
    expect(rows[0]).toContain('1 Collector');
    expect(rows[1]).toContain('Offline');
    expect(rows[1]).toContain('Failed');

    await click(document.querySelector('button[aria-label="Show details of gateway"]'));
    await settle(() => text().includes('Retry config') && text().includes('Configure capture'));
    expect(document.querySelector('a[href^="/captures/new?collectors="]')).not.toBeNull();
    expect(document.querySelector(`a[href="/fleet/bisect?agent=${A1}"]`)).not.toBeNull();
    await click(button('Retry config'));
    await settle(() => text().includes('the agent is in sync'));
    expect(sent('POST', `/api/v1/fleet/agents/${A1}/config-retry`)).toHaveLength(1);
  });
});

describe('Groups', () => {
  const group = { id: G1, name: 'payments-gw', kind: 'selector', revision: 3, selector: 'env=prod' };
  const inventory = { enabled: true, items: [{ id: 'gw-0', generation: 1, agent_id: 'a1', credential_id: 'cred', pod: 'gw-0', cluster: 'prod', namespace: 'pay', collector_group: '', enabled: true, point_count: 1, raw_point_count: 0 }] };
  beforeEach(() => {
    routes['GET /api/v1/collector-groups'] = () => json({ items: [group, { id: 'g2', name: 'edge', kind: 'static', revision: 1, agent_ids: ['a9'] }] });
    routes[`GET /api/v1/collector-groups/${G1}/members`] = () => json({ group_id: G1, capture_linked: true, items: [{ agent_id: 'a1', connected: true, collector_ids: ['gw-0'] }, { agent_id: 'a2', connected: false }] });
    routes[`GET /api/v1/collector-groups/${G1}`] = () => json(group);
    routes['GET /api/v1/collectors'] = () => json(inventory);
  });

  it('shows a group with its members and a way to capture it', async () => {
    await render('/fleet/groups');
    await settle(() => (document.querySelector('.od-table tbody')?.textContent ?? '').includes('gw-0'));
    expect(text()).toContain('Agents matching env=prod');
    expect(text()).toContain('By selector · revision 3');
    const rows = [...document.querySelectorAll('.od-table tbody tr')].map((r) => r.textContent);
    expect(rows[0]).toContain('Connected');
    expect(rows[1]).toContain('Offline');
    expect(document.querySelector(`a[href="/captures/new?group=${G1}"]`)).not.toBeNull();
  });

  it('lists members without Collectors on a fleet without live capture', async () => {
    routes[`GET /api/v1/collector-groups/${G1}/members`] = () => json({ group_id: G1, capture_linked: false, items: [{ agent_id: 'a1', connected: true, collector_ids: [] }] });
    delete routes['GET /api/v1/collectors'];
    await render('/fleet/groups');
    await settle(() => text().includes('This fleet has no live capture'));
    expect(text()).not.toContain('No Collector with the livecapture extension');
    expect(document.querySelector(`a[href="/captures/new?group=${G1}"]`)).toBeNull();
    expect(sent('GET', '/api/v1/collectors')).toHaveLength(0);
  });

  it('suggests attribute keys, then values with their agents', async () => {
    routes['GET /api/v1/fleet/agents'] = () => json({ items: [{ id: A1, connected: true, attributes: { 'service.name': 'gw', 'k8s.namespace.name': 'pay' } }] });
    routes['GET /api/v1/fleet'] = (c) => json(c.url.includes('group_by=service.name') ? { ...overview, groups: [{ value: 'gw', total: 2, connected: 1 }] } : overview);
    await render('/fleet/groups');
    await settle(() => !!button('New group'));
    await click(button('New group'));
    const options = () => [...document.querySelectorAll('#group-selector-options option')].map((o) => (o as HTMLOptionElement).value);
    await type('#group-selector', 'serv');
    await settle(() => options().length > 0);
    expect(options()).toEqual(['service.name=']);
    await type('#group-selector', 'service.name=');
    await settle(() => options().includes('service.name=gw'));
    expect((document.querySelector('#group-selector-options option') as HTMLOptionElement).label).toBe('2 agents, 1 connected');
  });

  it('saves an edit at the revision it loaded', async () => {
    routes[`PUT /api/v1/collector-groups/${G1}`] = (c) => json({ ...group, ...(c.body as object), id: G1, revision: 4 });
    await render('/fleet/groups');
    await settle(() => !!button('Edit group'));
    await click(button('Edit group'));
    expect((document.querySelector('#group-name') as HTMLInputElement).value).toBe('payments-gw');
    await type('#group-selector', 'env=prod,team=pay');
    await click(button('Save group'));
    await settle(() => sent('PUT', `/api/v1/collector-groups/${G1}`).length === 1);
    expect(sent('PUT', `/api/v1/collector-groups/${G1}`)[0].body).toEqual({ revision: 3, name: 'payments-gw', selector: 'env=prod,team=pay' });
  });

  it('says what to fix before sending a new group', async () => {
    routes['POST /api/v1/collector-groups'] = (c) => json({ ...(c.body as object), id: 'g3', kind: 'static', revision: 1 });
    await render('/fleet/groups');
    await settle(() => !!button('New group'));
    await click(button('New group'));
    await type('#group-name', 'pinned');
    await click(button('Fixed agents'));
    await type('#group-agents', 'not-an-id');
    await click(button('Create group'));
    expect(text()).toContain('Not an agent ID: not-an-id');
    expect(sent('POST', '/api/v1/collector-groups')).toHaveLength(0);
    await type('#group-agents', `${A1}\n${A2}`);
    await click(button('Create group'));
    await settle(() => sent('POST', '/api/v1/collector-groups').length === 1);
    expect(sent('POST', '/api/v1/collector-groups')[0].body).toEqual({ revision: 0, name: 'pinned', agent_ids: [A1, A2] });
  });

  it('asks before deleting and sends the revision', async () => {
    routes[`DELETE /api/v1/collector-groups/${G1}`] = () => json({ deleted: true });
    await render('/fleet/groups');
    await settle(() => !!button('Delete group'));
    await click(button('Delete group'));
    expect(sent('DELETE', `/api/v1/collector-groups/${G1}`)).toHaveLength(0);
    const confirm = document.querySelector('.od-confirm')!;
    await click([...confirm.querySelectorAll('button')].find((b) => b.textContent === 'Delete group'));
    await settle(() => sent('DELETE', `/api/v1/collector-groups/${G1}`).length === 1);
    expect(sent('DELETE', `/api/v1/collector-groups/${G1}`)[0].url).toContain('revision=3');
  });
  it('disables changes for a viewer and names the permission', async () => {
    client.setQueryData(['session'], { principal: { id: 'user:vic', kind: 'user' }, permissions: VIEWER });
    await render('/fleet/groups');
    await settle(() => (document.querySelector('.od-table tbody')?.textContent ?? '').includes('gw-0'));
    for (const label of ['New group', 'Edit group', 'Delete group']) {
      expect(button(label)!.disabled).toBe(true);
      expect(button(label)!.title).toBe('Needs the fleet.config.write permission, which your role does not have');
    }
    expect(document.querySelector('a[href^="/captures/new"]')).toBeNull();
    expect(button('Capture this group')!.title).toBe('Needs the capture.start permission, which your role does not have');
  });
});

describe('Configs', () => {
  const config = { id: C1, name: 'edge', description: '', revision: 2, latest_version: 2, created_at: '2026-10-27T10:00:00Z', created_by: 'admin' };
  const version = (v: number, body: string) => ({ config_id: C1, version: v, body, sha256: 'f'.repeat(64), size: body.length, created_at: '2026-10-27T10:00:00Z', message: `v${v}` });

  it('refuses a name fleet would refuse, then creates the config', async () => {
    routes['GET /api/v1/fleet/configs'] = () => json({ items: [] });
    routes['POST /api/v1/fleet/configs'] = (c) => json({ ...config, ...(c.body as object), latest_version: 0, revision: 1 });
    routes[`GET /api/v1/fleet/configs/${C1}`] = () => json({ ...config, latest_version: 0 });
    await render('/fleet/configs');
    await settle(() => !!button('New config'));
    await click(button('New config'));
    await type('#config-name', 'Edge Batching');
    await click(button('Create config'));
    expect(text()).toContain('lowercase letters');
    await type('#config-name', 'edge');
    await click(button('Create config'));
    await settle(() => text().includes('No versions yet'));
    expect(sent('POST', '/api/v1/fleet/configs')[0].body).toEqual({ name: 'edge' });
  });

  it('shows a version, its change, and saves a new one from it', async () => {
    routes[`GET /api/v1/fleet/configs/${C1}`] = () => json(config);
    routes[`GET /api/v1/fleet/configs/${C1}/versions`] = () => json({ items: [version(2, ''), version(1, '')] });
    routes[`GET /api/v1/fleet/configs/${C1}/versions/2`] = () => json(version(2, 'processors:\n  batch: {}\n'));
    routes[`GET /api/v1/fleet/configs/${C1}/versions/1`] = () => json(version(1, 'processors: {}\n'));
    routes[`POST /api/v1/fleet/configs/${C1}/versions`] = (c) => json(version(3, (c.body as { body: string }).body));
    await render(`/fleet/configs/${C1}`);
    await settle(() => (document.querySelector('textarea[aria-label="Version content"]') as HTMLTextAreaElement | null)?.value.includes('batch') ?? false);
    await click(button('Changes from v1'));
    await settle(() => !!document.querySelector('.u-add'));
    expect(document.querySelector('a[href^="/fleet/assignments?config="]')).not.toBeNull();

    await click(button('New version'));
    await settle(() => !!document.querySelector('textarea[aria-label="Draft content"]'));
    expect((document.querySelector('textarea[aria-label="Draft content"]') as HTMLTextAreaElement).value).toContain('batch');
    await type('textarea[aria-label="Draft content"]', 'processors:\n  batch:\n    timeout: 2s\n');
    // A dirty draft asks before it is thrown away.
    await click(button('Cancel'));
    expect(text()).toContain('Discard the unsaved version?');
    await click(button('Keep editing'));
    await type('#version-message', 'shorter timeout');
    await click(button('Save version'));
    await settle(() => sent('POST', `/api/v1/fleet/configs/${C1}/versions`).length === 1);
    expect(sent('POST', `/api/v1/fleet/configs/${C1}/versions`)[0].body).toEqual({ body: 'processors:\n  batch:\n    timeout: 2s\n', message: 'shorter timeout' });
  });
});

describe('Assignments', () => {
  it('assigns the linked version with a rollout policy', async () => {
    routes['GET /api/v1/fleet/assignments'] = () => json({ items: [] });
    routes['GET /api/v1/fleet/configs'] = () => json({ items: [{ id: C1, name: 'edge', description: '', revision: 1, latest_version: 2, created_at: '', created_by: '' }] });
    routes['GET /api/v1/collector-groups'] = () => json({ items: [{ id: G1, name: 'gateways', kind: 'selector', revision: 1, selector: 'service.name=gw' }] });
    routes['POST /api/v1/fleet/assignments'] = (c) => json({ ...(c.body as object), id: 'f1'.repeat(16), revision: 1, created_at: '', created_by: '', rollout_id: R1 });
    routes[`GET /api/v1/fleet/assignments/${'f1'.repeat(16)}`] = () => json({ id: 'f1'.repeat(16), name: 'edge-gw', config_id: C1, version: 2, layer: 10, group_id: G1, revision: 1, created_at: '', created_by: '' });
    routes[`GET /api/v1/fleet/assignments/${'f1'.repeat(16)}/preview`] = () => json({ items: [] });
    routes[`GET /api/v1/fleet/configs/${C1}`] = () => json({ id: C1, name: 'edge', description: '', revision: 1, latest_version: 2, created_at: '', created_by: '' });
    await render(`/fleet/assignments?config=${C1}&version=2`);
    await settle(() => !!document.querySelector('#assign-group option[value="' + G1 + '"]'));
    await type('#assign-name', 'edge-gw');
    await type('#assign-group', G1);
    await type('#assign-waves', '1, 50%, 100%');
    await type('#assign-on-failure', 'rollback');
    await click(button('Assign and roll out'));
    await settle(() => sent('POST', '/api/v1/fleet/assignments').length === 1);
    expect(sent('POST', '/api/v1/fleet/assignments')[0].body).toEqual({
      name: 'edge-gw', config_id: C1, version: 2, layer: 10, group_id: G1,
      rollout: { waves: [{ count: 1 }, { percent: 50 }, { percent: 100 }], on_failure: 'rollback' },
    });
  });
});

describe('Rollout', () => {
  const rollout = (state: string) => ({
    id: R1, assignment_id: 'f1'.repeat(16), config_id: C1, created_at: '', created_by: 'admin', updated_at: '', current_wave: 1, wave_count: 2,
    from_version: 1, to_version: 2, state, targets: 4, policy: { waves: [{ count: 1 }, { percent: 100 }] },
    waves: [{ wave: 0, targets: 1, outcomes: { succeeded: 1 }, released_at: '2026-10-27T10:00:00Z' }, { wave: 1, targets: 3, outcomes: { pending: 3 } }],
  });

  it('offers the controls the state allows and asks before rolling back', async () => {
    let state = 'paused';
    routes[`GET /api/v1/fleet/rollouts/${R1}`] = () => json(rollout(state));
    routes[`GET /api/v1/fleet/rollouts/${R1}/targets`] = () => json({ items: [{ agent_id: A1, wave: 0, outcome: 'succeeded', accepted: true }] });
    routes[`POST /api/v1/fleet/rollouts/${R1}/actions`] = (c) => { state = (c.body as { action: string }).action === 'resume' ? 'running' : 'rolling_back'; return json(rollout(state)); };
    await render(`/fleet/rollouts/${R1}`);
    await settle(() => !!button('Resume'));
    expect(text()).toContain('v1 → v2');
    expect(text()).toContain('1/4 done');
    expect(button('Pause')).toBeUndefined();
    await click(button('Resume'));
    await settle(() => !!button('Pause'));
    expect(sent('POST', `/api/v1/fleet/rollouts/${R1}/actions`).map((c) => c.body)).toEqual([{ action: 'resume' }]);

    await click(button('Roll back'));
    expect(sent('POST', `/api/v1/fleet/rollouts/${R1}/actions`)).toHaveLength(1);
    expect(text()).toContain('returns every released agent');
    await click([...document.querySelector('.od-confirm')!.querySelectorAll('button')].find((b) => b.textContent === 'Roll back'));
    await settle(() => text().includes('Rolling back'));
    expect(sent('POST', `/api/v1/fleet/rollouts/${R1}/actions`).map((c) => c.body)).toEqual([{ action: 'resume' }, { action: 'rollback' }]);
  });

  it('shows a cancelled rollout with its outcomes as they ended and no controls', async () => {
    routes[`GET /api/v1/fleet/rollouts/${R1}`] = () => json(rollout('cancelled'));
    routes[`GET /api/v1/fleet/rollouts/${R1}/targets`] = () => json({ items: [{ agent_id: A1, wave: 0, outcome: 'baking', accepted: false }] });
    await render(`/fleet/rollouts/${R1}`);
    await settle(() => text().includes('Cancelled'));
    expect(text()).toContain('outcomes when it ended');
    expect(text()).not.toContain('· 3 pending');
    expect(button('Cancel rollout')).toBeUndefined();
    expect([...document.querySelectorAll('option')].map((o) => o.value)).toContain('baking');
  });
  it('keeps the controls in view but disabled for a role without fleet.rollout', async () => {
    client.setQueryData(['session'], { principal: { id: 'user:eve', kind: 'user' }, permissions: ENGINEER });
    routes[`GET /api/v1/fleet/rollouts/${R1}`] = () => json(rollout('paused'));
    routes[`GET /api/v1/fleet/rollouts/${R1}/targets`] = () => json({ items: [] });
    await render(`/fleet/rollouts/${R1}`);
    await settle(() => !!button('Resume'));
    expect(button('Resume')!.disabled).toBe(true);
    expect(button('Resume')!.title).toBe('Needs the fleet.rollout permission, which your role does not have');
  });
});
