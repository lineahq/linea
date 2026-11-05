// @vitest-environment jsdom
import { parse } from 'yaml';
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { beforeEach, afterEach, expect, it, vi } from 'vitest';
import { InputsPanel } from '../input/InputsPanel';
import { getWorkspace, updateWorkspace, useWorkspace } from '../workspace/store';
import type { Graph, CaptureSession, CaptureFixture } from '../api/types';

vi.mock('@tanstack/react-router', () => ({ Link: ({to, children}: {to: string; children: React.ReactNode}) => createElement('a', {href: to}, children), useNavigate: () => vi.fn() }));
vi.mock('../editor/YamlEditor', () => ({ YamlEditor: () => createElement('div', null, 'Payload editor') }));
vi.mock('../monaco', () => ({ JSON_LANGUAGE: 'json' }));
const original = getWorkspace();
const graph = { components: [{ref: 'receiver/otlp', id: 'otlp', kind: 'receiver', type: 'otlp', mode: 'simulated'}], pipelines: [{id: 'logs', signal: 'logs', receivers: ['receiver/otlp']}] } as Graph;
const payload = {resourceLogs: [{scopeLogs: [{logRecords: [{body: {stringValue: 'captured test record'}}]}]}]};
const collector = {id: 'collector-1', pod: 'mock-collector', cluster: 'lab', namespace: 'test', collector_group: 'demo', collector_version: '0.162.0', generation: 1, credential_id: 'cred', enabled: true, point_count: 1, raw_point_count: 0};
const stream = {collector_id: collector.id, pipeline: 'logs', point: 'ingress', signal: 'logs'};
const baseSession: CaptureSession = {id: 'capture-existing', project_id: 'default', metadata: {revision: 1, name: 'Existing sample', description: '', updated_at: '2026-10-15T12:00:00Z'}, caller: {principal: 'local-owner', client: 'http'}, created_at: '2026-10-15T12:00:00Z', expires_at: '2026-10-15T12:01:00Z', state: 'completed', terminal: true, selection: {filter: {}, limits: {bytes_per_second: 1, records_per_second: 1, total_bytes: 1, total_records: 1, record_bytes: 1}}, requested_limits: {bytes_per_second: 1, records_per_second: 1, total_bytes: 1, total_records: 1, record_bytes: 1}, data: {enabled: true, memory_only: false, contains_raw: false, state: 'frozen', received_records: 1, received_bytes: 100, rejected_records: 0, byte_limit: 8388608, record_limit: 10000}};
let root: Root, host: HTMLDivElement, client: QueryClient;
let sessions: CaptureSession[], requests: {path: string; method?: string; body: any}[];
function Harness() { return createElement(InputsPanel, {ws: useWorkspace(), topology: graph, diagnostics: [], runInput: vi.fn(), blocker: () => undefined, running: false, onMocks: vi.fn()}); }
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  sessions = [baseSession]; requests = []; localStorage.clear(); updateWorkspace({...original, sessionOnly: false, configYAML: 'current configuration', scenarioYAML: '# building scenario\nversion: 1\ninputs: []\nmocks: {files: []}\nassertions: []\n'});
  vi.stubGlobal('fetch', vi.fn(async (path: string, init: RequestInit = {}) => {
    const body = init.body ? JSON.parse(String(init.body)) : undefined; requests.push({path, method: init.method, body}); let result: unknown;
    if (path.endsWith('/config/graph')) result = {valid: true, topology: graph};
    else if (path.includes('/capture-points')) result = {items: [{pipeline: 'logs', point: 'ingress', signal: 'logs', sensitivity: 'redacted'}]};
    else if (path.startsWith('/api/v1/collectors?')) result = {enabled: true, items: [collector]};
    else if (path.startsWith('/api/v1/collectors/')) result = collector;
    else if (path === '/api/v1/captures' && init.method === 'POST') { const created = {...baseSession, id: 'capture-new', selection: body}; sessions.unshift(created); result = created; }
    else if (path.includes('/records')) result = {session_id: sessions[0].id, watermark: 1, items: [{sequence: 1, stream, sensitivity: 'redacted', received_at: baseSession.created_at, payload}]};
    else if (path.endsWith('/fixtures')) result = {fixture_json: JSON.stringify(payload), payload, receiver: body.selection.receiver, signal: 'logs', setup_yaml: '', derivation: {session_id: sessions[0].id, selection: body.selection, source_digest: 'a'.repeat(64), digest: 'b'.repeat(64), destination_config_digest: 'c'.repeat(64), records: 1}, limitations: []} satisfies CaptureFixture;
    else if (path.startsWith('/api/v1/captures?')) result = {items: sessions.map(item => ({...item, target_count: 1}))};
    else if (path.startsWith('/api/v1/captures/')) result = sessions.find(item => path.includes(item.id));
    else result = {items: []};
    return new Response(JSON.stringify(result), {status: 200});
  }));
  host = document.createElement('div'); document.body.append(host); root = createRoot(host); client = new QueryClient({defaultOptions: {queries: {retry: false}}});
});
afterEach(() => {act(() => root.unmount()); client.clear(); host.remove(); updateWorkspace({...original, sessionOnly: false}); localStorage.clear(); vi.unstubAllGlobals();});
async function settle(until: () => boolean) {for (let i = 0; i < 180 && !until(); i++) await act(() => new Promise(r => setTimeout(r, 5))); expect(until()).toBe(true);}
async function render() {await act(async () => root.render(createElement(QueryClientProvider, {client}, createElement(Harness))));}
function click(text: string) {const button = [...host.querySelectorAll('button')].find(item => item.textContent?.startsWith(text)); if (!button) throw new Error(`Missing button ${text}`); act(() => button.click());}
async function openPicker() {click('From a capture…'); await settle(() => !!host.querySelector('dialog[aria-label="Add input from capture"]'));}
async function addRecord() {await settle(() => !!host.querySelector('[aria-label="Select record 1"]')); act(() => host.querySelector<HTMLInputElement>('[aria-label="Select record 1"]')!.click()); await settle(() => ![...host.querySelectorAll('button')].find(button => button.textContent === 'Add selected to scenario')?.disabled); click('Add selected to scenario'); await settle(() => !host.querySelector('dialog'));}

it('starts from the empty debugger inputs and adds an existing capture without replacing the scenario', async () => {
  await render(); await openPicker(); expect(host.textContent).toContain('Existing captures'); expect(host.textContent).toContain('Start new capture'); expect(host.textContent).not.toContain('Edit details'); expect(host.textContent).not.toContain('Manage retained data');
  await addRecord(); expect(getWorkspace().scenarioYAML).toContain('# building scenario'); expect(JSON.parse(parse(getWorkspace().scenarioYAML).inputs[0].data)).toEqual(payload); expect(getWorkspace().scenarioYAML).toContain('mocks:'); expect(getWorkspace().scenarioYAML).toContain('assertions:');
  expect(host.querySelector('dialog')).toBeNull(); expect(host.textContent).toContain('logs · 1 log record'); expect(requests.some(item => item.path.endsWith('/scenario'))).toBe(false);
});

it('offers capture under Add input and cancelling preserves all existing content', async () => {
  act(() => updateWorkspace({scenarioYAML: `# previous edit\nversion: 1\ninputs: [{receiver: otlp, data: '{"resourceLogs":[]}'}]\nassertions: []\n`}));
  const before = getWorkspace().scenarioYAML; await render(); click('+ Add input'); await openPicker(); click('Close');
  expect(getWorkspace().scenarioYAML).toBe(before); expect(host.querySelector('dialog')).toBeNull();
});

it('starts a new capture inside the debugger then adds its records to that scenario', async () => {
  await render(); await openPicker(); click('Start new capture'); await settle(() => !!host.querySelector('[aria-label="Select collector mock-collector"]'));
  act(() => host.querySelector<HTMLButtonElement>('[aria-label="Select collector mock-collector"]')!.click());
  await settle(() => !!host.querySelector('[aria-label="logs logs ingress"]'));
  act(() => host.querySelector<HTMLInputElement>('[aria-label="logs logs ingress"]')!.click());
  await settle(() => ![...host.querySelectorAll('button')].find(item => item.textContent === 'Start capture')?.disabled); click('Start capture');
  await settle(() => requests.some(item => item.path === '/api/v1/captures' && item.method === 'POST'));
  await addRecord(); expect(JSON.parse(parse(getWorkspace().scenarioYAML).inputs[0].data)).toEqual(payload);
  const request = requests.find(item => item.path === '/api/v1/captures' && item.method === 'POST')!;
  expect(request.body.collector_ids).toEqual(['collector-1']); expect(request.body.signals).toEqual(['logs']);
});
