// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { CaptureControls } from './CaptureControls';
import { selectionForPoint } from './selection';
import type { CaptureSelection, CaptureSession } from '../api/types';

let host: HTMLDivElement, root: Root, client: QueryClient;
let sessions: CaptureSession[];
let requests: { path: string; body: unknown }[];
let waitResponse: Promise<CaptureSession> | undefined;
const draft = selectionForPoint('collector-1', { pipeline: 'logs', point: 'egress', signal: 'logs', sensitivity: 'redacted' });
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  sessions = []; requests = []; waitResponse = undefined;
  vi.stubGlobal('fetch', vi.fn(async (path: string, init: RequestInit) => {
    const body = init.body ? JSON.parse(String(init.body)) : undefined;
    requests.push({ path, body });
    let result: unknown;
    if (path === '/api/v1/captures/validate') result = { selection: body, collectors: [{}], filter_validation: 'bounded_preflight_only', limits_scope: 'per_collector' };
    else if (path === '/api/v1/captures' && init.method === 'POST') {
      const session: CaptureSession = { id: 'capture-1', project_id: 'default', data: { enabled: false, contains_raw: false, state: 'unavailable', memory_only: true, received_records: 0, received_bytes: 0, rejected_records: 0, byte_limit: 0, record_limit: 0 }, requested_limits: body.limits, caller: { principal: 'local-owner', client: 'http' }, created_at: '2026-10-14T12:00:00Z', expires_at: '2026-10-14T12:01:00Z', selection: body, state: 'starting', terminal: false, targets: [{ collector_id: 'collector-1', source: { pod: 'pod-1', collector_group: 'lab', collector_version: '0.162.0', authentication: 'none', transport_security: 'plaintext' }, generation: 1, state: 'pending', terminal: false, stats: { selected: 0, enqueued_records: 0, enqueued_bytes: 0, filter_errors: 0, dropped: {} } }] };
      sessions = [session]; result = session;
    } else if (path.endsWith('/stop')) { sessions[0] = { ...sessions[0], state: 'stopping' }; result = sessions[0]; }
    else if (path.startsWith('/api/v1/captures/capture-1')) result = path.endsWith('?wait=true') && waitResponse ? await waitResponse : sessions[0];
    else if (path.startsWith('/api/v1/captures?')) result = { items: sessions.map(s => ({ id: s.id, state: s.state, terminal: s.terminal, created_at: s.created_at, expires_at: s.expires_at, target_count: 1, active_count: 0, uncertain_count: 0, failed_count: 0 })) };
    else result = {};
    return new Response(JSON.stringify(result), { status: 200 });
  }));
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
});
afterEach(() => { act(() => root.unmount()); client.clear(); host.remove(); vi.unstubAllGlobals(); });
async function settle(until: () => boolean) { for (let i = 0; i < 100 && !until(); i++) await act(() => new Promise(r => setTimeout(r, 5))); expect(until()).toBe(true); }
async function render(selection: CaptureSelection = draft) { await act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(CaptureControls, { draft: selection })))); }
function click(text: string) { const button = [...host.querySelectorAll('button')].find(b => b.textContent === text); if (!button) throw new Error(`Missing ${text}`); act(() => button.click()); }


it('checks and starts a capture, then moves to results with one stop action', async () => {
 await render(); await settle(() => !!host.querySelector('form'));
 const advanced = [...host.querySelectorAll('details')].find(d => d.querySelector('summary')?.textContent?.includes('Advanced request'))!;
 expect(advanced.open).toBe(false); expect(JSON.parse(host.querySelector('#capture-selection')!.textContent || (host.querySelector('#capture-selection') as HTMLTextAreaElement).value)).toEqual(draft);
 await settle(() => ![...host.querySelectorAll('button')].find(b => b.textContent === 'Check selection')!.disabled); click('Check selection'); await settle(() => host.textContent!.includes('Preflight accepted'));
 expect(requests.find(r => r.path.endsWith('/validate'))?.body).toEqual(draft);
 act(() => host.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })));
 await settle(() => [...host.querySelectorAll('button')].some(b => b.textContent === 'Stop capture'));
 expect(host.querySelector('[aria-label="Configure capture"]')?.hasAttribute('hidden')).toBe(true);
 expect(host.querySelector('[aria-label="Capture history"]')?.hasAttribute('hidden')).toBe(false);
 expect(host.textContent).toContain('authentication none');
 expect([...host.querySelectorAll('button')].filter(b => b.textContent === 'Stop capture')).toHaveLength(1);
 expect(host.textContent).not.toContain('Wait for capture capture-1');
 click('Stop capture'); await settle(() => host.textContent!.includes('stopping'));
 expect(requests.some(r => r.path.endsWith('/stop'))).toBe(true);
});
it('maps duration and sampling controls into the shared request', async () => {
 await render(); await settle(() => !!host.querySelector('form'));
 const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
 for (const [label, value] of [['Duration in seconds', '30'], ['Sampling percentage', '25']]) {
  const input = host.querySelector<HTMLInputElement>(`[aria-label="${label}"]`)!;
  act(() => { set.call(input, value); input.dispatchEvent(new Event('input', { bubbles: true })); });
 }
 click('Check selection'); await settle(() => requests.some(r => r.path.endsWith('/validate')));
 const body = requests.find(r => r.path.endsWith('/validate'))!.body as CaptureSelection;
 expect(body.duration_seconds).toBe(30); expect(body.sampling?.rate).toBe(.25);
});
it('retains edited requests until a deliberate replacement and handles incomplete JSON', async () => {
 await render(); await settle(() => !!host.querySelector('#capture-selection'));
 const area = host.querySelector<HTMLTextAreaElement>('#capture-selection')!;
 const set = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
 act(() => { set.call(area, 'unfinished'); area.dispatchEvent(new Event('input', { bubbles: true })); });
 await render(selectionForPoint('collector-2', { pipeline: 'traces', point: 'ingress', signal: 'traces', sensitivity: 'raw' }));
 expect(area.value).toBe('unfinished'); click('Keep capture selection');
 expect([...host.querySelectorAll('button')].find(b => b.textContent === 'Start capture')!.disabled).toBe(true);
 await render(selectionForPoint('collector-2', { pipeline: 'traces', point: 'ingress', signal: 'traces', sensitivity: 'raw' }));
 click('Replace capture selection'); expect(JSON.parse(area.value).collector_ids).toEqual(['collector-2']);
 act(() => { set.call(area, '{}'); area.dispatchEvent(new Event('input', { bubbles: true })); });
 expect([...host.querySelectorAll('button')].find(b => b.textContent === 'Start capture')!.disabled).toBe(true);
});
