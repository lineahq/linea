// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AccessPage } from './AccessPage';
import { parseAccessSearch } from './accessView';

const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status });
const ROLES = [
  { name: 'viewer', permissions: ['run.read'] },
  { name: 'engineer', permissions: ['run.read', 'run.submit'] },
];
const TEAM = { id: 'config-1', source: 'config', group: 'admins', role: 'admin', scope: '*' };
const SRE = { id: 'a'.repeat(32), source: 'managed', group: 'sre', role: 'viewer', scope: '*', created_by: 'user:idp/root', created: '2026-11-01T10:00:00Z' };

interface Call { method: string; url: string; body?: unknown }
let calls: Call[];
let respond: (c: Call) => Response;
let host: HTMLDivElement, root: Root, client: QueryClient;

beforeEach(() => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
  calls = [];
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    const c = { method: init?.method ?? 'GET', url, body: init?.body ? JSON.parse(String(init.body)) : undefined };
    calls.push(c);
    return respond(c);
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
const text = () => document.body.textContent ?? '';
const button = (label: string) => [...document.querySelectorAll('button')].find((b) => b.textContent?.trim() === label) as HTMLButtonElement | undefined;
const click = async (el: Element | null | undefined) => act(async () => (el as HTMLElement).click());
async function type(id: string, value: string) {
  const el = document.getElementById(id) as HTMLInputElement;
  const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
  await act(async () => {
    set.call(el, value);
    el.dispatchEvent(new Event('input', { bubbles: true }));
  });
}
const render = (entry = '/access') => {
  const rootRoute = createRootRoute({ component: Outlet });
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({ getParentRoute: () => rootRoute, path: '/access', component: AccessPage, validateSearch: parseAccessSearch }),
      createRoute({ getParentRoute: () => rootRoute, path: '/access/projects', component: () => null }),
    ]),
    history: createMemoryHistory({ initialEntries: [entry] }),
  });
  return act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(RouterProvider, { router: router as never }))));
};

describe('AccessPage', () => {
  it('shows server config bindings read-only and removes a managed one after confirming', async () => {
    let list = { revision: 1, bindings: [TEAM, SRE], roles: ROLES };
    respond = (c) => {
      if (c.method === 'DELETE') {
        list = { revision: 2, bindings: [TEAM], roles: ROLES };
        return json(list);
      }
      return json(list);
    };
    await render();
    await settle(() => text().includes('Group sre'));
    expect(text()).toContain('Server config');
    expect([...document.querySelectorAll('button')].filter((b) => b.textContent === 'Remove')).toHaveLength(1);
    await click(button('Remove'));
    expect(text()).toContain('Group sre loses viewer (Everywhere)');
    await click(button('Remove binding'));
    await settle(() => !text().includes('Group sre'));
    const del = calls.find((c) => c.method === 'DELETE')!;
    expect(del.url).toBe(`/api/v1/role-bindings/${SRE.id}?revision=1`);
  });

  it('keeps the form and reloads the list when the revision moved', async () => {
    let list = { revision: 0, bindings: [TEAM], roles: ROLES };
    respond = (c) => {
      if (c.method === 'POST') {
        list = { revision: 1, bindings: [TEAM, SRE], roles: ROLES };
        return json({ error: 'conflict: the role bindings changed or already grant this; reload' }, 409);
      }
      return json(list);
    };
    await render();
    await settle(() => !!button('Grant'));
    expect(button('Grant')!.disabled).toBe(true);
    await type('access-name', 'ops');
    await click(button('Grant'));
    await settle(() => text().includes('Group sre'));
    expect(text()).toContain('The role bindings changed or already grant this; reload');
    expect((document.getElementById('access-name') as HTMLInputElement).value).toBe('ops');
    expect(calls.find((c) => c.method === 'POST')!.body).toEqual({ revision: 0, group: 'ops', role: 'viewer', scope: '*' });
  });

  it('opens the form scoped to the project a link names and offers known projects', async () => {
    const projects = [
      { id: 'default', name: 'Default', built_in: true, archived: false },
      { id: 'old', name: 'Old', built_in: false, archived: true },
      { id: 'payments', name: 'Payments', built_in: false, archived: false },
    ];
    respond = (c) => (c.url.includes('/projects') ? json({ revision: 4, projects }) : json({ revision: 1, bindings: [TEAM], roles: ROLES }));
    await render('/access?grant=payments');
    await settle(() => [...document.querySelectorAll('#access-project option')].some((o) => o.textContent === 'Payments (payments)'));
    expect((document.getElementById('access-scope') as HTMLSelectElement).value).toBe('project');
    expect((document.getElementById('access-project') as HTMLSelectElement).value).toBe('payments');
    expect([...document.querySelectorAll('#access-project option')].map((o) => (o as HTMLOptionElement).value)).toEqual(['', 'default', 'payments']);
    await type('access-name', 'pay-oncall');
    await click(button('Grant'));
    await settle(() => calls.some((c) => c.method === 'POST'));
    expect(calls.find((c) => c.method === 'POST')!.body).toEqual({ revision: 1, group: 'pay-oncall', role: 'viewer', scope: 'project:payments' });
  });
});
