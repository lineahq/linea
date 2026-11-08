// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { CaptureDetailPage } from './CaptureDetailPage';
import { CapturesPage } from './CapturesPage';
import { NewCapturePage } from './NewCapturePage';
import { getWorkspace, updateWorkspace } from '../../workspace/store';

const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status });
const stats = { enqueued_records: 10, enqueued_bytes: 100, filter_errors: 0, selected: 10 };
const target = (id: string, patch: Record<string, unknown> = {}) => ({
  collector_id: id,
  generation: 1,
  source: { pod: id, collector_group: '', collector_version: '0.162.0' },
  state: 'active',
  terminal: false,
  stats,
  ...patch,
});
const data = { enabled: true, state: 'open', received_records: 4, received_bytes: 2048, rejected_records: 0, record_limit: 10000, byte_limit: 8388608, memory_only: false, contains_raw: false };
const limits = { total_records: 5000, total_bytes: 50 * 1024 * 1024, records_per_second: 5000, bytes_per_second: 1048576, record_bytes: 65536 };
const session = (patch: Record<string, unknown> = {}) => ({
  id: 'cap-0412',
  caller: { principal: 'local-owner', client: 'http' },
  created_at: '2026-10-15T14:30:02Z',
  expires_at: '2026-10-15T14:32:02Z',
  data,
  requested_limits: limits,
  selection: { collector_ids: ['gw-0', 'gw-1', 'gw-2'], capture_points: [{ pipeline: 'logs', point: 'output' }], signals: ['logs'], duration_seconds: 120, filter: {}, limits, sampling: { mode: 'hash', rate: 1 } },
  state: 'active',
  terminal: false,
  targets: [target('gw-0'), target('gw-1'), target('gw-2')],
  ...patch,
});

interface Call { method: string; url: string; body: unknown }
let calls: Call[];
let routes: Record<string, (call: Call) => Response>;
let host: HTMLDivElement, root: Root, client: QueryClient;

beforeEach(() => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
  vi.stubGlobal('scrollTo', () => undefined);
  calls = [];
  routes = {};
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    const call = { method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined };
    calls.push(call);
    const hit = Object.keys(routes).find((k) => `${method} ${url}`.startsWith(k));
    if (hit) return routes[hit](call);
    if (url.startsWith('/api/v1/collectors')) return json({ enabled: true, items: [] });
    if (url.startsWith('/api/v1/collector-groups')) return json({ items: [] });
    if (url.includes('/records')) return json({ session_id: 'cap-0412', watermark: 0, items: [] });
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
  const page = (path: string, component: () => unknown) => createRoute({ getParentRoute: () => rootRoute, path, component: component as never });
  const tree = rootRoute.addChildren([
    page('/', () => createElement('p', null, 'The debugger')),
    page('/fleet', () => createElement('p', null, 'Fleet')),
    page('/captures', CapturesPage),
    page('/captures/$captureId', CaptureDetailPage),
    page('/captures/new', NewCapturePage),
  ]);
  const router = createRouter({ routeTree: tree, history: createMemoryHistory({ initialEntries: [entry] }) });
  await act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(RouterProvider, { router: router as never }))));
  return router;
}
const button = (text: string) => [...document.querySelectorAll('button')].find((b) => b.textContent?.trim() === text) as HTMLButtonElement | undefined;
const click = async (el: Element | null | undefined) => act(async () => (el as HTMLElement).click());

describe('Capture detail', () => {
  it('waits for the Collectors to release a stopping capture and never claims they are still sending', async () => {
    routes['GET /api/v1/captures/cap-0412'] = () =>
      json(session({
        state: 'stopping',
        stop_reason: 'stop_requested',
        targets: [target('gw-0', { state: 'completed', terminal: true }), target('gw-1', { state: 'completed', terminal: true }), target('gw-2', { state: 'stopping' })],
      }));
    await render('/captures/cap-0412');
    await settle(() => !!document.querySelector('.od-callout--bar'));
    const text = document.body.textContent ?? '';
    expect(document.querySelector('.od-wbhead .od-chip')!.textContent).toBe('Stopping');
    expect(text).toContain('Stop requested · waiting for the Collectors to release the capture');
    expect(text).toContain('stop requested · 1 Collector still open');
    expect(text).not.toMatch(/still sending/i);
    expect(button('Stop')!.disabled).toBe(true);
  });

  it('says why a capture failed and shows targets without stats', async () => {
    routes['GET /api/v1/captures/cap-0412'] = () =>
      json(session({ state: 'failed', terminal: true, reason: 'collector_rejected', data: { ...data, state: 'frozen' }, targets: [target('gw-0', { state: 'failed', terminal: true, stats: undefined })] }));
    await render('/captures/cap-0412');
    await settle(() => !!document.querySelector('.od-callout--bar'));
    expect(document.querySelector('.od-wbhead .od-chip')!.textContent).toBe('Failed');
    expect(document.querySelector('.od-callout--bar')!.textContent).toContain('A Collector rejected the capture.');
    expect(document.querySelector('[aria-label="Per target"] tbody')!.textContent).toContain('—');
  });

  it('asks before stopping and sends nothing until confirmed', async () => {
    routes['GET /api/v1/captures/cap-0412'] = () => json(session());
    routes['POST /api/v1/captures/cap-0412/stop'] = () => json(session({ state: 'stopping' }));
    await render('/captures/cap-0412');
    await settle(() => !!button('Stop'));
    await click(button('Stop'));
    const dialog = document.querySelector('[role=alertdialog]')!;
    expect(dialog.textContent).toContain('Ask every Collector to stop');
    expect(calls.some((c) => c.method === 'POST')).toBe(false);
    await click(button('Keep capturing'));
    expect(document.querySelector('[role=alertdialog]')).toBeNull();
    expect(calls.some((c) => c.method === 'POST')).toBe(false);
    await click(button('Stop'));
    await click(button('Stop capture'));
    await settle(() => calls.some((c) => c.method === 'POST' && c.url.endsWith('/stop')));
  });

  it('keeps Delete closed while the capture is live and asks before deleting a finished one', async () => {
    routes['GET /api/v1/captures/cap-0412'] = () => json(session({ state: 'completed', terminal: true, data: { ...data, state: 'frozen' } }));
    routes['DELETE /api/v1/captures/cap-0412'] = () => json({ deleted: true });
    routes['GET /api/v1/captures?'] = () => json({ items: [] });
    const router = await render('/captures/cap-0412');
    await settle(() => !!button('Delete'));
    expect(button('Delete')!.disabled).toBe(false);
    await click(button('Delete'));
    expect(document.querySelector('[role=alertdialog]')!.textContent).toContain('Delete this capture');
    expect(calls.some((c) => c.method === 'DELETE')).toBe(false);
    await click(button('Delete capture'));
    await settle(() => router.state.location.pathname === '/captures');
  });

  it('explains a capture that no longer exists', async () => {
    routes['GET /api/v1/captures/cap-0412'] = () => json({ error: 'capture not found' }, 404);
    await render('/captures/cap-0412');
    await settle(() => !!document.querySelector('[role=alert]'));
    expect(document.querySelector('[role=alert]')!.textContent).toContain('no longer exists');
  });
});

describe('Capture records', () => {
  const stream = { collector_id: 'gw-0', pipeline: 'logs', point: 'output', signal: 'logs' };
  const log = (body: string) => ({ resourceLogs: [{ resource: { attributes: [{ key: 'service.name', value: { stringValue: 'pay' } }] }, scopeLogs: [{ logRecords: [{ body: { stringValue: body }, severityNumber: 9, attributes: [{ key: 'http.route', value: { stringValue: '/pay' } }] }] }] }] });
  const record = (sequence: number, body: string, patch: Record<string, unknown> = {}) => ({ sequence, stream, payload: log(body), received_at: '2026-10-15T14:30:05Z', sensitivity: 'redacted', ...patch });
  /** Serves `items` the way the platform pages them: by cursor and limit. */
  function serve(items: ReturnType<typeof record>[]) {
    // First, since routes match by prefix in order and the capture's own route is a prefix.
    routes = { 'GET /api/v1/captures/cap-0412/records': (c) => {
      const q = new URL(c.url, 'http://x').searchParams;
      const from = Number(q.get('cursor') || 0);
      const limit = Number(q.get('limit'));
      const page = items.slice(from, from + limit);
      return json({ session_id: 'cap-0412', watermark: items.length, items: page, ...(from + limit < items.length ? { next_cursor: String(from + limit) } : {}) });
    }, ...routes };
  }
  async function type(el: HTMLInputElement, value: string) {
    await act(async () => {
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(el, value);
      el.dispatchEvent(new Event('input', { bubbles: true }));
    });
  }
  const rows = () => [...document.querySelectorAll('[aria-label="Received records"] tbody tr[aria-selected]')];

  it('opens a record from anywhere on its row, straight to its details, and shows raw in its own column', async () => {
    routes['GET /api/v1/captures/cap-0412'] = () => json(session());
    serve([record(1, 'Request completed', { sensitivity: 'raw' }), record(2, 'Cache hit')]);
    await render('/captures/cap-0412');
    await settle(() => rows().length === 2);
    const heads = [...document.querySelectorAll('[aria-label="Received records"] thead th')].map((th) => th.textContent);
    expect(heads).toContain('Data');
    expect(rows()[0].children[1].textContent).toBe('INFO');
    expect(rows()[0].children[3].textContent).toBe('Raw');
    expect(rows()[1].children[3].textContent).toBe('Redacted');
    await click(rows()[0].children[2]);
    const details = document.querySelector('[aria-label="Capture record 1"]')!;
    expect(details.textContent).toContain('http.route');
    expect(details.textContent).toContain('service.name');
    expect(details.querySelector('input[type=search]')).toBeNull();
    // The checkbox selects without opening the row.
    await click(document.querySelector('[aria-label="Select record 2"]'));
    expect(document.querySelector('[aria-label="Capture record 2"]')).toBeNull();
    expect(document.body.textContent).toContain('1 record selected');
  });

  it('filters and selects across the whole capture, not only the page on screen', async () => {
    routes['GET /api/v1/captures/cap-0412'] = () => json(session({ data: { ...data, received_records: 130 } }));
    serve(Array.from({ length: 130 }, (_, i) => record(i + 1, i >= 120 ? `payment failed ${i + 1}` : `ok ${i + 1}`)));
    await render('/captures/cap-0412');
    await settle(() => rows().length === 25);
    await type(document.querySelector('[aria-label="Filter all records"]') as HTMLInputElement, 'PAYMENT failed');
    await settle(() => rows().length === 10);
    expect(calls.filter((c) => c.url.includes('limit=100')).length).toBe(2);
    expect(rows()[0].textContent).toContain('payment failed 121');
    await click(button('Select 10 matching'));
    await settle(() => document.body.textContent!.includes('10 records selected'));
    expect(document.body.textContent).toContain('Selected all 10 matching records.');
  });

  it('says what to do next once the capture finished, samples it and warns about a large selection', async () => {
    routes['GET /api/v1/captures/cap-0412'] = () => json(session({ state: 'completed', terminal: true, data: { ...data, state: 'frozen', received_records: 250 } }));
    serve(Array.from({ length: 250 }, (_, i) => record(i + 1, `line ${i + 1}`)));
    await render('/captures/cap-0412');
    await settle(() => !!document.querySelector('[aria-label="Next steps"]'));
    expect(document.querySelector('[aria-label="Next steps"]')!.textContent).toContain('nothing to save');
    await click(button('Select a sample of 100'));
    await settle(() => document.body.textContent!.includes('100 records selected'));
    expect(document.querySelector('[aria-label="Next steps"]')).toBeNull();
    expect(document.body.textContent).not.toContain('make a large scenario');
    await click(button('Clear'));
    await click(button('Select all 250'));
    await settle(() => document.body.textContent!.includes('250 records selected'));
    expect(document.body.textContent).toContain('make a large scenario');
    await click(button('Take a sample of 100 instead'));
    await settle(() => document.body.textContent!.includes('100 records selected'));
  });

  it('creates a new configuration and scenario from the records, asking before it replaces unsaved edits', async () => {
    const original = getWorkspace();
    act(() => updateWorkspace({ configYAML: 'receivers: {otlp: {}}\n', scenarioYAML: '# my unsaved scenario\nversion: 1\ninputs: []\n', baselines: { config: 'receivers: {otlp: {}}\n', scenario: '' } }));
    try {
      routes['GET /api/v1/captures/cap-0412'] = () => json(session({ state: 'completed', terminal: true, data: { ...data, state: 'frozen', received_records: 2 } }));
      serve([record(1, 'a'), record(2, 'b')]);
      routes['POST /api/v1/config/graph'] = () => json({ valid: true, topology: { components: [{ ref: 'receiver/otlp', id: 'otlp', kind: 'receiver' }], pipelines: [{ id: 'logs', signal: 'logs', receivers: ['receiver/otlp'] }] } });
      routes['POST /api/v1/captures/cap-0412/fixtures'] = (c) => {
        const sel = (c.body as { selection: { sequences: number[]; receiver: string } }).selection;
        return json({ fixture_json: '{"resourceLogs":[]}', setup_yaml: '', receiver: sel.receiver, signal: 'logs', derivation: { session_id: 'cap-0412', selection: sel, source_digest: 'a'.repeat(64), digest: 'b'.repeat(64), destination_config_digest: 'c'.repeat(64), records: sel.sequences.length }, limitations: [] });
      };
      const router = await render('/captures/cap-0412');
      await settle(() => !!button('Select all 2'));
      await click(button('Select all 2'));
      await settle(() => !!button('Use as inputs'));
      await click(button('Use as inputs'));
      await settle(() => !!button('Create and open it') && !button('Create and open it')!.disabled);
      expect(document.querySelector('input[name=dest]:checked')!.parentElement!.textContent).toContain('New configuration and scenario');
      // A new configuration needs no receiver choice and no check of the open one.
      expect(document.querySelector('select[aria-label^="Receiver for"]')).toBeNull();
      await click(button('Create and open it'));
      await settle(() => !!document.querySelector('[aria-label="Replace unsaved work"]'));
      expect(getWorkspace().scenarioYAML).toContain('# my unsaved scenario');
      expect(calls.some((c) => c.url.endsWith('/config/graph'))).toBe(false);
      const derive = calls.find((c) => c.url.endsWith('/fixtures'))!.body as { config_yaml: string; setup_yaml: string; selection: { receiver: string } };
      expect(derive).toMatchObject({ setup_yaml: '', selection: { receiver: 'otlp' } });
      expect(derive.config_yaml).toContain('logs:');
      await click(button('Replace'));
      await settle(() => router.state.location.pathname === '/');
      const next = getWorkspace();
      expect(next.scenarioYAML).not.toContain('# my unsaved scenario');
      expect(next.scenarioYAML).toContain('receiver: otlp');
      expect(next.scenarioYAML).toContain('capture_origins');
      expect(next.configYAML).toBe(derive.config_yaml);
      expect(next.scenarioPath).toBe('');
      expect(next.configPath).toBe('');
    } finally {
      act(() => updateWorkspace({ ...original }));
    }
  });

  it('adds a missing signal pipeline to the open configuration on request', async () => {
    const original = getWorkspace();
    const config = 'receivers:\n  otlp:\n    protocols: {grpc: {}}\nexporters:\n  debug: {}\nservice:\n  pipelines:\n    traces:\n      receivers: [otlp]\n      exporters: [debug]\n';
    act(() => updateWorkspace({ configYAML: config, scenarioYAML: 'version: 1\ninputs: []\n', baselines: { config, scenario: 'version: 1\ninputs: []\n' } }));
    try {
      routes['GET /api/v1/captures/cap-0412'] = () => json(session({ state: 'completed', terminal: true, data: { ...data, state: 'frozen', received_records: 2 } }));
      serve([record(1, 'a'), record(2, 'b')]);
      routes['POST /api/v1/config/graph'] = (c) => {
        const logs = (c.body as { config_yaml: string }).config_yaml.includes('logs:');
        const pipelines = [{ id: 'traces', signal: 'traces', receivers: ['receiver/otlp'] }, ...(logs ? [{ id: 'logs', signal: 'logs', receivers: ['receiver/otlp'] }] : [])];
        return json({ valid: true, topology: { components: [{ ref: 'receiver/otlp', id: 'otlp', kind: 'receiver' }], pipelines } });
      };
      routes['POST /api/v1/captures/cap-0412/fixtures'] = (c) => {
        const sel = (c.body as { selection: { sequences: number[]; receiver: string } }).selection;
        return json({ fixture_json: '{"resourceLogs":[]}', setup_yaml: '', receiver: sel.receiver, signal: 'logs', derivation: { session_id: 'cap-0412', selection: sel, source_digest: 'a'.repeat(64), digest: 'b'.repeat(64), destination_config_digest: 'c'.repeat(64), records: sel.sequences.length }, limitations: [] });
      };
      const router = await render('/captures/cap-0412');
      await settle(() => !!button('Select all 2'));
      await click(button('Select all 2'));
      await settle(() => !!button('Use as inputs'));
      await click(button('Use as inputs'));
      await settle(() => !!document.querySelector('input[name=dest]'));
      await click(document.querySelectorAll<HTMLInputElement>('input[name=dest]')[1]);
      await settle(() => !!button('Add a logs pipeline'));
      expect(button('Add as inputs and open the scenario')!.disabled).toBe(true);
      await click(button('Add a logs pipeline'));
      expect(getWorkspace().configYAML).toContain('    logs:\n      receivers: [otlp]\n      exporters: [debug]');
      await settle(() => !button('Add as inputs and open the scenario')!.disabled);
      await click(button('Add as inputs and open the scenario'));
      await settle(() => router.state.location.pathname === '/');
      expect(getWorkspace().scenarioYAML).toContain('receiver: otlp');
    } finally {
      act(() => updateWorkspace({ ...original }));
    }
  });
});

describe('Captures', () => {
  it('shows an empty state with the way to start one', async () => {
    routes['GET /api/v1/captures'] = () => json({ items: [] });
    await render('/captures');
    await settle(() => document.body.textContent!.includes('No captures yet'));
    expect(document.querySelector('a[href="/captures/new"]')).not.toBeNull();
  });
});

describe('New capture', () => {
  const collector = { id: 'gw-0', generation: 1, agent_id: 'a1', credential_id: 'cred', pod: 'gw-0', cluster: 'prod', namespace: 'pay', collector_group: '', collector_version: '0.162.0', distribution_version: '', enabled: true, point_count: 1, raw_point_count: 0 };
  const points = { items: [{ pipeline: 'logs', point: 'output', signal: 'logs', sensitivity: 'redacted' }] };

  it('preselects the Collectors from the link and starts only once a point is chosen', async () => {
    routes['GET /api/v1/collectors?'] = () => json({ enabled: true, items: [collector] });
    routes['GET /api/v1/collectors/gw-0/capture-points'] = () => json(points);
    routes['POST /api/v1/captures'] = () => json(session());
    routes['GET /api/v1/captures/cap-0412'] = () => json(session());
    const router = await render('/captures/new?collectors=gw-0');
    await settle(() => !!document.querySelector('input[aria-label="logs output logs"]'));
    expect(button('Start capture')!.disabled).toBe(true);
    expect(document.querySelector('[aria-label="Before you can start"]')!.textContent).toContain('Choose at least one capture point.');
    await click(document.querySelector('input[aria-label="logs output logs"]')!);
    expect(button('Start capture')!.disabled).toBe(false);
    await click(button('Start capture'));
    await settle(() => calls.some((c) => c.method === 'POST' && c.url === '/api/v1/captures'));
    const body = calls.find((c) => c.method === 'POST' && c.url === '/api/v1/captures')!.body as { collector_ids: string[]; duration_seconds: number };
    expect(body.collector_ids).toEqual(['gw-0']);
    expect(body.duration_seconds).toBe(60);
    await settle(() => router.state.location.pathname === '/captures/cap-0412');
  });

  it('asks before leaving with unsaved choices, and leaves only on request', async () => {
    routes['GET /api/v1/collectors?'] = () => json({ enabled: true, items: [collector] });
    routes['GET /api/v1/collectors/gw-0/capture-points'] = () => json(points);
    routes['GET /api/v1/captures'] = () => json({ items: [] });
    const router = await render('/captures/new?collectors=gw-0');
    await settle(() => !!document.querySelector('input[aria-label="logs output logs"]'));
    await click(document.querySelector('input[aria-label="logs output logs"]')!);
    const cancel = () => [...document.querySelectorAll('a')].find((a) => a.textContent === 'Cancel')!;
    await click(cancel());
    await settle(() => !!document.querySelector('[aria-label="Leave New capture"]'));
    expect(router.state.location.pathname).toBe('/captures/new');
    await click(button('Keep editing'));
    expect(document.querySelector('[aria-label="Leave New capture"]')).toBeNull();
    expect(router.state.location.pathname).toBe('/captures/new');
    expect(document.querySelector<HTMLInputElement>('input[aria-label="logs output logs"]')!.checked).toBe(true);
    await click(cancel());
    await settle(() => !!button('Discard and leave'));
    await click(button('Discard and leave'));
    await settle(() => router.state.location.pathname === '/captures');
  });

  it('leaves at once when nothing was chosen', async () => {
    routes['GET /api/v1/collectors?'] = () => json({ enabled: true, items: [collector] });
    routes['GET /api/v1/collectors/gw-0/capture-points'] = () => json(points);
    routes['GET /api/v1/captures'] = () => json({ items: [] });
    const router = await render('/captures/new?collectors=gw-0');
    await settle(() => !!document.querySelector('input[aria-label="logs output logs"]'));
    await click([...document.querySelectorAll('a')].find((a) => a.textContent === 'Cancel')!);
    await settle(() => router.state.location.pathname === '/captures');
  });

  it('starts a fleet group capture by naming the group', async () => {
    routes['GET /api/v1/collector-groups?'] = () => json({ items: [{ id: 'g1', name: 'payments-gw', kind: 'static', revision: 1, agent_ids: ['a1'] }] });
    routes['GET /api/v1/collector-groups/g1/members'] = () => json({ group_id: 'g1', items: [{ agent_id: 'a1', connected: true, collector_ids: ['gw-0'] }] });
    routes['GET /api/v1/collector-groups/g1'] = () => json({ id: 'g1', name: 'payments-gw', kind: 'static', revision: 1, agent_ids: ['a1'] });
    routes['GET /api/v1/collectors?'] = () => json({ enabled: true, items: [collector] });
    routes['GET /api/v1/collectors/gw-0/capture-points'] = () => json(points);
    routes['POST /api/v1/captures'] = () => json(session());
    routes['GET /api/v1/captures/cap-0412'] = () => json(session());
    await render('/captures/new?group=g1');
    await settle(() => !!document.querySelector('input[aria-label="logs output logs"]'));
    await click(document.querySelector('input[aria-label="logs output logs"]')!);
    expect(button('Start capture')!.disabled).toBe(false);
    await click(button('Start capture'));
    await settle(() => calls.some((c) => c.method === 'POST' && c.url === '/api/v1/captures'));
    const body = calls.find((c) => c.method === 'POST' && c.url === '/api/v1/captures')!.body as { group_id?: string; collector_ids?: string[] };
    expect(body.group_id).toBe('g1');
    expect(body.collector_ids).toBeUndefined();
  });
});
