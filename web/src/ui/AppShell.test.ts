// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AppShell } from './AppShell';

const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status });
const VIEWER = ['run.read', 'artifact.read', 'capture.read', 'fleet.read', 'runtime.read', 'token.own'];
const ADMIN = [...VIEWER, 'run.submit', 'run.delete', 'artifact.write', 'capture.start', 'capture.raw', 'fleet.config.write', 'fleet.rollout', 'runtime.manage', 'audit.read', 'token.manage', 'access.manage'];

interface Call { method: string; url: string; headers: Record<string, string> }
let calls: Call[];
let session: () => Response;
let host: HTMLDivElement, root: Root, client: QueryClient;

beforeEach(() => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
  vi.stubGlobal('scrollTo', () => undefined);
  calls = [];
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    calls.push({ method, url, headers: (init?.headers ?? {}) as Record<string, string> });
    const path = url.split('?')[0];
    if (path === '/api/v1/session') return session();
    if (path === '/api/v1/captures') return json({ items: [] });
    if (path === '/api/v1/runtime-builds') return json({ builds: [] });
    if (path === '/auth/logout') return new Response(null, { status: 204 });
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
  const rootRoute = createRootRoute({ component: AppShell });
  const page = (path: string) => createRoute({ getParentRoute: () => rootRoute, path, component: () => createElement('p', null, `Page ${path}`) });
  const tree = rootRoute.addChildren([page('/'), page('/runs'), page('/audit')]);
  const router = createRouter({ routeTree: tree, history: createMemoryHistory({ initialEntries: [entry] }) });
  await act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(RouterProvider, { router: router as never }))));
}
const text = () => document.body.textContent ?? '';
const rail = () => [...document.querySelectorAll('nav[aria-label="Destinations"] a')].map((a) => a.textContent);
const button = (label: string) => [...document.querySelectorAll('button')].find((b) => b.textContent?.trim() === label) as HTMLButtonElement | undefined;
const click = async (el: Element | null | undefined) => act(async () => (el as HTMLElement).click());

describe('AppShell', () => {
  it('asks to sign in on a team server, returning to the page', async () => {
    session = () => json({ error: 'unauthenticated: sign in first' }, 401);
    await render('/runs?run=r-1');
    await settle(() => text().includes('Sign in to the debugger'));
    expect(document.querySelector('a')!.getAttribute('href')).toBe('/auth/login?return_to=%2Fruns%3Frun%3Dr-1');
    expect(text()).not.toContain('Page /runs');
  });

  it('lists only the destinations a viewer can use', async () => {
    session = () => json({ principal: { id: 'user:vic', kind: 'user', name: 'Vic' }, permissions: VIEWER, csrf_token: 'tok' });
    await render('/');
    await settle(() => rail().length === 5);
    expect(rail()).toEqual(['Debugger', 'Livecapture', 'Fleet', 'Runtimes', 'Savedwork']);
    expect(text()).toContain('Page /');
  });

  it('shows the audit log to an admin and signs out with the CSRF token', async () => {
    session = () => json({ principal: { id: 'user:ada', kind: 'user', name: 'Ada' }, permissions: ADMIN, csrf_token: 'tok-1' });
    const assign = vi.fn();
    vi.stubGlobal('location', { ...window.location, assign });
    await render('/');
    await settle(() => rail().includes('Auditlog'));
    expect(rail()).toHaveLength(8);
    await click(button('Settings'));
    expect(text()).toContain('Signed in as');
    expect(text()).toContain('Ada');
    await click(button('Sign out'));
    await settle(() => assign.mock.calls.length === 1);
    const logout = calls.find((c) => c.url === '/auth/logout')!;
    expect(logout.method).toBe('POST');
    expect(logout.headers['X-CSRF-Token']).toBe('tok-1');
    expect(assign).toHaveBeenCalledWith('/');
  });

  it('offers no sign-out for the local owner, who holds every permission', async () => {
    session = () => json({ principal: { id: 'local-owner', kind: 'local', name: 'local-owner' }, permissions: ADMIN });
    await render('/');
    await settle(() => rail().length === 8);
    await click(button('Settings'));
    expect(text()).toContain('Local owner');
    expect(button('Sign out')).toBeUndefined();
  });

  it('says a principal without a role binding has no access', async () => {
    session = () => json({ principal: { id: 'user:mal', kind: 'user', name: 'Mallory' }, permissions: [] });
    await render('/runs');
    await settle(() => text().includes('No access yet'));
    expect(text()).toContain('signed in as Mallory');
    expect(rail()).toEqual([]);
    expect(text()).not.toContain('Page /runs');
  });

  it('renders no page before the session, and its CSRF token, are known', async () => {
    let answer!: () => void;
    const answered = new Promise<void>((r) => (answer = r));
    session = () => json({ principal: { id: 'user:eve', kind: 'user', name: 'Eve' }, permissions: ADMIN, csrf_token: 'tok-2' });
    const fetchMock = vi.mocked(fetch);
    const respond = fetchMock.getMockImplementation()!;
    fetchMock.mockImplementation(async (url, init) => {
      if (String(url).split('?')[0] === '/api/v1/session') await answered;
      return respond(url as string, init);
    });
    await render('/runs');
    await act(() => new Promise((r) => setTimeout(r, 20)));
    expect(text()).not.toContain('Page /runs');
    answer();
    await settle(() => text().includes('Page /runs'));
  });

  it('offers everything when the session cannot be read for another reason', async () => {
    session = () => json({ error: 'boom' }, 500);
    await render('/');
    await settle(() => calls.some((c) => c.url === '/api/v1/session') && text().includes('Page /'));
    expect(rail()).toHaveLength(8);
  });
});
