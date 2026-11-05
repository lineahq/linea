// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { RunPruneRequest, StorageUsage } from '../api/types';
import { RunStoragePage } from './RunStoragePage';

const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status });
const usage: StorageUsage = {
  limit: 4_000_000_000, run_limit: 100_000_000, used: 1_000_000_000, runs: 1200, kept_runs: 3, oldest_run: '2026-09-01T10:00:00Z',
  retention: { runs: 10_000, age_seconds: 30 * 86_400 },
};

interface FetchCall { method: string; url: string; body?: RunPruneRequest }
let calls: FetchCall[];
let respond: (call: FetchCall) => Response;
let host: HTMLDivElement, root: Root, client: QueryClient;

beforeEach(() => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
  vi.stubGlobal('scrollTo', () => undefined);
  calls = [];
  respond = (c) => {
    if (c.url === '/api/v1/storage') return json(usage);
    if (c.url === '/api/v1/runs/prune') return json({ runs: 40, bytes: 3_200_000, dry_run: !!c.body?.dry_run });
    return json({ error: `unexpected ${c.method} ${c.url}` }, 404);
  };
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    const call: FetchCall = { method: init?.method ?? 'GET', url, body: init?.body ? JSON.parse(String(init.body)) : undefined };
    calls.push(call);
    return respond(call);
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

async function render() {
  const rootRoute = createRootRoute({ component: Outlet });
  const page = (path: string, component: () => ReturnType<typeof createElement>) => createRoute({ getParentRoute: () => rootRoute, path, component });
  const router = createRouter({
    routeTree: rootRoute.addChildren([page('/runs/storage', RunStoragePage as never), page('/runs', () => createElement('p', null, 'All runs')), page('/', () => createElement('p', null, 'Debugger'))]),
    history: createMemoryHistory({ initialEntries: ['/runs/storage'] }),
  });
  await act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(RouterProvider, { router: router as never }))));
  await settle(() => !!host.querySelector('.od-page'));
}
async function settle(until: () => boolean) {
  for (let i = 0; i < 200 && !until(); i++) await act(() => new Promise((r) => setTimeout(r, 5)));
}
const button = (text: string) => [...host.querySelectorAll('button')].find((b) => b.textContent === text) as HTMLButtonElement | undefined;
const field = (id: string) => host.querySelector<HTMLInputElement | HTMLSelectElement>(`#${id}`)!;
function enter(el: HTMLInputElement | HTMLSelectElement, value: string) {
  const select = el instanceof HTMLSelectElement;
  Object.getOwnPropertyDescriptor(select ? HTMLSelectElement.prototype : HTMLInputElement.prototype, 'value')!.set!.call(el, value);
  act(() => { el.dispatchEvent(new Event(select ? 'change' : 'input', { bubbles: true })); });
}
const prunes = () => calls.filter((c) => c.url === '/api/v1/runs/prune');

describe('Run storage page', () => {
  it('shows usage against the limit, the counts and the retention policy', async () => {
    await render();
    await settle(() => !!host.querySelector('.od-meter'));
    expect(host.querySelector('.od-meter')!.getAttribute('aria-label')).toBe('1.0 GB of 4.0 GB, 25% in use');
    const text = host.querySelector('.od-kv')!.textContent!;
    expect(text).toContain('Runs1,200');
    expect(text).toContain('Kept runs3');
    expect(text).toContain('Oldest runSep 1');
    expect(text).toContain('RetentionAt most 10,000 runs, removed after 30 days.');
    expect(host.textContent).toContain('1.0 GB of 4.0 GB');
  });

  it('enables the prune only after a preview of the same values, then asks inline and refreshes', async () => {
    await render();
    await settle(() => !!host.querySelector('.od-meter'));
    expect(button('Prune')!.disabled).toBe(true);

    enter(field('prune-before'), '2026-10-01');
    enter(field('prune-result'), 'failed');
    enter(field('prune-scenario'), 'checkout');
    await act(async () => button('Preview')!.click());
    await settle(() => prunes().length === 1);
    expect(prunes()[0].body).toEqual({ before: new Date(2026, 9, 1).toISOString(), result: 'failed', scenario: 'checkout', dry_run: true });
    await settle(() => host.textContent!.includes('40 runs, 3.2 MB freed'));
    expect(button('Prune')!.disabled).toBe(false);

    // Changing a value invalidates the preview: nothing can be pruned for values never counted.
    enter(field('prune-scenario'), 'other');
    expect(button('Prune')!.disabled).toBe(true);
    expect(host.textContent).not.toContain('40 runs, 3.2 MB freed');
    enter(field('prune-scenario'), 'checkout');
    expect(button('Prune')!.disabled).toBe(false);

    await act(async () => button('Prune')!.click());
    expect(prunes()).toHaveLength(1);
    const confirm = host.querySelector('[role=alertdialog]')!;
    expect(confirm.textContent).toContain('Delete 40 runs (finished runs admitted before 2026-10-01, with result failed, of scenario checkout), freeing 3.2 MB?');
    await act(async () => button('Cancel')!.click());
    expect(host.querySelector('[role=alertdialog]')).toBeNull();
    expect(prunes()).toHaveLength(1);

    const before = calls.filter((c) => c.url === '/api/v1/storage').length;
    await act(async () => button('Prune')!.click());
    await act(async () => button('Delete 40 runs')!.click());
    await settle(() => prunes().length === 2);
    expect(prunes()[1].body).toEqual({ before: new Date(2026, 9, 1).toISOString(), result: 'failed', scenario: 'checkout' });
    await settle(() => !!host.querySelector('.od-callout--pass'));
    expect(host.querySelector('.od-callout--pass')!.textContent).toBe('Removed 40 runs, freed 3.2 MB.');
    await settle(() => calls.filter((c) => c.url === '/api/v1/storage').length > before);
    // The pruned runs are gone, so a new prune needs a new preview.
    expect(button('Prune')!.disabled).toBe(true);
  });

  it('keeps Prune disabled when the preview found no run', async () => {
    respond = (c) => (c.url === '/api/v1/storage' ? json(usage) : json({ runs: 0, bytes: 0, dry_run: true }));
    await render();
    await settle(() => !!host.querySelector('.od-meter'));
    await act(async () => button('Preview')!.click());
    await settle(() => host.textContent!.includes('No run matches'));
    expect(button('Prune')!.disabled).toBe(true);
  });

  it('says only an admin may prune on a 403', async () => {
    respond = (c) => (c.url === '/api/v1/storage' ? json(usage) : json({ code: 'forbidden', error: 'forbidden: admin required' }, 403));
    await render();
    await settle(() => !!host.querySelector('.od-meter'));
    await act(async () => button('Preview')!.click());
    await settle(() => !!host.querySelector('.od-callout--fail'));
    expect(host.querySelector('.od-callout--fail')!.textContent).toBe('Only an admin may prune runs.');
  });

  it('disables the prune form for a role without run.delete and names the permission', async () => {
    client.setQueryData(['session'], { principal: { id: 'user:vic', kind: 'user' }, permissions: ['run.read', 'artifact.read'] });
    await render();
    await settle(() => !!host.querySelector('.od-meter'));
    expect(button('Preview')!.disabled).toBe(true);
    expect(button('Preview')!.title).toBe('Needs the run.delete permission, which your role does not have');
    expect(button('Prune')!.disabled).toBe(true);
  });

  it('explains a server without run history instead of a generic failure', async () => {
    respond = () => json({ code: 'unavailable', error: 'no saved work' }, 503);
    await render();
    await settle(() => !!host.querySelector('[role=alert]'));
    expect(host.querySelector('[role=alert]')!.textContent).toContain('keeps no saved work');
  });
});
