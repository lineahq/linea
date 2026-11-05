// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { RuntimeBuild } from '../../api/types';
import { getWorkspace, updateWorkspace } from '../../workspace/store';
import { rememberBuild } from '../buildMemory';
import { BuildPage } from './BuildPage';

/**
 * The build page: a build read once, its log followed over a server-sent
 * stream (a fake EventSource here), Cancel, and the way from a finished
 * build to a run on the new runtime.
 */

class FakeEventSource {
  static CONNECTING = 0; static OPEN = 1; static CLOSED = 2;
  static instances: FakeEventSource[] = [];
  readyState = 1;
  closed = false;
  private listeners = new Map<string, ((m: Event) => void)[]>();
  constructor(readonly url: string) { FakeEventSource.instances.push(this); }
  addEventListener(type: string, fn: (m: Event) => void) { this.listeners.set(type, [...(this.listeners.get(type) ?? []), fn]); }
  close() { this.closed = true; this.readyState = 2; }
  emit(type: string, data?: unknown) {
    const ev = data === undefined ? new Event(type) : Object.assign(new Event(type), { data: JSON.stringify(data) });
    this.listeners.get(type)?.forEach((fn) => fn(ev));
  }
}

const build = (patch: Partial<RuntimeBuild> = {}): RuntimeBuild => ({
  id: 'b1', name: 'mine', collector_version: '0.162.0', created: '2026-10-31T10:00:00Z', started: '2026-10-31T10:00:05Z',
  log: [], log_from: 0, log_lines: 0, reused: false, state: 'running', verified: true, ...patch,
});
const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status });

interface FetchCall { method: string; url: string }
let calls: FetchCall[];
let routes: Record<string, () => Response>;
let host: HTMLDivElement, root: Root, client: QueryClient;
const stream = () => FakeEventSource.instances.at(-1)!;

beforeEach(() => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
  vi.stubGlobal('scrollTo', () => undefined);
  vi.stubGlobal('EventSource', FakeEventSource);
  FakeEventSource.instances = [];
  updateWorkspace({ runtime: undefined, configYAML: 'receivers: {}\n', configPath: 'checkout-logs.yaml' });
  localStorage.clear();
  calls = [];
  routes = {};
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    calls.push({ method, url });
    const route = routes[`${method} ${url}`];
    if (route) return route();
    throw new Error(`Unexpected request: ${method} ${url}`);
  }));
  client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
});
afterEach(() => {
  act(() => root.unmount());
  client.clear();
  host.remove();
  localStorage.clear();
  updateWorkspace({ runtime: undefined, configYAML: '', configPath: '' });
  vi.unstubAllGlobals();
});

function makeRouter(entry: string) {
  const rootRoute = createRootRoute({ component: Outlet });
  const page = (path: string, component: () => ReturnType<typeof createElement>) => createRoute({ getParentRoute: () => rootRoute, path, component });
  const tree = rootRoute.addChildren([
    page('/', () => createElement('p', null, 'The debugger')),
    page('/runtimes', () => createElement('p', null, 'The runtimes')),
    page('/runtimes/builds/$buildId', BuildPage as never),
  ]);
  return createRouter({ routeTree: tree, history: createMemoryHistory({ initialEntries: [entry] }) });
}
type TestRouter = ReturnType<typeof makeRouter>;

async function render(entry = '/runtimes/builds/b1', held?: string[]): Promise<TestRouter> {
  if (held) client.setQueryData(['session'], { permissions: held });
  const router = makeRouter(entry);
  await act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(RouterProvider, { router: router as never }))));
  await settle(() => !!host.querySelector('.od-page, [role="alert"]'));
  return router;
}
async function settle(until: () => boolean) {
  for (let i = 0; i < 200 && !until(); i++) await act(() => new Promise((r) => setTimeout(r, 5)));
}
const button = (label: string) => [...document.querySelectorAll('button')].find((b) => b.textContent?.trim() === label) as HTMLButtonElement | undefined;
const logLines = () => [...document.querySelectorAll('[role="log"] .od-code__line')].map((l) => l.textContent);
const fire = (type: string, data?: unknown) => act(async () => stream().emit(type, data));
const path = (r: TestRouter) => r.state.location.pathname + r.state.location.searchStr;

describe('Build page', () => {
  it('reads the build once, then follows its log over the event stream from the next line', async () => {
    routes['GET /api/v1/runtime-builds/b1'] = () => json(build({ log: ['go: downloading', 'compiling'], log_lines: 2 }));
    await render();
    expect(logLines()).toEqual(['1go: downloading', '2compiling']);
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(stream().url).toBe('/api/v1/runtime-builds/b1/events?log_from=2');
    expect(document.querySelector('[role="progressbar"]')).not.toBeNull();
    expect(button('Cancel build')).toBeDefined();
    expect(button('Rebuild')).toBeUndefined();

    await fire('build', build({ log: ['linking'], log_from: 2, log_lines: 3 }));
    expect(logLines()).toEqual(['1go: downloading', '2compiling', '3linking']);
    expect(stream().closed).toBe(false);
  });

  it('ends on the end event: closes the stream, keeps the log, offers a run on the new runtime and a rebuild', async () => {
    routes['GET /api/v1/runtime-builds/b1'] = () => json(build({ log: ['compiling'], log_lines: 1 }));
    await render();
    await fire('build', build({ state: 'done', log: [], log_from: 1, log_lines: 1, finished: '2026-10-31T10:01:05Z' }));
    await fire('end', build({ state: 'done', log: [], log_from: 1, log_lines: 1, finished: '2026-10-31T10:01:05Z' }));
    expect(stream().closed).toBe(true);
    expect(logLines()).toEqual(['1compiling']);
    expect(document.querySelector('[role="status"].od-callout--pass, .od-callout--pass')?.textContent).toContain('Built mine');
    expect(button('Run on mine')).toBeDefined();
    expect(button('Cancel build')).toBeUndefined();
    expect(button('Rebuild')).toBeDefined();
    expect(document.querySelector('[role="progressbar"]')).toBeNull();
  });

  it('does not open a stream for a build that already ended', async () => {
    routes['GET /api/v1/runtime-builds/b1'] = () => json(build({ state: 'failed', error: 'go build: exit status 1', log: ['boom'], log_lines: 1 }));
    await render();
    expect(FakeEventSource.instances).toHaveLength(0);
    expect(logLines()).toEqual(['1boom']);
    expect(host.querySelector('[role="alert"]')?.textContent).toContain('go build: exit status 1');
    expect(button('Run on mine')).toBeUndefined();
  });

  it('says a reused build compiled nothing', async () => {
    routes['GET /api/v1/runtime-builds/b1'] = () => json(build({ state: 'done', reused: true }));
    await render();
    expect(host.textContent).toContain('nothing was compiled');
    expect(host.textContent).toContain('The build wrote no log.');
  });

  it('cancels a running build and stops following only when the stream ends', async () => {
    routes['GET /api/v1/runtime-builds/b1'] = () => json(build());
    routes['POST /api/v1/runtime-builds/b1/cancel'] = () => json(build({ state: 'cancelled' }));
    await render();
    await act(async () => button('Cancel build')!.click());
    expect(calls.filter((c) => c.method === 'POST').map((c) => c.url)).toEqual(['/api/v1/runtime-builds/b1/cancel']);
    await fire('end', build({ state: 'cancelled' }));
    expect(host.textContent).toContain('Build cancelled');
    expect(button('Run on mine')).toBeUndefined();
  });

  it('shows a refused cancel', async () => {
    routes['GET /api/v1/runtime-builds/b1'] = () => json(build());
    routes['POST /api/v1/runtime-builds/b1/cancel'] = () => json({ error: 'not your build' }, 403);
    await render();
    await act(async () => button('Cancel build')!.click());
    await settle(() => host.textContent!.includes('Could not cancel'));
    expect(host.textContent).toContain('Could not cancel');
  });

  it('selects the runtime in the workspace and opens the debugger from "Run on"', async () => {
    routes['GET /api/v1/runtime-builds/b1'] = () => json(build({ state: 'done' }));
    const router = await render();
    expect(getWorkspace().runtime).toBeUndefined();
    await act(async () => button('Run on mine')!.click());
    expect(getWorkspace().runtime).toBe('mine');
    expect(getWorkspace().configYAML).toBe('receivers: {}\n');
    await settle(() => path(router) === '/');
    expect(path(router)).toBe('/');
  });

  it('opens the builder seeded from the build with Rebuild', async () => {
    routes['GET /api/v1/runtime-builds/b1'] = () => json(build({ state: 'failed', error: 'x' }));
    const router = await render();
    await act(async () => button('Rebuild')!.click());
    await settle(() => router.state.location.pathname === '/runtimes');
    expect(path(router)).toBe('/runtimes?rebuild=b1');
  });

  it('shows the manifest the server returns, or the one this browser remembers', async () => {
    routes['GET /api/v1/runtime-builds/b1'] = () => json(build({ state: 'done' }));
    await render();
    expect(host.querySelector('section[aria-label="Manifest"]')?.textContent).toContain('did not return');
    act(() => root.unmount());
    root = createRoot(host);
    routes['GET /api/v1/runtime-builds/b1'] = () => json(build({ state: 'done', manifest_yaml: 'dist:\n  name: theirs\n' }));
    await render();
    let card = host.querySelector('section[aria-label="Manifest"]')!;
    expect(card.textContent).toContain('as sent');
    expect(card.textContent).toContain('name: theirs');
    act(() => root.unmount());
    root = createRoot(host);
    rememberBuild(localStorage, 'b1', { name: 'mine', manifest: 'dist:\n  name: mine\n' });
    await render();
    card = host.querySelector('section[aria-label="Manifest"]')!;
    expect(card.textContent).toContain('name: mine');
  });

  it('says a build that is not there is not available, and opens no stream', async () => {
    routes['GET /api/v1/runtime-builds/b1'] = () => json({ error: 'no build' }, 404);
    await render();
    expect(host.querySelector('[role="alert"]')?.textContent).toContain('This build is not available');
    expect(FakeEventSource.instances).toHaveLength(0);
  });

  it('closes the stream on leaving without cancelling the build', async () => {
    routes['GET /api/v1/runtime-builds/b1'] = () => json(build());
    await render();
    const s = stream();
    act(() => root.unmount());
    root = createRoot(host);
    expect(s.closed).toBe(true);
    expect(calls.filter((c) => c.method === 'POST')).toEqual([]);
  });

  it('disables Cancel and Rebuild for a viewer, naming the missing permission', async () => {
    routes['GET /api/v1/runtime-builds/b1'] = () => json(build());
    await render('/runtimes/builds/b1', ['run.read']);
    expect(button('Cancel build')!.disabled).toBe(true);
    expect(button('Cancel build')!.title).toContain('runtime.build');
    await fire('end', build({ state: 'failed', error: 'x' }));
    expect(button('Rebuild')!.disabled).toBe(true);
    expect(button('Rebuild')!.title).toContain('runtime.build');
  });
});
