// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ProjectsPage } from './ProjectsPage';

const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status });
const DEFAULT = { id: 'default', name: 'Default', built_in: true, archived: false, permissions: ['run.read'] };
const LIBRARY = { id: 'library', name: 'Library', built_in: true, archived: false };
const PAYMENTS = { id: 'payments', name: 'Payments', built_in: false, archived: false, retention_runs: 50 };

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
const button = (label: string, n = 0) => [...document.querySelectorAll('button')].filter((b) => b.textContent?.trim() === label)[n] as HTMLButtonElement | undefined;
const click = async (el: Element | null | undefined) => act(async () => (el as HTMLElement).click());
const value = (id: string) => (document.getElementById(id) as HTMLInputElement).value;
async function type(id: string, v: string) {
  const el = document.getElementById(id) as HTMLInputElement;
  const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
  await act(async () => {
    set.call(el, v);
    el.dispatchEvent(new Event('input', { bubbles: true }));
  });
}
const render = () => {
  const rootRoute = createRootRoute({ component: Outlet });
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({ getParentRoute: () => rootRoute, path: '/access', component: () => null }),
      createRoute({ getParentRoute: () => rootRoute, path: '/access/projects', component: ProjectsPage }),
    ]),
    history: createMemoryHistory({ initialEntries: ['/access/projects'] }),
  });
  return act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(RouterProvider, { router: router as never }))));
};

describe('ProjectsPage', () => {
  it('adds a project at the loaded revision', async () => {
    let list = { revision: 2, projects: [DEFAULT, LIBRARY] };
    respond = (c) => {
      if (c.method === 'POST') list = { revision: 3, projects: [DEFAULT, LIBRARY, PAYMENTS] };
      return json(list, c.method === 'POST' ? 201 : 200);
    };
    await render();
    await settle(() => text().includes('Default'));
    await type('project-id', 'payments');
    await type('project-name', 'Payments');
    await type('project-retentionRuns', '50');
    await click(button('Add project'));
    await settle(() => text().includes('50 runs'));
    expect(calls.find((c) => c.method === 'POST')?.body).toEqual({ revision: 2, id: 'payments', name: 'Payments', retention_runs: 50 });
  });

  it('lists who is bound in each project and links to granting access there', async () => {
    const bindings = [
      { id: 'a', source: 'managed', group: 'pay-oncall', role: 'engineer', scope: 'project:payments' },
      { id: 'b', source: 'config', group: 'platform', role: 'admin', scope: '*' },
    ];
    respond = (c) => (c.url.includes('/role-bindings') ? json({ revision: 1, bindings, roles: [] }) : json({ revision: 3, projects: [DEFAULT, LIBRARY, PAYMENTS] }));
    await render();
    await settle(() => text().includes('Group pay-oncall · engineer'));
    expect(text()).toContain('and 1 binding everywhere');
    const links = [...document.querySelectorAll('a')].filter((a) => a.textContent === 'Grant access').map((a) => a.getAttribute('href'));
    expect(links).toContain('/access?grant=payments');
  });

  it('asks before editing another project replaces unsaved changes', async () => {
    respond = () => json({ revision: 3, projects: [DEFAULT, LIBRARY, PAYMENTS] });
    await render();
    await settle(() => text().includes('Payments'));
    await type('project-name', 'Half typed');
    await click(button('Edit', 2));
    expect(text()).toContain('unsaved changes');
    await click(button('Keep editing'));
    expect(value('project-name')).toBe('Half typed');
    await click(button('Edit', 2));
    await click(button('Discard changes'));
    expect(value('project-name')).toBe('Payments');
    expect(value('project-id')).toBe('payments');
  });
});
