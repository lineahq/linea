// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Runtime, RuntimeDefault, RuntimeStorage } from '../../api/types';
import { getWorkspace, updateWorkspace } from '../../workspace/store';
import { RuntimesPage } from './RuntimesPage';
import { StoragePage } from './StoragePage';
import { UpgradeCheckPage } from './UpgradeCheckPage';

const runtime = (name: string, version: string, patch: Partial<Runtime> = {}): Runtime => ({
  name, collector_version: version, verified: true, go_image: 'golang', image_id: `sha256:${name}${version}`, manifest: '',
  module_set: '', product_version: 'dev', protocol: 1, size: 70_000_000, tags: [`linea-runtime:${name}`], ...patch,
});
const support = { oldest: '0.160.0', verified: ['0.162.0'], built_in: '0.162.0' };
const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status });

interface FetchCall { method: string; url: string; body: unknown; signal?: AbortSignal | null }
let calls: FetchCall[];
let runtimes: Runtime[];
let team: RuntimeDefault;
let routes: Record<string, (call: FetchCall) => Response | Promise<Response>>;
let host: HTMLDivElement, root: Root, client: QueryClient;

beforeEach(() => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
  vi.stubGlobal('scrollTo', () => undefined);
  updateWorkspace({ runtime: undefined, configYAML: 'receivers: {}\n', configPath: 'checkout-logs.yaml', scenarioYAML: '', scenarioPath: '' });
  calls = [];
  runtimes = [runtime('acme', '0.136.0'), runtime('v0163', '0.163.0', { verified: false })];
  team = { name: 'built-in' };
  routes = {};
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    const call: FetchCall = { method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined, signal: init?.signal };
    calls.push(call);
    const route = routes[`${method} ${url}`];
    if (route) return route(call);
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
    page('/runtimes/upgrade', UpgradeCheckPage as never),
    page('/runtimes/storage', StoragePage as never),
  ]);
  return createRouter({ routeTree: tree, history: createMemoryHistory({ initialEntries: [entry] }) });
}
type TestRouter = ReturnType<typeof makeRouter>;

async function render(entry: string): Promise<TestRouter> {
  const router = makeRouter(entry);
  await act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(RouterProvider, { router: router as never }))));
  await settle(() => !!host.querySelector('.od-pagehead, .od-wbhead, p'));
  return router;
}
async function settle(until: () => boolean) {
  for (let i = 0; i < 200 && !until(); i++) await act(() => new Promise((r) => setTimeout(r, 5)));
}
const button = (text: string) => [...document.querySelectorAll('button')].find((b) => b.textContent === text);
const path = (r: TestRouter) => r.state.location.pathname + r.state.location.searchStr;

describe('Runtimes page', () => {
  it('lists the runtimes with their state and shows the selected one beside the list', async () => {
    await render('/runtimes');
    await settle(() => document.querySelectorAll('tbody tr').length >= 3);
    await settle(() => !!document.querySelector('.rt-default'));
    const rows = [...document.querySelectorAll('.od-table tbody tr')].slice(0, 3).map((tr) => [...tr.querySelectorAll('td')].map((td) => td.textContent));
    expect(rows).toEqual([
      ['built-inDefault', 'Built-in', '0.162.0', 'Verified'],
      ['acme', 'Local container', '0.136.0', 'Verified'],
      ['v0163', 'Local container', '0.163.0', 'Unverified'],
    ]);
    expect(document.querySelector('.od-subnav a[aria-current=page]')!.textContent).toBe('Runtimes3');
    await act(async () => (document.querySelector('button[aria-label="Show v0163 0.163.0"]') as HTMLButtonElement).click());
    expect(document.querySelector('.rt-detail')!.textContent).toContain('Unverified release');
    expect(document.querySelector('tr[aria-selected=true]')!.textContent).toContain('v0163');
  });

  it('lists the components of the selected runtime and of the built-in one', async () => {
    const catalog = { collector_version: '0.162.0', source: 't', components: [
      { kind: 'receivers', type: 'otlp', gomod: 'go.opentelemetry.io/collector/receiver/otlpreceiver v0.162.0', class: 'core' },
      { kind: 'exporters', type: 'debug', gomod: 'go.opentelemetry.io/collector/exporter/debugexporter v0.162.0', class: 'core' },
    ] };
    routes['GET /api/v1/collector-versions/0.162.0/catalog'] = () => json(catalog);
    routes['POST /api/v1/manifests'] = () => json({ manifest: 'receivers:\n  - gomod: go.opentelemetry.io/collector/receiver/otlpreceiver v0.162.0\n', unknown: [] });
    runtimes = [runtime('mine', '0.162.0', { manifest: 'receivers:\n  - gomod: go.opentelemetry.io/collector/receiver/otlpreceiver v0.162.0\nexporters:\n  - gomod: go.opentelemetry.io/collector/exporter/debugexporter v0.162.0\n  - gomod: example.com/stamp v1.0.0\n    import: example.com/stamp/stampexporter\n' })];
    await render('/runtimes');
    const components = () => document.querySelector('.rt-detail section[aria-label="Components"]')?.textContent ?? '';
    await settle(() => components().includes('otlp'));
    expect(components()).toBe('Components · 1 in its manifestreceivers1otlp');
    expect(calls.find((c) => c.url === '/api/v1/manifests')!.body).toEqual({ version: '0.162.0', preset: 'built-in' });
    await act(async () => (document.querySelector('button[aria-label="Show mine 0.162.0"]') as HTMLButtonElement).click());
    await settle(() => components().includes('debug'));
    expect(components()).toBe('Components · 3 in its manifestreceivers1otlpexporters2debugstampexporter custom');
  });

  it('offers the way back only when it was opened from the debugger, ', async () => {
    const router = await render('/runtimes?from=%2F%3Frun%3Dr1');
    const callout = await vi.waitFor(() => {
      const el = document.querySelector('.od-callout--bar');
      if (!el) throw new Error('no callout');
      return el;
    });
    expect(callout.textContent).toContain('Setting up a runtime for your configuration checkout-logs.yaml');
    await act(async () => button('Back to the experiment')!.click());
    expect(path(router)).toBe('/?run=r1');
  });

  it('has no return callout when opened directly', async () => {
    await render('/runtimes');
    await settle(() => document.querySelectorAll('tbody tr').length >= 3);
    expect(document.querySelector('.od-callout--bar')).toBeNull();
  });

  it('removes a runtime only after the inline confirmation', async () => {
    routes['DELETE /api/v1/runtimes/acme'] = () => new Response(null, { status: 204 });
    await render('/runtimes');
    await settle(() => document.querySelectorAll('tbody tr').length >= 3);
    await act(async () => (document.querySelector('button[aria-label="Show acme 0.136.0"]') as HTMLButtonElement).click());
    await act(async () => button('Remove…')!.click());
    expect(calls.some((c) => c.method === 'DELETE')).toBe(false);
    expect(document.querySelector('[role=alertdialog]')!.textContent).toContain('Runs that used it keep their evidence');
    await act(async () => button('Keep')!.click());
    expect(document.querySelector('[role=alertdialog]')).toBeNull();
    await act(async () => button('Remove…')!.click());
    await act(async () => button('Remove')!.click());
    await settle(() => calls.some((c) => c.method === 'DELETE'));
    expect(calls.filter((c) => c.method === 'DELETE').map((c) => c.url)).toEqual(['/api/v1/runtimes/acme']);
  });

  describe('the team default', () => {
    const catalogRuntime = (name: string): Runtime => runtime(name, '0.162.0', {
      tags: undefined,
      catalog: { identity: 'sha256:abc', image: 'img', registered_at: '2026-01-01T00:00:00Z', registered_by: 'ana', workers: 1, slots: 2 },
    } as Partial<Runtime>);
    const show = async (label: string) => {
      await settle(() => !!document.querySelector(`button[aria-label="${label}"]`));
      await act(async () => (document.querySelector(`button[aria-label="${label}"]`) as HTMLButtonElement).click());
    };

    it('badges the default row, words the Default fact and disables Remove with the reason', async () => {
      runtimes = [catalogRuntime('lab'), catalogRuntime('ci')];
      team = { name: 'lab', set_by: 'ana', set: '2026-03-04T10:00:00Z' };
      await render('/runtimes');
      await settle(() => !!document.querySelector('.rt-default'));
      const badged = [...document.querySelectorAll('.od-table tbody tr')].filter((tr) => tr.querySelector('.rt-default')).map((tr) => tr.querySelector('.rt-rowbtn')!.textContent);
      expect(badged).toEqual(['lab']);
      await show('Show lab 0.162.0');
      expect(document.querySelector('.rt-detail')!.textContent).toMatch(/DefaultRequests that name no runtime run on it · chosen by ana/);
      expect(button('Make default')).toBeUndefined();
      expect(button('Remove…')!.disabled).toBe(true);
      expect(button('Remove…')!.title).toBe('This is the default runtime; make another runtime the default first.');
    });

    it('words the built-in default before the team has chosen one', async () => {
      await render('/runtimes');
      await settle(() => !!document.querySelector('.rt-default'));
      expect(document.querySelector('.rt-detail')!.textContent).toContain('Requests that name no runtime run on it until the team chooses another');
      expect(button('Make default')).toBeUndefined();
    });

    it('makes a catalog runtime the default only after the inline confirmation, and shows the new default', async () => {
      runtimes = [catalogRuntime('lab')];
      routes['PUT /api/v1/runtime-default'] = (call) => {
        team = { name: (call.body as { name: string }).name, set_by: 'me', set: '2026-03-04T10:00:00Z' };
        return json(team);
      };
      updateWorkspace({ runtime: 'v0163' });
      await render('/runtimes');
      await show('Show lab 0.162.0');
      expect(button('Make default')).toBeDefined();
      await act(async () => button('Make default')!.click());
      expect(calls.some((c) => c.method === 'PUT')).toBe(false);
      expect(document.querySelector('[role=alertdialog]')!.textContent).toContain('Make lab the default runtime? Every run, validation and preview that names no runtime runs on it, for the whole team.');
      await act(async () => button('Cancel')!.click());
      expect(document.querySelector('[role=alertdialog]')).toBeNull();
      await act(async () => button('Make default')!.click());
      await act(async () => button('Confirm')!.click());
      await settle(() => calls.some((c) => c.method === 'PUT'));
      expect(calls.filter((c) => c.method === 'PUT')).toMatchObject([{ url: '/api/v1/runtime-default', body: { name: 'lab' } }]);
      await settle(() => [...document.querySelectorAll('.rt-default')].length === 1 && !!document.querySelector('tr[aria-selected=true] .rt-default'));
      expect(document.querySelector('tr[aria-selected=true] .rt-default')).not.toBeNull();
      expect(button('Make default')).toBeUndefined();
      expect(getWorkspace().runtime).toBe('v0163');
    });

    it('makes the built-in runtime the default again', async () => {
      runtimes = [catalogRuntime('lab')];
      team = { name: 'lab' };
      routes['PUT /api/v1/runtime-default'] = () => json({ name: 'built-in' });
      await render('/runtimes');
      await settle(() => !!document.querySelector('.rt-default'));
      await show('Show built-in 0.162.0');
      await act(async () => button('Make default')!.click());
      expect(document.querySelector('[role=alertdialog]')!.textContent).toContain('Make built-in the default runtime?');
      await act(async () => button('Confirm')!.click());
      await settle(() => calls.some((c) => c.method === 'PUT'));
      expect(calls.find((c) => c.method === 'PUT')!.body).toEqual({ name: 'built-in' });
    });

    it('offers no Make default on a local container, and shows the server refusal', async () => {
      runtimes = [catalogRuntime('lab'), runtime('acme', '0.136.0')];
      routes['PUT /api/v1/runtime-default'] = () => json({ code: 'not_found', error: 'runtime "lab" is not in the catalog' }, 404);
      await render('/runtimes');
      await show('Show acme 0.136.0');
      expect(button('Make default')).toBeUndefined();
      await show('Show lab 0.162.0');
      await act(async () => button('Make default')!.click());
      await act(async () => button('Confirm')!.click());
      await settle(() => !!document.querySelector('.rt-detail [role=alert]'));
      expect(document.querySelector('.rt-detail [role=alert]')!.textContent).toContain('runtime "lab" is not in the catalog');
      expect(document.querySelector('[role=alertdialog]')).not.toBeNull();
    });

    it('keeps Make default visible but disabled with the reason without runtime.manage', async () => {
      runtimes = [catalogRuntime('lab')];
      client.setQueryData(['session'], { permissions: ['runtime.read'] });
      await render('/runtimes');
      await show('Show lab 0.162.0');
      const b = button('Make default')!;
      expect(b.disabled).toBe(true);
      expect(b.title).not.toBe('');
    });
  });

  it('says built runtimes are unavailable without hiding the built-in one', async () => {
    routes['GET /api/v1/runtimes'] = () => json({ code: 'unavailable', error: 'runtimes need Docker' }, 503);
    await render('/runtimes');
    await settle(() => !!document.querySelector('[role=alert]'));
    expect(document.querySelector('[role=alert]')!.textContent).toContain('Runtimes are unavailable on this server: runtimes need Docker');
    expect(document.querySelectorAll('.od-table tbody tr')).toHaveLength(1);
  });
});

describe('Upgrade check page', () => {
  const result = {
    upgrade_check_schema: '0.1', verdict: 'behavior_changes',
    from: { name: 'built-in', collector_version: '0.162.0', verified: true },
    to: { name: 'v0163', collector_version: '0.163.0', verified: false },
    configuration: { from_valid: true, to_valid: true, added: [{ severity: 'warning', code: 'deprecated', message: 'old key' }], removed: [] },
    feature_gates: { changes: [{ id: 'processor.transform.Flag', component: 'processors/transform', from: { stage: 'alpha', enabled: false }, to: { stage: 'beta', enabled: true } }] },
    scenarios: [{ name: 's.yaml', outcome: 'different', inputs_identical: true, before: 'run-1', after: 'run-2', before_assertions: 'pass', after_assertions: 'pass' }],
  };

  /** A check that waits until the test answers it, or its request is aborted. */
  function pendingCheck() {
    let answer!: (r: Response) => void;
    routes['POST /api/v1/upgrade-checks'] = (call) => new Promise<Response>((resolve, reject) => {
      answer = resolve;
      call.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')));
    });
    return { answer: (v: unknown) => answer(json(v)) };
  }
  const start = async () => {
    await settle(() => !!button('Check upgrade') && document.querySelectorAll('select option').length > 2);
    await act(async () => button('Check upgrade')!.click());
    await settle(() => calls.some((c) => c.url === '/api/v1/upgrade-checks'));
  };

  it('checks the drafts and words the verdict, the facets and the differences', async () => {
    updateWorkspace({ scenarioYAML: 'inputs: []\n', scenarioPath: 's.yaml' });
    const check = pendingCheck();
    await render('/runtimes/upgrade');
    await start();
    expect(calls.find((c) => c.url === '/api/v1/upgrade-checks')!.body).toEqual({
      to: 'v0163', config_yaml: 'receivers: {}\n', config_label: 'checkout-logs.yaml', scenarios: [{ scenario_yaml: 'inputs: []\n', scenario_label: 's.yaml' }],
    });
    await act(async () => check.answer(result));
    await settle(() => !!document.querySelector('.od-facets'));
    const facets = [...document.querySelectorAll('.od-facet')].map((f) => f.textContent);
    expect(facets).toEqual([
      'ConfigurationValid on both1 new diagnostic on the target',
      'Behavior1 differentof 1 scenario',
      'Feature gates1 changedprocessor.transform.Flag',
      'CertaintyCompleteevery part was compared',
    ]);
    expect(document.querySelector('.od-callout--fail')!.textContent).toContain('Behavior changes');
    const link = document.querySelector<HTMLAnchorElement>('a[href*="compare=run-2"]')!;
    expect(link.getAttribute('href')).toContain('run=run-1');
    expect(document.body.textContent).toContain('processor.transform.Flag');
    expect(document.body.textContent).toContain('alpha · off');
    expect(document.body.textContent).toContain('old key');
    updateWorkspace({ scenarioYAML: '', scenarioPath: '' });
  });

  it('warns while the check runs, asks before leaving, and cancels the request on leaving', async () => {
    pendingCheck();
    const router = await render('/runtimes/upgrade');
    await start();
    expect(document.querySelector('.od-callout--warn')!.textContent).toContain('Leaving this page cancels the check');
    const signal = calls.find((c) => c.url === '/api/v1/upgrade-checks')!.signal!;
    expect(signal.aborted).toBe(false);

    // Navigating away is held back and asked about inline.
    await act(async () => { void router.navigate({ to: '/runtimes/storage' as never }); });
    await settle(() => !!document.querySelector('[role=alertdialog]'));
    expect(path(router)).toBe('/runtimes/upgrade');
    expect(document.querySelector('[role=alertdialog]')!.textContent).toContain('Cancel the check and leave for /runtimes/storage?');
    expect(signal.aborted).toBe(false);

    // Keeping the check goes on checking.
    await act(async () => button('Keep checking')!.click());
    expect(document.querySelector('[role=alertdialog]')).toBeNull();
    expect(path(router)).toBe('/runtimes/upgrade');
    expect(signal.aborted).toBe(false);

    // Confirming aborts the request and leaves.
    await act(async () => { void router.navigate({ to: '/runtimes/storage' as never }); });
    await settle(() => !!document.querySelector('[role=alertdialog]'));
    await act(async () => button('Cancel the check and leave')!.click());
    await settle(() => path(router) === '/runtimes/storage');
    expect(path(router)).toBe('/runtimes/storage');
    expect(signal.aborted).toBe(true);
  });

  it('stops the check on request and does not block leaving afterwards', async () => {
    pendingCheck();
    const router = await render('/runtimes/upgrade');
    await start();
    const signal = calls.find((c) => c.url === '/api/v1/upgrade-checks')!.signal!;
    await act(async () => button('Stop the check')!.click());
    expect(signal.aborted).toBe(true);
    expect(document.body.textContent).toContain('Check cancelled');
    expect(document.querySelector('.od-callout--bar')).toBeNull();
    await act(async () => { void router.navigate({ to: '/runtimes/storage' as never }); });
    await settle(() => path(router) === '/runtimes/storage');
    expect(path(router)).toBe('/runtimes/storage');
  });

  it('shows a refused check as an alert and does not block leaving', async () => {
    routes['POST /api/v1/upgrade-checks'] = () => json({ code: 'invalid_argument', error: 'unknown runtime "v0163"' }, 400);
    const router = await render('/runtimes/upgrade');
    await start();
    await settle(() => !!document.querySelector('[role=alert]'));
    expect(document.querySelector('[role=alert]')!.textContent).toContain('unknown runtime "v0163"');
    await act(async () => { void router.navigate({ to: '/runtimes/storage' as never }); });
    await settle(() => path(router) === '/runtimes/storage');
    expect(path(router)).toBe('/runtimes/storage');
  });

  it('names a problem before starting', async () => {
    updateWorkspace({ configYAML: '' });
    await render('/runtimes/upgrade');
    await settle(() => !!button('Check upgrade'));
    await act(async () => button('Check upgrade')!.click());
    expect(document.querySelector('[role=alert]')!.textContent).toBe('Write or open a configuration first.');
    expect(calls.some((c) => c.url === '/api/v1/upgrade-checks')).toBe(false);
  });
});

describe('Storage page', () => {
  const stored = (name: string, state: string, size: number, inUse = false) => ({
    ...runtime(name, '0.162.0'), state, in_use: inUse, size,
  });
  const storage: RuntimeStorage = {
    reclaimable: 3_000_000_000,
    runtimes: [stored('a', 'named', 3_000_000_000, true), stored('a', 'replaced', 2_000_000_000)],
    caches: [{ name: 'go-mod', size: 1_100_000_000 }],
    leftovers: [],
  };

  it('measures on arrival and removes only the ticked items, after the inline confirmation', async () => {
    routes['GET /api/v1/runtime-storage'] = () => json(storage);
    routes['POST /api/v1/runtime-storage/prune'] = () => json({ removed: [{ kind: 'runtime', name: 'a', image_id: 'sha256:x', size: 2_000_000_000 }], kept: [], reclaimed: 2_000_000_000 });
    await render('/runtimes/storage');
    await settle(() => !!document.querySelector('.od-meter'));
    expect(document.querySelector('.od-meter')!.getAttribute('aria-label')).toBe('Runtime images 5.0 GB, Build caches 1.1 GB');
    const boxes = [...document.querySelectorAll<HTMLInputElement>('.od-table input[type=checkbox]')];
    expect(boxes.map((b) => [b.getAttribute('aria-label'), b.checked])).toEqual([['1 unused runtime image', true], ['Build cache', false]]);
    expect(document.querySelector('.od-pager')!.textContent).toContain('Selected frees 2.0 GB');

    await act(async () => button('Clean up 2.0 GB')!.click());
    expect(calls.some((c) => c.method === 'POST')).toBe(false);
    expect(document.querySelector('[role=alertdialog]')!.textContent).toContain('Remove 1 runtime image (2.0 GB)? Runs keep their evidence.');
    await act(async () => button('Keep')!.click());
    expect(document.querySelector('[role=alertdialog]')).toBeNull();

    await act(async () => boxes[1].click());
    expect(document.body.textContent).toContain('The next build downloads its modules again');
    await act(async () => button('Clean up 3.1 GB')!.click());
    await act(async () => button('Clean up')!.click());
    await settle(() => calls.some((c) => c.method === 'POST'));
    expect(calls.find((c) => c.method === 'POST')!.body).toEqual({ replaced: true, stale: false, caches: true });
    await settle(() => !!document.querySelector('[role=status]'));
    expect(document.querySelector('.od-callout--pass')!.textContent).toContain('Removed 1 object, reclaiming 2.0 GB.');
  });

  it('says when Docker cannot be measured', async () => {
    routes['GET /api/v1/runtime-storage'] = () => json({ code: 'unavailable', error: 'runtimes need Docker' }, 503);
    await render('/runtimes/storage');
    await settle(() => !!document.querySelector('[role=alert]'));
    expect(document.querySelector('[role=alert]')!.textContent).toContain('Runtimes are unavailable on this server: runtimes need Docker');
  });
});
