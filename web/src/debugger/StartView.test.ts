// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { StartView } from './StartView';
import { encodeText, openedPatch } from '../workspace/drafts';
import { getWorkspace, updateWorkspace, type WorkspaceState } from '../workspace/store';

const blank: WorkspaceState = {
  configYAML: '', configPath: '', scenarioYAML: '', scenarioPath: '', compare: false,
  baselines: undefined, saved: undefined, editedAt: undefined, lastRunId: undefined,
};

const samples = [
  { id: 'broken-routing', title: 'Debug me: error logs never page', description: 'Find the stage that drops them.', expect: 'fail', config_yaml: 'service: {}\n', scenario_yaml: 'name: broken-routing\n', input_json: '', receiver: 'otlp' },
  { id: 'enrich', title: 'Kubernetes enrichment', description: 'k8sattributes with a mocked pod.', expect: 'pass', config_yaml: 'service: {pipelines: {}}\n', scenario_yaml: 'name: enrich\n', input_json: '', receiver: 'otlp' },
];

type Reply = [number, unknown];
let routes: Record<string, () => Reply>;
let host: HTMLDivElement, root: Root;

const ok = (body: unknown): Reply => [200, body];
const empty = (): Record<string, () => Reply> => ({
  '/api/v1/samples': () => ok(samples),
  '/api/v1/runs': () => ok({ runs: [] }),
  '/api/v1/suites': () => ok([]),
  '/api/v1/captures': () => ok({ items: [] }),
  '/api/v1/runtime-builds': () => ok({ builds: [] }),
  '/api/v1/collector-versions': () => ok({ oldest: '0.100.0', built_in: '0.162.0', verified: ['0.162.0'] }),
  '/api/v1/artifacts': () => [503, { error: 'this server keeps no saved work' }],
});

beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  updateWorkspace(blank);
  routes = empty();
  vi.stubGlobal('fetch', vi.fn(async (url: string) => {
    const path = url.split('?')[0].replace(/\/$/, '');
    const reply = routes[path]?.() ?? [404, { error: `unexpected ${url}` }];
    return new Response(JSON.stringify(reply[1]), { status: reply[0] });
  }));
  host = document.createElement('div');
  document.body.append(host);
  root = createRoot(host);
});
afterEach(() => {
  act(() => root.unmount());
  host.remove();
  vi.unstubAllGlobals();
});

async function settle(until: () => boolean) {
  for (let i = 0; i < 100 && !until(); i++) await act(() => new Promise((r) => setTimeout(r, 5)));
}

async function render(onLeave?: () => void) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const router = createRouter({
    routeTree: createRootRoute({ component: () => createElement(StartView, { onLeave }) }),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  });
  await act(async () => {
    root.render(createElement(QueryClientProvider, { client }, createElement(RouterProvider, { router })));
  });
}

const button = (text: string) => [...host.querySelectorAll('button')].find((b) => b.textContent?.includes(text));
const click = (el: Element | undefined) => {
  if (!el) throw new Error(`nothing to click in ${host.innerHTML}`);
  act(() => (el as HTMLElement).click());
};

const run = (id: string, created: string, assertions?: unknown) => ({
  id, state: 'done', created,
  report: { report_schema: '1', scenario: 'debug-error-logs', result: { status: 'completed', mode: 'scenario' }, assertions },
});

describe('first-time start state', () => {
  it('offers the five starting actions and the bundled examples', async () => {
    await render();
    await settle(() => host.textContent?.includes('Kubernetes enrichment') === true && !host.textContent?.includes('Looking for'));
    const text = host.textContent ?? '';
    expect(text).toContain('Investigate a Collector pipeline');
    for (const t of ['New', 'Open a Collector configuration', 'Try an example', 'Capture telemetry from a Collector', 'Open saved work']) expect(text).toContain(t);
    expect(text).toContain('Has a bug');
    expect(text).toContain('Built-in runtime 0.162.0 verified');
    expect(host.querySelector('a[href="/captures/new"]')).not.toBeNull();
    expect(host.querySelector('a[href="/saved"]')).not.toBeNull();
    expect(text).not.toContain('Continue where you left off');
  });

  it('starts new work from a template with an empty scenario', async () => {
    const left = vi.fn();
    await render(left);
    await settle(() => button('New') !== undefined);
    click(button('New'));
    click(button('Logs pipeline'));
    const ws = getWorkspace();
    expect(ws.configYAML).toContain('logs:');
    expect(ws.configPath).toBe('');
    expect(ws.scenarioYAML).toBe('');
    expect(ws.saved).toEqual({});
    expect(left).toHaveBeenCalledTimes(1);
  });

  it('loads an example straight into an empty workspace and leaves the start state', async () => {
    const left = vi.fn();
    await render(left);
    await settle(() => button('Kubernetes enrichment') !== undefined);
    click(button('Kubernetes enrichment'));
    expect(getWorkspace().configPath).toBe('samples/enrich/config.yaml');
    expect(getWorkspace().scenarioYAML).toBe('name: enrich\n');
    expect(left).toHaveBeenCalledTimes(1);
  });

  it('asks inline before replacing a dirty draft, and keeps it on request', async () => {
    updateWorkspace({ configYAML: 'my edit\n' });
    const left = vi.fn();
    routes['/api/v1/runs'] = () => ok({ runs: [] });
    await render(left);
    // A returning user (a draft) finds the examples behind the header button.
    await settle(() => button('Try an example') !== undefined);
    click(button('Try an example'));
    await settle(() => button('Kubernetes enrichment') !== undefined);
    click(button('Kubernetes enrichment'));
    expect(host.querySelector('[role="alertdialog"]')?.textContent).toContain('Replace your unsaved configuration?');
    expect(getWorkspace().configYAML).toBe('my edit\n');
    click(button('Keep mine'));
    expect(host.querySelector('[role="alertdialog"]')).toBeNull();
    expect(getWorkspace().configYAML).toBe('my edit\n');
    expect(left).not.toHaveBeenCalled();

    click(button('Kubernetes enrichment'));
    click(button('Replace'));
    expect(getWorkspace().configYAML).toBe('service: {pipelines: {}}\n');
    expect(left).toHaveBeenCalledTimes(1);
  });

  it('lists the examples even when every other query fails', async () => {
    for (const k of Object.keys(routes)) if (k !== '/api/v1/samples') routes[k] = () => [500, { error: 'boom' }];
    await render();
    await settle(() => host.textContent?.includes('Kubernetes enrichment') === true);
    expect(host.textContent).toContain('Investigate a Collector pipeline');
    expect(host.textContent).not.toContain('Resume current draft');
  });
});

describe('returning start state', () => {
  beforeEach(() => {
    routes['/api/v1/runs'] = () => ok({ runs: [
      run('run-aaaaaaaa11', '2026-10-23T12:09:00Z', { outcome: 'pass', results: [{ outcome: 'pass' }, { outcome: 'pass' }] }),
      run('run-bbbbbbbb22', '2026-10-23T12:02:00Z', { outcome: 'fail', results: [{ outcome: 'pass' }, { outcome: 'fail' }] }),
    ] });
    routes['/api/v1/captures'] = () => ok({ items: [{
      id: 'cap-0412', state: 'active', terminal: false, target_count: 3, active_count: 3, failed_count: 0, uncertain_count: 0, 
      created_at: new Date(Date.now() - 60_000).toISOString(), expires_at: new Date(Date.now() + 60_000).toISOString(),
      metadata: { name: 'payments-gw', description: '', revision: 1, updated_at: '' }, data: { received_records: 1204 },
    }] });
    routes['/api/v1/runtime-builds'] = () => ok({ builds: [
      { id: 'b-run', name: 'acme', collector_version: '0.137.0', state: 'running', created: '2026-10-23T12:00:00Z', log: [], log_from: 0, log_lines: 1, verified: true, reused: false },
      { id: 'b-bad', name: 'broken', collector_version: '0.137.0', state: 'failed', error: 'go build failed', created: '2026-10-22T12:00:00Z', log: [], log_from: 0, log_lines: 0, verified: true, reused: false },
    ] });
  });

  it('shows the draft, runs, active work and problems with their links', async () => {
    updateWorkspace(openedPatch(getWorkspace(), 'configuration', 'receivers: {}\n', 'checkout-logs.yaml'));
    updateWorkspace(openedPatch(getWorkspace(), 'scenario', 'name: debug-error-logs\ninputs:\n  - receiver: otlp\n    data: "{}"\n', 'debug-error-logs.yaml'));
    const left = vi.fn();
    await render(left);
    await settle(() => host.textContent?.includes('Build acme 0.137.0') === true);
    const text = host.textContent ?? '';
    expect(text).toContain('Continue where you left off');
    expect(text).toContain('checkout-logs.yamlNot saved');
    expect(text).toContain('debug-error-logsNot saved1 input · 0 mocks · 0 assertions');
    expect(text).toContain('2 of 2 passed');
    expect(text).toContain('1 of 2 failed');
    expect(text).toContain('payments-gw');
    expect(text).toContain('3 Collectors · 1204 records');
    expect(text).toContain('Build acme 0.137.0');
    expect(text).toContain('Build broken 0.137.0 failed');
    expect(host.querySelector('a[href="/captures/cap-0412"]')).not.toBeNull();
    expect(host.querySelector('a[href="/runtimes/builds/b-run"]')).not.toBeNull();
    expect(host.querySelector('a[href="/runtimes/builds/b-bad"]')).not.toBeNull();
    expect([...host.querySelectorAll('a')].some((a) => a.getAttribute('href')?.includes('run=run-aaaaaaaa11'))).toBe(true);
    click(button('Continue'));
    expect(left).toHaveBeenCalledTimes(1);
    // Saved work the server cannot list (503): the section is absent, not an error.
    expect(text).not.toContain('Recently used saved work');
  });

  it("shows the draft's own last run with its evidence and when it was edited", async () => {
    updateWorkspace(openedPatch(getWorkspace(), 'configuration', 'receivers: {}\n', 'checkout-logs.yaml'));
    updateWorkspace({ editedAt: new Date(Date.now() - 6 * 60_000).toISOString(), lastRunId: 'run-0wn0000001' });
    const mine = run('run-0wn0000001', '2026-10-20T10:00:00Z');
    routes['/api/v1/runs'] = () => ok({ runs: [run('run-newer00001', '2026-10-23T10:00:00Z')] });
    routes['/api/v1/runs/run-0wn0000001'] = () => ok(mine);
    await render();
    await settle(() => host.textContent?.includes('Last run') === true);
    const card = host.querySelector('[aria-label="Where you left off"]')!;
    expect(card.textContent).toContain('edited 6 min ago');
    expect(card.textContent).toContain('Last run');
    expect(card.textContent).toContain('EvidenceComplete');
    expect(card.textContent).not.toContain('run-newer');
    expect(card.querySelector('a')!.getAttribute('href')).toContain('run=run-0wn0000001');
  });

  it('keeps showing runs when saved work and suites fail', async () => {
    routes['/api/v1/suites'] = () => [500, { error: 'suites are down' }];
    routes['/api/v1/artifacts'] = () => [500, { error: 'store is down' }];
    await render();
    await settle(() => host.textContent?.includes('Saved work could not be listed') === true);
    expect(host.textContent).toContain('Recent runs');
    expect(host.textContent).toContain('2 of 2 passed');
    expect(host.textContent).toContain('store is down');
  });

  it('shows recently used saved work and opens a revision as a draft', async () => {
    routes['/api/v1/artifacts'] = () => ok({
      workspace: { id: 'w', generation: 1, schema: 1 },
      items: [{ id: 'a1', workspace_id: 'w', kind: 'configuration', label: 'checkout-logs.yaml', generation: 3, head_revision_id: 'rev3', created: '2026-10-01T00:00:00Z', updated: '2026-10-22T00:00:00Z' }],
    });
    routes['/api/v1/artifacts/a1/revisions/rev3'] = () => ok({ revision: {}, content_base64: encodeText('exporters: {}\n') });
    const left = vi.fn();
    await render(left);
    const openA1 = () => host.querySelector<HTMLButtonElement>('button[aria-label="Open checkout-logs.yaml"]') ?? undefined;
    await settle(() => openA1() !== undefined);
    expect(host.textContent).toContain('Configuration · rev 3');
    click(openA1());
    await settle(() => getWorkspace().saved?.configuration !== undefined);
    expect(getWorkspace().configYAML).toBe('exporters: {}\n');
    expect(getWorkspace().saved?.configuration).toEqual({ artifactId: 'a1', revisionId: 'rev3' });
    expect(left).toHaveBeenCalledTimes(1);
  });

  it('lists recent test results from the suites and the run list', async () => {
    routes['/api/v1/suites'] = () => ok([{ id: 's1', config: 'C:\\w\\otel.yaml', created: '2026-10-23T12:10:00Z', members: [{ label: 'a', run_id: 'run-aaaaaaaa11' }, { label: 'b', run_id: 'run-bbbbbbbb22' }] }]);
    await render();
    await settle(() => host.textContent?.includes('Recent test results') === true);
    expect(host.textContent).toContain('otel.yaml');
    expect(host.textContent).toContain('2 scenarios');
    expect(host.querySelector('a[href="/saved/suites/s1"]')).not.toBeNull();
  });
});
