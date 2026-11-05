// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { CollectorsPage } from './CollectorsPage';
vi.mock('@tanstack/react-router', () => ({ useNavigate: () => vi.fn(), Link: ({ to, children, ...props }: { to: string; children: React.ReactNode }) => createElement('a', { href: to, ...props }, children) }));
let host: HTMLDivElement, root: Root, client: QueryClient;
let handler: (url: string) => [number, unknown];
let captureHandler: ((url: string, init?: RequestInit) => [number, unknown]) | undefined;
const collector = { id: 'c1', generation: 1, agent_id: 'agent-1', credential_id: 'cred-1', pod: 'gateway-0', collector_group: 'gateways', cluster: 'cluster', namespace: 'telemetry', point_count: 2, raw_point_count: 1, collector_version: '0.162.0', distribution_version: 'dev', enabled: true };
const points = [{ pipeline: 'logs', point: 'ingress', signal: 'logs', sensitivity: 'redacted' }, { pipeline: 'logs', point: 'egress', signal: 'logs', sensitivity: 'raw' }];
beforeEach(() => {
 (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
 captureHandler = undefined;
 handler = url => url.includes('/capture-points') ? [200, { collector_id: 'c1', items: points }] : url.startsWith('/api/v1/collectors/c1') ? [200, collector] : [200, { enabled: true, items: [collector] }];
 vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => { const [status, body] = url.startsWith('/api/v1/captures') ? captureHandler ? captureHandler(url, init) : [200, { items: [] }] : handler(url); return new Response(JSON.stringify(body), { status }); }));
 host = document.createElement('div'); document.body.append(host); root = createRoot(host); client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
});
afterEach(() => { act(() => root.unmount()); client.clear(); host.remove(); vi.unstubAllGlobals(); });
async function settle(until: () => boolean) { for (let i = 0; i < 100 && !until(); i++) await act(() => new Promise(r => setTimeout(r, 5))); expect(until()).toBe(true); }
async function render() { await act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(CollectorsPage)))); }
function click(name: string) { const button = [...host.querySelectorAll('button')].find(b => b.getAttribute('aria-label') === name || b.textContent === name); if (!button) throw new Error(`Missing ${name}`); act(() => button.click()); }
it('starts with the workflow and keeps history out of the main view', async () => {
 await render(); await settle(() => !!host.querySelector('[aria-label="Select collector gateway-0"]'));
 expect(host.textContent).toContain('Choose a connected collector');
 expect(host.querySelector('[aria-label="Select collector gateway-0"]')!.textContent).toContain('2 points');
 expect(host.querySelector('[aria-label="Capture history"]')?.hasAttribute('hidden')).toBe(true);
 expect(host.querySelector('textarea')).toBeNull();
 expect(host.textContent).toContain('Connected to fleet');
 expect([...host.querySelectorAll('button')].some(b => b.textContent === 'Connection settings' || b.textContent === 'Groups')).toBe(false);
});
it('builds a capture from checkboxes, offers raw points for fleet to authorize and keeps JSON collapsed', async () => {
 await render(); await settle(() => !!host.querySelector('[aria-label="Select collector gateway-0"]')); click('Select collector gateway-0');
 await settle(() => !!host.querySelector('[aria-label="logs logs ingress"]'));
 const start = [...host.querySelectorAll('button')].find(b => b.textContent === 'Start capture')!;
 expect(start.disabled).toBe(true);
 expect(host.querySelector<HTMLInputElement>('[aria-label="logs logs egress"]')!.disabled).toBe(false);
 expect(host.querySelector('[aria-label="logs logs egress"]')!.closest('label')!.textContent).toContain('raw · needs fleet permission');
 const advanced = [...host.querySelectorAll('details')].find(d => d.querySelector('summary')?.textContent?.includes('Advanced request'))!;
 expect(advanced.open).toBe(false);
 act(() => host.querySelector<HTMLInputElement>('[aria-label="logs logs ingress"]')!.click());
 const request = JSON.parse(host.querySelector<HTMLTextAreaElement>('#capture-selection')!.value);
 expect(request.collector_ids).toEqual(['c1']); expect(request.capture_points).toEqual([{ pipeline: 'logs', point: 'ingress' }]); expect(request.signals).toEqual(['logs']); expect(start.disabled).toBe(false);
 expect(host.querySelector<HTMLInputElement>('[aria-label="Duration in seconds"]')!.value).toBe('60');
});
it('shows a disabled capture as an explanation rather than a dead start action', async () => {
 handler = url => url.includes('/capture-points') ? [200, { collector_id: 'c1', items: points }] : url.startsWith('/api/v1/collectors/c1') ? [200, { ...collector, enabled: false }] : [200, { enabled: true, items: [{ ...collector, enabled: false }] }];
 await render(); await settle(() => !!host.querySelector('[aria-label="Select collector gateway-0"]')); click('Select collector gateway-0'); await settle(() => host.textContent!.includes('capture disabled in its livecapture extension'));
 expect(host.querySelector<HTMLInputElement>('[aria-label="logs logs ingress"]')!.disabled).toBe(true);
});
it('handles a missing fleet connection and bounded inventory paging', async () => {
 handler = () => [200, { enabled: false, items: [] }]; await render(); await settle(() => host.textContent!.includes('Live capture is not set up'));
 expect(host.textContent).toContain('--fleet-token-file');
 handler = url => url.includes('cursor=next') ? [409, { error: 'Inventory changed; restart paging' }] : [200, { enabled: true, items: [collector], next_cursor: 'next' }];
 await act(async () => { await client.invalidateQueries({ queryKey: ['collectors'] }); });
 await settle(() => host.textContent!.includes('Next Collectors')); expect(host.textContent).toContain('Grouping and search apply to the current page.'); click('Next Collectors'); await settle(() => host.textContent!.includes('Restart Collector list')); click('Restart Collector list'); await settle(() => !host.textContent!.includes('Inventory changed; restart paging'));
});

it('groups metadata with counts, preserves capture drafts and reveals searched groups', async () => {
 const inventory = [collector, { ...collector, id: 'c2', pod: 'gateway-1', namespace: 'payments' }, { ...collector, id: 'c3', pod: 'agent-0', cluster: 'other', collector_group: 'agents', enabled: false }, { ...collector, id: 'c4', pod: 'local-0', cluster: '', namespace: '', collector_group: '' }];
 handler = url => url.includes('/capture-points') ? [200, { collector_id: 'c1', items: points }] : url.startsWith('/api/v1/collectors/c1') ? [200, collector] : [200, { enabled: true, items: inventory }];
 await render(); await settle(() => !!host.querySelector('[aria-label="Cluster: cluster"]'));
 const cluster = host.querySelector<HTMLElement>('[aria-label="Cluster: cluster"]')!;
 expect(cluster.querySelector('[aria-label="2 collectors"]')).not.toBeNull();
 expect(host.querySelector('[aria-label="Cluster: Not reported"]')).not.toBeNull();
 click('Select collector gateway-0'); await settle(() => !!host.querySelector('[aria-label="logs logs ingress"]'));
 act(() => host.querySelector<HTMLInputElement>('[aria-label="logs logs ingress"]')!.click());
 const request = host.querySelector<HTMLTextAreaElement>('#capture-selection')!.value;
 act(() => cluster.querySelector<HTMLButtonElement>('.capture-group-heading')!.click());
 expect(cluster.querySelector('button')!.getAttribute('aria-expanded')).toBe('false');
 expect(cluster.querySelector('div')!.hidden).toBe(true); expect(cluster.textContent).toContain('Selected');
 const search = host.querySelector<HTMLInputElement>('input[type="search"]')!;
 act(() => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(search, 'gateway-0'); search.dispatchEvent(new Event('input', { bubbles: true })); });
 expect(cluster.querySelector('button')!.getAttribute('aria-expanded')).toBe('true');
 expect(host.querySelector('[aria-label="Cluster: other"]')).toBeNull();
 expect(cluster.querySelector('[aria-label="1 collector"]')).not.toBeNull();
 act(() => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(search, ''); search.dispatchEvent(new Event('input', { bubbles: true })); });
 const select = host.querySelector<HTMLSelectElement>('.capture-grouping select')!;
 function groupBy(value: string) { act(() => { select.value = value; select.dispatchEvent(new Event('change', { bubbles: true })); }); }
 groupBy('namespace'); expect(host.querySelector('[aria-label="Namespace: payments"]')).not.toBeNull();
 groupBy('collector_group'); expect(host.querySelector('[aria-label="Collector group: agents"]')).not.toBeNull();
 groupBy('none'); expect(host.querySelector('.capture-group-heading')).toBeNull();
 expect(host.querySelector<HTMLTextAreaElement>('#capture-selection')!.value).toBe(request);
 expect(host.querySelector('[aria-label="Select collector gateway-0"]')!.getAttribute('aria-pressed')).toBe('true');
});

it('captures the whole metadata group across pages despite search, with shared points and fixed targets', async () => {
 const second = { ...collector, id: 'c2', pod: 'gateway-1' };
 let inventory = [collector]; let session: Record<string, unknown> | undefined;
 handler = url => url.includes('/capture-points') ? [200, { items: url.includes('/c1/') ? [...points.map(p => ({ ...p, sensitivity: 'redacted' })), { pipeline: 'traces', point: 'ingress', signal: 'traces', sensitivity: 'redacted' }] : points }] : [200, { enabled: true, items: url.includes('cursor=next') ? [second] : inventory, next_cursor: url.includes('cursor=next') ? '' : 'next' }];
 captureHandler = (url, init) => {
   if (url === '/api/v1/captures' && init?.method === 'POST') {
     const selection = JSON.parse(String(init.body));
     session = { id: 'group-session', selection, created_at: '2026-10-15T10:00:00Z', expires_at: '2026-10-15T10:01:00Z', state: 'starting', terminal: false, data: { enabled: false }, targets: [collector, second].map(c => ({ collector_id: c.id, generation: 1, source: c, state: 'pending', terminal: false })) };
     return [200, session];
   }
   return url.startsWith('/api/v1/captures/group-session') ? [200, session] : [200, { items: [] }];
 };
 await render(); await settle(() => !!host.querySelector('[aria-label="Capture group Cluster: cluster"]'));
 const search = host.querySelector<HTMLInputElement>('input[type="search"]')!;
 act(() => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(search, 'gateway-0'); search.dispatchEvent(new Event('input', { bubbles: true })); });
 click('Capture group Cluster: cluster'); await settle(() => host.textContent!.includes('Review 2 collectors'));
 expect(host.querySelector('[aria-label="traces traces ingress"]')).toBeNull();
 expect(host.querySelector('[aria-label="logs logs egress"]')!.closest('label')!.textContent).toContain('raw');
 expect(host.querySelector('[aria-label="Capture targets"]')!.textContent).toContain('gateway-1');
 act(() => host.querySelector<HTMLInputElement>('[aria-label="logs logs ingress"]')!.click());
 inventory = [collector, { ...collector, id: 'c3', pod: 'new-gateway' }];
 await act(async () => { await client.invalidateQueries({ queryKey: ['collectors'] }); });
 click('Start capture'); await settle(() => !!session);
 const selection = session!.selection as { collector_ids: string[]; capture_points: unknown[] };
 expect(selection.collector_ids).toEqual(['c1', 'c2']); expect(selection.capture_points).toEqual([{ pipeline: 'logs', point: 'ingress' }]);
 await settle(() => !!host.querySelector('[aria-label="Collector capture status"]'));
 expect(host.querySelector('[aria-label="Collector capture status"]')!.textContent).toContain('gateway-1');
 expect([...host.querySelectorAll('button')].filter(b => b.textContent === 'Stop capture')).toHaveLength(1);
});

it('keeps a reviewed group and edited points when individual target replacement is cancelled', async () => {
 handler = url => url.includes('/capture-points') ? [200, { items: points }] : url.startsWith('/api/v1/collectors/c1') ? [200, collector] : [200, { enabled: true, items: [collector, { ...collector, id: 'c2', pod: 'gateway-1' }] }];
 await render(); await settle(() => !!host.querySelector('[aria-label="Capture group Cluster: cluster"]'));
 click('Capture group Cluster: cluster'); await settle(() => host.textContent!.includes('Review 2 collectors'));
 act(() => host.querySelector<HTMLInputElement>('[aria-label="logs logs ingress"]')!.click());
 click('Select collector gateway-0'); await settle(() => host.textContent!.includes('Replace capture settings?')); click('Keep capture selection');
 const request = JSON.parse(host.querySelector<HTMLTextAreaElement>('#capture-selection')!.value);
 expect(request.collector_ids).toEqual(['c1', 'c2']); expect(request.capture_points).toHaveLength(1);
 expect(host.textContent).toContain('Review 2 collectors');
 expect(host.querySelector('[aria-label="Select collector gateway-1"]')!.getAttribute('aria-pressed')).toBe('true');
});

it('explains unavailable group members and never starts a partial group silently', async () => {
 handler = url => url.includes('/capture-points') ? [200, { items: points }] : [200, { enabled: true, items: [collector, { ...collector, id: 'c2', pod: 'gateway-1', enabled: false }] }];
 await render(); await settle(() => !!host.querySelector('[aria-label="Capture group Cluster: cluster"]'));
 click('Capture group Cluster: cluster'); await settle(() => host.textContent!.includes('Every group member must have capture enabled'));
 expect(host.querySelector('[aria-label="Capture targets"]')!.textContent).toContain('capture disabled');
 expect([...host.querySelectorAll('button')].find(b => b.textContent === 'Start capture')!.disabled).toBe(true);
});

it('explains groups with no common capture point and keeps Start disabled', async () => {
 handler = url => url.includes('/capture-points') ? [200, { items: url.includes('/c1/') ? points : [{ pipeline: 'traces', point: 'ingress', signal: 'traces', sensitivity: 'redacted' }] }] : [200, { enabled: true, items: [collector, { ...collector, id: 'c2', pod: 'gateway-1' }] }];
 await render(); await settle(() => !!host.querySelector('[aria-label="Capture group Cluster: cluster"]'));
 click('Capture group Cluster: cluster'); await settle(() => host.textContent!.includes('No capture points are shared'));
 expect([...host.querySelectorAll('button')].find(b => b.textContent === 'Start capture')!.disabled).toBe(true);
});
