// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { CaptureDataPane } from './CaptureDataPane';
import type { CaptureSession, CaptureFixture } from '../api/types';
import { getWorkspace, updateWorkspace } from '../workspace/store';

vi.mock('@tanstack/react-router', () => ({ Link: ({ to, children }: { to: string; children: React.ReactNode }) => createElement('a', { href: to }, children) }));
let host: HTMLDivElement, root: Root, client: QueryClient;
let requests: { path: string; body: any; signal?: AbortSignal | null }[];
const original = getWorkspace();
const payload = { resourceLogs: [{ scopeLogs: [{ logRecords: [{ body: { stringValue: 'raw-sensitive-value' } }] }] }] };
const stream = { collector_id: 'collector-1', pipeline: 'logs', point: 'egress', signal: 'logs' };
const metricStream = { ...stream, pipeline: 'metrics', signal: 'metrics' };
const metricPayload = { resourceMetrics: [{ scopeMetrics: [{ metrics: [{ name: 'example', gauge: { dataPoints: [{ asDouble: 1 }] } }] }] }] };
const graphFor = (entries: [string, string[]][]) => ({ valid: true, topology: {
  components: [...new Set(entries.flatMap(([, ids]) => ids))].map(id => ({ ref: `receiver/${id}`, id, kind: 'receiver' })),
  pipelines: entries.map(([signal, ids]) => ({ id: signal, signal, receivers: ids.map(id => `receiver/${id}`) })),
} });
let destinationGraph: ReturnType<typeof graphFor> & { diagnostics?: {severity: string; message: string; hint?: string}[] };
let items: {sequence: number; stream: typeof stream; payload: unknown}[];
const session: CaptureSession = { id: 'capture-1', project_id: 'default', caller: { principal: 'local-owner', client: 'http' }, created_at: '2026-10-14T12:00:00Z', expires_at: '2026-10-14T12:01:00Z', state: 'completed', terminal: true, requested_limits: { bytes_per_second: 1, records_per_second: 1, total_bytes: 1, total_records: 1, record_bytes: 1 }, selection: { filter: {}, limits: { bytes_per_second: 1, records_per_second: 1, total_bytes: 1, total_records: 1, record_bytes: 1 } }, data: { enabled: true, contains_raw: true, memory_only: true, state: 'frozen', received_records: 2, received_bytes: 200, rejected_records: 0, byte_limit: 8388608, record_limit: 10000 } };
function fixtureFor(selection: any): CaptureFixture {
  const data = selection.stream.signal === 'metrics' ? metricPayload : payload;
  return { fixture_json: JSON.stringify(data), setup_yaml: '', payload: data, receiver: selection.receiver, signal: selection.stream.signal, derivation: { session_id: 'capture-1', selection, source_digest: 'a'.repeat(64), digest: 'b'.repeat(64), destination_config_digest: 'c'.repeat(64), records: selection.sequences.length }, limitations: [] };
}
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  requests = []; items = [{ sequence: 1, stream, payload }]; destinationGraph = graphFor([['logs', ['otlp']]]);
  localStorage.clear(); updateWorkspace({ ...original, sessionOnly: false, configYAML: 'receivers: {}', scenarioYAML: '# keep my edits\nversion: 1\ninputs: []\nmocks:\n  files: []\nassertions: []\n' });
  vi.stubGlobal('fetch', vi.fn(async (path: string, init: RequestInit = {}) => {
    const body = init.body ? JSON.parse(String(init.body)) : undefined; requests.push({ path, body, signal: init.signal });
    const result = path.endsWith('/config/graph') ? destinationGraph : path.includes('/records') ? { session_id: session.id, watermark: items.length, items: items.map(item => ({ ...item, received_at: session.created_at, sensitivity: 'raw' })) } : path.endsWith('/fixtures') ? fixtureFor(body.selection) : { deleted: true };
    return new Response(JSON.stringify(result), { status: 200 });
  }));
  host = document.createElement('div'); document.body.append(host); root = createRoot(host); client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
});
afterEach(() => { act(() => root.unmount()); client.clear(); host.remove(); updateWorkspace({ ...original, sessionOnly: false }); localStorage.clear(); vi.unstubAllGlobals(); });
async function settle(until: () => boolean) { for (let i = 0; i < 160 && !until(); i++) await act(() => new Promise(r => setTimeout(r, 5))); expect(until()).toBe(true); }
async function render(onInputsAdded?: (count: number) => void) { await act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(CaptureDataPane, { session, onInputsAdded })))); }
function click(text: string) { const button = [...host.querySelectorAll('button')].find(b => b.textContent === text); if (!button) throw new Error(`Missing ${text}`); act(() => button.click()); }
async function selectAll() { await settle(() => !!host.querySelector('[aria-label="Select record 1"]')); act(() => host.querySelectorAll<HTMLInputElement>('input[aria-label^="Select record "]').forEach(input => input.click())); await settle(() => !!host.querySelector('select[aria-label^="Receiver for"]')); }
const addButton = () => [...host.querySelectorAll('button')].find(b => b.textContent === 'Add selected to scenario')!;

it('appends raw input without persisting browser drafts and keeps mocks/assertions', async () => {
  await render(); await selectAll(); click("Add selected to scenario"); await settle(() => host.textContent!.includes("Added 1 input"));
  expect(getWorkspace().scenarioYAML).toContain('# keep my edits'); expect(getWorkspace().scenarioYAML).toContain('mocks:'); expect(getWorkspace().scenarioYAML).toContain('raw-sensitive-value'); expect(getWorkspace().sessionOnly).toBe(true);
  expect(localStorage.getItem('linea.workspace.v1')).not.toContain('raw-sensitive-value');
  act(() => updateWorkspace({ configYAML: 'later edit' })); expect(localStorage.getItem('linea.workspace.v1')).not.toContain('raw-sensitive-value');
});

it('appends several streams in one draft update without replacing existing scenario content', async () => {
  items.push({ sequence: 2, stream: metricStream, payload: metricPayload }); destinationGraph = graphFor([['logs', ['otlp/logs']], ['metrics', ['otlp/metrics']]]);
  act(() => updateWorkspace({ scenarioYAML: '# original scenario\nversion: 1\ninputs:\n  - receiver: otlp\n    data: existing\nassertions: []\nenvironment: {KEEP: yes}\n' }));
  const added = vi.fn(); await render(added); await selectAll(); click('Add selected to scenario'); await settle(() => added.mock.calls.length === 1);
  expect(added).toHaveBeenCalledWith(2);
  const writes = requests.filter(r => r.path.endsWith('/fixtures'));
  expect(writes.map(r => r.body.selection.receiver)).toEqual(['otlp/logs', 'otlp/metrics']);
  expect(getWorkspace().scenarioYAML).toContain('# original scenario'); expect(getWorkspace().scenarioYAML).toContain('data: existing'); expect(getWorkspace().scenarioYAML).toContain('KEEP: yes'); expect(getWorkspace().scenarioYAML).toContain('assertions: []');
  expect(requests.some(r => r.path.endsWith('/scenario'))).toBe(false);
});

it('leaves the whole draft unchanged when materializing a later stream fails', async () => {
  items.push({ sequence: 2, stream: metricStream, payload: metricPayload }); destinationGraph = graphFor([['logs', ['otlp']], ['metrics', ['otlp']]]);
  const fetch = globalThis.fetch;
  vi.stubGlobal('fetch', vi.fn(async (path: string, init: RequestInit = {}) => path.endsWith('/fixtures') && JSON.parse(String(init.body)).selection.stream.signal === 'metrics' ? new Response(JSON.stringify({ error: 'Capture records expired. Select another capture.' }), { status: 404 }) : fetch(path, init)));
  const before = getWorkspace().scenarioYAML; const added = vi.fn(); await render(added); await selectAll(); click('Add selected to scenario');
  await settle(() => !!host.querySelector('.capture-use-selection [role="alert"]'));
  expect(host.querySelector('.capture-use-selection [role="alert"]')!.textContent).toContain('Your scenario has not been changed');
  expect(host.textContent).toContain('Capture records expired'); expect(getWorkspace().scenarioYAML).toBe(before); expect(added).not.toHaveBeenCalled();
});

it('aborts pending reads when the picker closes and never appends after cancellation', async () => {
  const fetch = globalThis.fetch; let started = false; let pendingSignal: AbortSignal | null | undefined;
  vi.stubGlobal('fetch', vi.fn(async (path: string, init: RequestInit = {}) => {
    if (!path.endsWith('/fixtures')) return fetch(path, init);
    started = true; pendingSignal = init.signal;
    return new Promise<Response>((_, reject) => init.signal?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError'))));
  }));
  const before = getWorkspace().scenarioYAML; await render(); await selectAll(); click('Add selected to scenario'); await settle(() => started);
  await act(async () => root.render(null));
  expect(pendingSignal?.aborted).toBe(true); expect(getWorkspace().scenarioYAML).toBe(before);
});

it('blocks incompatible signals and offers direct deselection', async () => {
  destinationGraph = graphFor([['traces', ['otlp']]]); await render(); await selectAll();
  await settle(() => host.textContent!.includes('No receiver accepts logs')); expect(addButton().disabled).toBe(true);
  expect(host.textContent).toContain('Available signals: traces'); click('Deselect logs records');
  expect(addButton()).toBeUndefined(); expect(requests.some(r => r.path.endsWith('/fixtures'))).toBe(false);
});

it('shows actual configuration diagnostics with a repair action', async () => {
  destinationGraph = { ...graphFor([]), valid: false, diagnostics: [{ severity: 'error', message: 'Unknown receiver otlp/missing in logs pipeline', hint: 'Declare the receiver or correct its name.' }] };
  await render(); await settle(() => !!host.querySelector('[aria-label="Select record 1"]')); act(() => host.querySelector<HTMLInputElement>('[aria-label="Select record 1"]')!.click());
  await settle(() => host.textContent!.includes('Unknown receiver otlp/missing')); expect(host.textContent).toContain('Declare the receiver'); expect(host.textContent).toContain('Fix the configuration in the debugger'); expect(addButton().disabled).toBe(true);
});

it('explains an absent configuration and keeps scenario editors off collection', async () => {
  act(() => updateWorkspace({ configYAML: '' })); await render(); await settle(() => !!host.querySelector('[aria-label="Select record 1"]')); act(() => host.querySelector<HTMLInputElement>('[aria-label="Select record 1"]')!.click());
  expect(host.textContent).toContain('No debugger configuration is loaded in this browser'); expect(addButton().disabled).toBe(true);
  expect(host.querySelector('textarea')).toBeNull(); expect(host.textContent).not.toContain('Mocks & assertions'); expect(host.textContent).not.toContain('Create scenario from selection');
});

it('uses the debugger mocks for both receiver checks and materialization', async () => {
  act(() => updateWorkspace({ scenarioYAML: 'version: 1\nenvironment:\n  INPUT_RECEIVER: otlp\ninputs: []\n' })); await render(); await selectAll(); click('Add selected to scenario'); await settle(() => requests.some(r => r.path.endsWith('/fixtures')));
  const check = requests.find(r => r.path.endsWith('/config/graph'))!.body;
  expect(check.setup_yaml).toContain('INPUT_RECEIVER: otlp'); expect(requests.find(r => r.path.endsWith('/fixtures'))!.body.setup_yaml).toBe(check.setup_yaml);
});

it('requires a deliberate retained-copy decision before saving a fixture', async () => {
  await render(); await selectAll(); act(() => host.querySelector<HTMLDetailsElement>('.capture-selection-options')!.open = true); click('Save selected fixture…');
  expect(requests.some(r => r.path.endsWith('/fixtures'))).toBe(false); click('Retain selected copy'); await settle(() => host.textContent!.includes('Fixture saved'));
  expect(requests.find(r => r.path.endsWith('/fixtures'))!.body.save).toBe(true);
});

it('blocks cached receiver choices after a failed recheck', async () => {
  await render(); await selectAll(); const fetch = globalThis.fetch;
  vi.stubGlobal('fetch', vi.fn(async (path: string, init: RequestInit = {}) => path.endsWith('/config/graph') ? new Response(JSON.stringify({ error: 'Configuration check unavailable' }), { status: 503 }) : fetch(path, init)));
  await act(async () => { await client.invalidateQueries({ queryKey: ['capture-destinations'] }); });
  await settle(() => host.textContent!.includes('Could not check destination receivers')); expect(addButton().disabled).toBe(true); expect(host.textContent).toContain('Retry receiver check');
});

it('does not add to malformed scenario YAML and links to its repair', async () => {
  act(() => updateWorkspace({ scenarioYAML: 'inputs: [broken' })); await render(); await settle(() => !!host.querySelector('[aria-label="Select record 1"]')); act(() => host.querySelector<HTMLInputElement>('[aria-label="Select record 1"]')!.click()); expect(addButton().disabled).toBe(true); expect(host.textContent).toContain('Fix the scenario YAML');
});

it('preserves edits made to inputs while capture materialization is pending', async () => {
  const fetch = globalThis.fetch; let finish: (() => void) | undefined;
  vi.stubGlobal('fetch', vi.fn(async (path: string, init: RequestInit = {}) => {
    if (!path.endsWith('/fixtures')) return fetch(path, init);
    return new Promise<Response>(resolve => { finish = () => resolve(new Response(JSON.stringify(fixtureFor(JSON.parse(String(init.body)).selection)), { status: 200 })); });
  }));
  await render(); await selectAll(); click('Add selected to scenario'); await settle(() => !!finish);
  act(() => updateWorkspace({ scenarioYAML: getWorkspace().scenarioYAML.replace('inputs: []', 'inputs:\n  - receiver: old\n    data: kept') }));
  await act(async () => finish!()); await settle(() => host.textContent!.includes('Added 1 input'));
  expect(getWorkspace().scenarioYAML).toContain('# keep my edits'); expect(getWorkspace().scenarioYAML).toContain('data: kept'); expect(getWorkspace().scenarioYAML).toContain('raw-sensitive-value');
});

it('refuses to commit against a configuration changed during preparation', async () => {
  const fetch = globalThis.fetch; let finish: (() => void) | undefined;
  vi.stubGlobal('fetch', vi.fn(async (path: string, init: RequestInit = {}) => {
    if (!path.endsWith('/fixtures')) return fetch(path, init);
    return new Promise<Response>(resolve => { finish = () => resolve(new Response(JSON.stringify(fixtureFor(JSON.parse(String(init.body)).selection)), { status: 200 })); });
  }));
  const before = getWorkspace().scenarioYAML; await render(); await selectAll(); click('Add selected to scenario'); await settle(() => !!finish);
  act(() => updateWorkspace({ configYAML: 'different destination' })); await act(async () => finish!());
  await settle(() => host.textContent!.includes('configuration or mocks changed while preparing'));
  expect(getWorkspace().scenarioYAML).toBe(before);
});
