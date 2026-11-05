// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Runtime } from '../../api/types';
import { updateWorkspace } from '../../workspace/store';
import { CLI_STEPS, RuntimesPage } from './RuntimesPage';

/**
 * The operations the Runtimes page offers through its forms: push and import,
 * and building a runtime on the server (a manifest, folders of local modules
 * packed and uploaded, the list of builds, Rebuild). Removing a runtime, the
 * build page and the storage and upgrade pages are covered by
 * RuntimePages.test.ts and BuildPage.test.ts.
 */

const runtime = (name: string, version: string, patch: Partial<Runtime> = {}): Runtime => ({
  name, collector_version: version, verified: true, go_image: 'golang', image_id: `sha256:${name}${version}`, manifest: '',
  module_set: '', product_version: 'dev', protocol: 1, size: 70_000_000, tags: [`linea-runtime:${name}`], ...patch,
});
const support = { oldest: '0.160.0', verified: ['0.162.0'], built_in: '0.162.0' };
const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status });

interface FetchCall { method: string; url: string; body: unknown; headers: Record<string, string>; form?: FormData }
let calls: FetchCall[];
let runtimes: Runtime[];
let routes: Record<string, (call: FetchCall) => Response>;
let host: HTMLDivElement, root: Root, client: QueryClient;

beforeEach(() => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
  vi.stubGlobal('scrollTo', () => undefined);
  updateWorkspace({ runtime: undefined, configYAML: 'processors:\n  transform:\n', configPath: 'checkout-logs.yaml' });
  localStorage.clear();
  calls = [];
  runtimes = [runtime('mine', '0.162.0')];
  routes = {};
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    const form = init?.body instanceof FormData ? init.body : undefined;
    const call: FetchCall = {
      method, url, form, body: init?.body && !form ? JSON.parse(String(init.body)) : undefined,
      headers: Object.fromEntries(Object.entries((init?.headers ?? {}) as Record<string, string>).map(([k, v]) => [k.toLowerCase(), v])),
    };
    calls.push(call);
    const route = routes[`${method} ${url}`];
    if (route) return route(call);
    if (method === 'GET' && url.startsWith('/api/v1/runtime-builds')) return json({ builds: [] });
    if (method === 'GET' && url === '/api/v1/runtimes') return json(runtimes);
    if (method === 'GET' && url === '/api/v1/collector-versions') return json(support);
    // The built-in runtime's component list reads its catalog and manifest.
    if (method === 'GET' && url === '/api/v1/collector-versions/0.162.0/catalog') return json({ collector_version: '0.162.0', source: 't', components: [] });
    if (method === 'POST' && url === '/api/v1/manifests') return json({ manifest: 'dist:\n  otelcol_version: 0.162.0\n', unknown: [] });
    throw new Error(`Unexpected request: ${method} ${url}`);
  }));
  client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
});
afterEach(() => {
  act(() => root.unmount());
  client.clear();
  host.remove();
  updateWorkspace({ runtime: undefined, configYAML: '', configPath: '' });
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function makeRouter(entry: string) {
  const rootRoute = createRootRoute({ component: Outlet });
  const page = (path: string, component: () => ReturnType<typeof createElement>) => createRoute({ getParentRoute: () => rootRoute, path, component });
  const tree = rootRoute.addChildren([
    page('/', () => createElement('p', null, 'The debugger')),
    page('/runtimes', RuntimesPage as never),
    page('/runtimes/builds/$buildId', () => createElement('p', null, 'A build')),
  ]);
  return createRouter({ routeTree: tree, history: createMemoryHistory({ initialEntries: [entry] }) });
}

/** render opens the Runtimes page and waits for its rows. */
async function render(entry = '/runtimes', rows = 2) {
  const router = makeRouter(entry);
  await act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(RouterProvider, { router: router as never }))));
  await settle(() => document.querySelectorAll('.od-table tbody tr').length >= rows);
  return router;
}
async function settle(until: () => boolean) {
  for (let i = 0; i < 200 && !until(); i++) await act(() => new Promise((r) => setTimeout(r, 5)));
}
const button = (text: string) => [...document.querySelectorAll('button')].find((b) => b.textContent === text);
const inside = (el: Element, text: string) => [...el.querySelectorAll('button')].find((b) => b.textContent === text);
const show = (name: string, version: string) => act(async () => (document.querySelector(`button[aria-label="Show ${name} ${version}"]`) as HTMLButtonElement).click());
async function choose(el: HTMLInputElement, value: string) {
  Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(el, value);
  await act(async () => el.dispatchEvent(new Event('input', { bubbles: true })));
}
const field = <T extends Element>(selector: string) => document.querySelector<T>(selector)!;

describe('Runtimes page: push and import', () => {
  it('pushes a runtime to a registry reference', async () => {
    routes['POST /api/v1/runtimes/mine/push'] = () => new Response(null, { status: 204 });
    await render();
    await show('mine', '0.162.0');
    const detail = field('.rt-detail');
    await act(async () => inside(detail, 'Push…')!.click());
    const form = field('form[aria-label="Push mine"]');
    const target = form.querySelector<HTMLInputElement>('input[aria-label="Push to"]')!;
    await choose(target, 'linea-runtime:mine');
    expect(form.textContent).toContain('not the local name');
    expect(inside(form, 'Push')!.disabled).toBe(true);
    await choose(target, 'registry.example.com/team/rt:1');
    await act(async () => inside(form, 'Push')!.click());
    await settle(() => !!form.textContent?.includes('Pushed to'));
    expect(calls.filter((c) => c.method === 'POST' && c.url !== '/api/v1/manifests').map((c) => [c.url, c.body])).toEqual([
      ['/api/v1/runtimes/mine/push', { target: 'registry.example.com/team/rt:1' }],
    ]);
  });

  it('imports a runtime image by reference and pulls it first', async () => {
    routes['POST /api/v1/runtimes/import'] = () => json(runtime('ci', '0.161.0'));
    await render();
    await act(async () => button('Import image')!.click());
    const form = field('form[aria-label="Import runtime"]');
    await choose(form.querySelector<HTMLInputElement>('input[aria-label="Image reference"]')!, 'registry.example.com/team/rt:2');
    await act(async () => inside(form, 'Import')!.click());
    await settle(() => !!form.textContent?.includes('Imported'));
    expect(calls.filter((c) => c.method === 'POST' && c.url !== '/api/v1/manifests').map((c) => [c.url, c.body])).toEqual([
      ['/api/v1/runtimes/import', { ref: 'registry.example.com/team/rt:2', pull: true }],
    ]);
  });
});

describe('Runtimes page: building a runtime', () => {
  const open = async () => {
    await render();
    await act(async () => button('Build a runtime')!.click());
    return field('section[aria-label="Build a runtime"]');
  };
  const radio = (label: string) => field<HTMLInputElement>(`input[type="radio"][aria-label="${label}"]`);
  const setText = async (el: HTMLTextAreaElement, text: string) => {
    Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!.call(el, text);
    await act(async () => el.dispatchEvent(new Event('input', { bubbles: true })));
  };
  const importManifest = async (text: string) => {
    await act(async () => radio('Import a manifest').click());
    await setText(field<HTMLTextAreaElement>('textarea[aria-label="Manifest"]'), text);
  };
  const form = () => field('section[aria-label="New runtime"]');
  const posts = () => calls.filter((c) => c.method === 'POST' && c.url === '/api/v1/runtime-builds');
  const queued = (id: string, name: string) => json({ id, name, collector_version: '0.162.0', state: 'queued', created: '2026-10-31T10:00:00Z', log_from: 0, log_lines: 0, reused: false, verified: true }, 202);
  const picked = (rel: string, text: string) => Object.assign(new File([text], rel.split('/').at(-1)!), { webkitRelativePath: rel });
  const pick = async (module: string, files: File[]) => {
    const input = field<HTMLInputElement>(`input[aria-label="Folder picker for ${module}"]`);
    Object.defineProperty(input, 'files', { configurable: true, value: files });
    await act(async () => input.dispatchEvent(new Event('change', { bubbles: true })));
  };

  it('opens the builder panel on request and shows the CLI line beside it, not instead of it', async () => {
    await render();
    expect(document.querySelector('section[aria-label="Build a runtime"]')).toBeNull();
    expect(button('Add a runtime')).toBeUndefined();
    await act(async () => button('Build a runtime')!.click());
    const panel = field('section[aria-label="Build a runtime"]');
    expect(panel.hasAttribute('hidden')).toBe(false);
    expect(panel.textContent).toContain(CLI_STEPS);
    expect(CLI_STEPS).toContain('linea runtime build --server');
    expect(panel.querySelector('section[aria-label="New runtime"]')).not.toBeNull();
    expect(calls.filter((c) => c.method !== 'GET' && c.url !== '/api/v1/manifests')).toEqual([]);
  });

  it('queues a manifest with no local modules as plain JSON, follows the build and remembers the form', async () => {
    routes['POST /api/v1/runtime-builds'] = () => queued('b9', 'v0162');
    const router = await render();
    await act(async () => button('Build a runtime')!.click());
    await importManifest('dist:\n  otelcol_version: 0.162.0\n');
    expect(host.textContent).toContain('Your own components');
    await act(async () => inside(form(), 'Build')!.click());
    await settle(() => router.state.location.pathname.startsWith('/runtimes/builds/'));
    expect(posts().map((c) => [c.headers['content-type'], c.body])).toEqual([['application/json', { name: 'v0162', manifest_yaml: 'dist:\n  otelcol_version: 0.162.0\n' }]]);
    expect(router.state.location.pathname).toBe('/runtimes/builds/b9');
    expect(JSON.parse(localStorage.getItem('linea.runtimeBuilds')!)[0]).toMatchObject({ id: 'b9', build: { name: 'v0162', version: '0.162.0' } });
  });

  it('names what is wrong before sending anything', async () => {
    await open();
    await importManifest('');
    await choose(field<HTMLInputElement>('input[aria-label="Runtime name"]'), 'Bad Name');
    await act(async () => inside(form(), 'Build')!.click());
    expect(form().textContent).toContain('Use lower-case letters');
    expect(form().textContent).toContain('Write or import a manifest first.');
    expect(posts()).toEqual([]);
  });

  it('asks for the folder of a local module, packs it and uploads it with the upload header', async () => {
    routes['POST /api/v1/runtime-builds'] = () => queued('b10', 'withmod');
    const router = await render();
    await act(async () => button('Build a runtime')!.click());
    const manifest = 'processors:\n  - gomod: example.com/proc v0.0.0\n    path: ./proc\n';
    await importManifest(manifest);
    await choose(field<HTMLInputElement>('input[aria-label="Runtime name"]'), 'withmod');
    const slot = field('[role="group"][aria-label="Folder of example.com/proc"]');
    expect(slot.textContent).toContain('No folder chosen');

    await act(async () => inside(form(), 'Build')!.click());
    expect(form().textContent).toContain('Choose the folder of example.com/proc');
    expect(posts()).toEqual([]);

    await pick('example.com/proc', [picked('proc/go.mod', 'module example.com/proc\n'), picked('proc/p.go', 'package proc\n'), picked('proc/.git/HEAD', 'ref')]);
    await settle(() => slot.textContent!.includes('2 files'));
    expect(slot.textContent).toContain('proc: 2 files');

    await act(async () => inside(form(), 'Build')!.click());
    await settle(() => router.state.location.pathname.startsWith('/runtimes/builds/'));
    const [up] = posts();
    expect(up.headers['x-linea-upload']).toBe('1');
    expect(up.headers['content-type']).toBeUndefined();
    expect(JSON.parse(await (up.form!.get('request') as Blob).text())).toEqual({
      name: 'withmod', manifest_yaml: manifest, sources: [{ module: 'example.com/proc', part: 'source1' }],
    });
    const archive = up.form!.get('source1') as File;
    expect(archive.name).toBe('source1.tar');
    expect(archive.size % 512).toBe(0);
    expect(up.form!.get('source2')).toBeNull();
  });

  it('refuses a folder whose go.mod declares another module', async () => {
    await open();
    await importManifest('processors:\n  - gomod: example.com/proc v0.0.0\n    path: ./proc\n');
    const slot = field('[role="group"][aria-label="Folder of example.com/proc"]');
    await pick('example.com/proc', [picked('proc/go.mod', 'module example.com/other\n')]);
    await settle(() => slot.textContent!.includes('declares module'));
    expect(slot.textContent).toContain('its go.mod declares module example.com/other, not example.com/proc');
    await choose(field<HTMLInputElement>('input[aria-label="Runtime name"]'), 'x');
    await act(async () => inside(form(), 'Build')!.click());
    expect(posts()).toEqual([]);
  });

  it('shows a refusal of the server', async () => {
    routes['POST /api/v1/runtime-builds'] = () => json({ error: 'The manifest is refused' }, 400);
    await open();
    await importManifest('foo: 1\n');
    await act(async () => inside(form(), 'Build')!.click());
    await settle(() => !!document.querySelector('section[aria-label="New runtime"] [role="alert"]'));
    expect(field('section[aria-label="New runtime"] [role="alert"]').textContent).toContain('The manifest is refused');
  });

  it('keeps the builder out of reach of a role without runtime.build', async () => {
    client.setQueryData(['session'], { permissions: ['run.read', 'runtime.manage'] });
    await render();
    const open = button('Build a runtime')!;
    expect(open.disabled).toBe(true);
    expect(open.title).toContain('runtime.build');
    expect(button('Import image')!.disabled).toBe(false);
  });

  it('lists the builds, newest first, and pages older ones', async () => {
    const b = (id: string, state: string) => ({ id, name: id, collector_version: '0.162.0', state, created: '2026-10-31T10:00:00Z', log_from: 0, log_lines: 0, reused: false, verified: true });
    routes['GET /api/v1/runtime-builds?limit=10'] = () => json({ builds: [b('new1', 'done'), b('new2', 'failed')], next_cursor: 'c2' });
    routes['GET /api/v1/runtime-builds?limit=10&cursor=c2'] = () => json({ builds: [b('old1', 'cancelled')] });
    await render();
    await settle(() => !!document.querySelector('section[aria-label="Builds"] a'));
    const links = () => [...document.querySelectorAll('section[aria-label="Builds"] a')].map((a) => a.getAttribute('href'));
    expect(links()).toEqual(['/runtimes/builds/new1', '/runtimes/builds/new2']);
    await act(async () => button('Show older builds')!.click());
    await settle(() => links().length === 3);
    expect(links()).toEqual(['/runtimes/builds/new1', '/runtimes/builds/new2', '/runtimes/builds/old1']);
    expect(button('Show older builds')).toBeUndefined();
  });

  it('opens the builder seeded from a build behind a rebuild link', async () => {
    routes['GET /api/v1/runtime-builds/b5'] = () => json({ id: 'b5', name: 'again', collector_version: '0.162.0', state: 'done', created: '2026-10-31T10:00:00Z', log_from: 0, log_lines: 0, reused: false, verified: true });
    await render('/runtimes?rebuild=b5');
    await settle(() => !!document.querySelector('section[aria-label="Build a runtime"]:not([hidden])'));
    expect(form().querySelector('h4')!.textContent).toContain('Rebuild again');
    expect(field<HTMLInputElement>('input[aria-label="Runtime name"]').value).toBe('again');
    expect(form().textContent).toContain('This browser did not start that build');
  });
});
