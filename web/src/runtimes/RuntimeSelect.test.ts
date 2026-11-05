// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { getWorkspace, updateWorkspace, useWorkspace } from '../workspace/store';
import type { Runtime, RuntimeDefault } from '../api/types';
import { RuntimeSelect } from './RuntimeSelect';

const runtime = (name: string, version: string, verified: boolean): Runtime => ({
  name, collector_version: version, verified, go_image: 'golang', image_id: `sha256:${name}`, manifest: '',
  module_set: '', product_version: 'dev', protocol: 1, size: 70_000_000,
});
const support = { oldest: '0.160.0', verified: ['0.162.0'], built_in: '0.162.0' };

interface FetchCall { method: string; url: string; body: unknown }
let calls: FetchCall[];
let runtimes: Runtime[];
let team: RuntimeDefault;
let routes: Record<string, (body: unknown) => Response>;
let host: HTMLDivElement, root: Root, client: QueryClient;
const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status });

beforeEach(() => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
  updateWorkspace({ runtime: undefined, configYAML: 'processors:\n  transform:\n' });
  calls = [];
  runtimes = [runtime('v0163', '0.163.0', false), runtime('mine', '0.162.0', true)];
  team = { name: 'built-in' };
  routes = {};
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    calls.push({ method, url, body });
    const route = routes[`${method} ${url}`];
    if (route) return route(body);
    if (method === 'GET' && url === '/api/v1/runtimes') return json(runtimes);
    if (method === 'GET' && url === '/api/v1/runtime-default') return json(team);
    if (method === 'GET' && url === '/api/v1/collector-versions') return json(support);
    throw new Error(`Unexpected request: ${method} ${url}`);
  }));
  client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
});
afterEach(() => {
  act(() => root.unmount());
  client.clear();
  host.remove();
  updateWorkspace({ runtime: undefined, configYAML: '' });
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function Select() {
  return createElement('div', null, createElement(RuntimeSelect, { selected: useWorkspace().runtime ?? '' }), createElement(Outlet));
}
function makeRouter(entry: string) {
  const rootRoute = createRootRoute({ component: Select });
  const page = (path: string, text: string) => createRoute({ getParentRoute: () => rootRoute, path, component: () => createElement('p', null, text) });
  return createRouter({
    routeTree: rootRoute.addChildren([page('/', 'The debugger'), page('/runtimes', 'The Runtimes destination')]),
    history: createMemoryHistory({ initialEntries: [entry] }),
  });
}
type TestRouter = ReturnType<typeof makeRouter>;
async function mount(entry = '/'): Promise<TestRouter> {
  const router = makeRouter(entry);
  await act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(RouterProvider, { router: router as never }))));
  return router;
}
async function render(entry = '/') {
  const router = await mount(entry);
  await settle(() => host.querySelectorAll('option').length > 1);
  return router;
}
async function settle(until: () => boolean) {
  for (let i = 0; i < 200 && !until(); i++) await act(() => new Promise((r) => setTimeout(r, 5)));
}
const select = () => host.querySelector<HTMLSelectElement>('select[aria-label="Runtime"]')!;
async function choose(el: HTMLSelectElement | HTMLInputElement | HTMLTextAreaElement, value: string) {
  const proto = el instanceof HTMLSelectElement ? HTMLSelectElement.prototype : el instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
  Object.getOwnPropertyDescriptor(proto, 'value')!.set!.call(el, value);
  await act(async () => el.dispatchEvent(new Event(el instanceof HTMLSelectElement ? 'change' : 'input', { bubbles: true })));
}

describe('RuntimeSelect', () => {
  it('chooses the runtime beside Run and shows unverified while it is selected', async () => {
    await render();
    await settle(() => select().options[0].textContent !== 'Default');
    expect([...select().options].map((o) => o.textContent)).toEqual([
      'Default · built-in · Collector 0.162.0', 'mine · Collector 0.162.0', 'v0163 · Collector 0.163.0 · unverified',
    ]);
    expect(host.querySelector('.badge.unverified')).toBeNull();
    await choose(select(), 'v0163');
    expect(getWorkspace().runtime).toBe('v0163');
    expect(host.querySelector('.badge.unverified')!.textContent).toBe('unverified');
    await choose(select(), '');
    expect(getWorkspace().runtime).toBeUndefined();
  });

  it('names the team default and keeps built-in a choice of its own', async () => {
    team = { name: 'v0163', set_by: 'ana', set: '2026-03-04T10:00:00Z' };
    await render();
    await settle(() => select().options[0].textContent !== 'Default');
    expect([...select().options].map((o) => [o.value, o.textContent])).toEqual([
      ['', 'Default · v0163 · Collector 0.163.0 · unverified'],
      ['built-in', 'Built-in · Collector 0.162.0'],
      ['mine', 'mine · Collector 0.162.0'],
      ['v0163', 'v0163 · Collector 0.163.0 · unverified'],
    ]);
    // The default is selected (no runtime sent) and unverified, which says so.
    expect(select().value).toBe('');
    expect(host.querySelector('.badge.unverified')).not.toBeNull();
    await choose(select(), 'built-in');
    expect(getWorkspace().runtime).toBe('built-in');
    expect(host.querySelector('.badge.unverified')).toBeNull();
    expect(host.querySelector('[role=alert]')).toBeNull();
    await choose(select(), '');
    expect(getWorkspace().runtime).toBeUndefined();
  });

  it('says only Default while the default is unknown', async () => {
    routes['GET /api/v1/runtime-default'] = () => json({ code: 'unavailable', error: 'no store' }, 503);
    await render();
    expect(select().options[0].textContent).toBe('Default');
    expect(select().options[0].value).toBe('');
  });

  it('keeps a runtime that is gone selected and says so', async () => {
    updateWorkspace({ runtime: 'gone' });
    await render();
    await settle(() => !!host.querySelector('[role=alert]'));
    expect(select().value).toBe('gone');
    expect(host.querySelector('[role=alert]')!.textContent).toContain('Runtime gone is not available on this server');
  });

  it('offers only the built-in runtime when the server has no runtime store', async () => {
    routes['GET /api/v1/runtimes'] = () => json({ code: 'unavailable', error: 'runtimes need Docker' }, 503);
    await mount();
    await settle(() => !!host.querySelector('select') && select().title.includes('Docker'));
    expect(select().options).toHaveLength(1);
    expect(select().title).toBe('Runtimes are unavailable on this server: runtimes need Docker');
  });

  it('links "Runtimes…" to the Runtimes destination with the current location as the way back', async () => {
    const router = await render('/?run=r1');
    const link = [...host.querySelectorAll('a')].find((a) => a.textContent === 'Runtimes…')!;
    expect(link.getAttribute('href')).toBe('/runtimes?from=%2F%3Frun%3Dr1');
    expect(link.title).not.toBe('');
    expect(document.querySelector('[role=dialog]')).toBeNull();
    await act(async () => link.click());
    await settle(() => router.state.location.pathname === '/runtimes');
    expect(router.state.location.pathname + router.state.location.searchStr).toBe('/runtimes?from=%2F%3Frun%3Dr1');
    expect(host.textContent).toContain('The Runtimes destination');
    expect(calls.some((c) => c.method !== 'GET')).toBe(false);
  });
});
