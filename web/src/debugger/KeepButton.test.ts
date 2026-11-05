// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Run } from '../api/types';
import { KeepButton } from './KeepButton';

const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status });
const run = (patch: Partial<Run> = {}) => ({ id: 'run-1', state: 'done', created: '2026-10-30T00:00:00Z', project_id: 'default', ...patch }) as Run;

interface FetchCall { method: string; url: string; contentType?: string; body?: string }
let calls: FetchCall[];
let respond: (call: FetchCall) => Response;
let host: HTMLDivElement, root: Root, client: QueryClient;

beforeEach(() => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
  calls = [];
  respond = (c) => json(run(c.method === 'PUT' ? { kept: true } : {}));
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    const headers = (init?.headers ?? {}) as Record<string, string>;
    const call: FetchCall = { method: init?.method ?? 'GET', url, contentType: headers['Content-Type'], body: init?.body as string | undefined };
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

const mount = (r: Run) => act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(KeepButton, { run: r }))));
const button = () => host.querySelector('button')!;
async function settle(until: () => boolean) {
  for (let i = 0; i < 100 && !until(); i++) await act(() => new Promise((r) => setTimeout(r, 5)));
}

describe('Keep toggle', () => {
  it('keeps a run with a JSON PUT, which the server requires even without a body', async () => {
    const cached = run({ report: { report_schema: '1' } as unknown as Run['report'] });
    client.setQueryData(['run', 'run-1'], cached);
    await mount(cached);
    expect(button().textContent).toBe('Keep');
    expect(button().getAttribute('aria-pressed')).toBe('false');
    expect(button().title).toContain('Kept runs are never removed by retention or prune.');
    await act(async () => button().click());
    await settle(() => calls.length > 0);
    expect(calls[0]).toEqual({ method: 'PUT', url: '/api/v1/runs/run-1/keep', contentType: 'application/json', body: '{}' });
    await settle(() => (client.getQueryData<Run>(['run', 'run-1']) as Run).kept === true);
    // The cached run keeps its report; only the mark changed.
    expect(client.getQueryData<Run>(['run', 'run-1'])!.report).toBe(cached.report);
  });

  it('shows a kept run as Kept and lets retention have it again with a DELETE', async () => {
    client.setQueryData(['run', 'run-1'], run({ kept: true }));
    await mount(run({ kept: true }));
    expect(button().textContent).toBe('Kept');
    expect(button().getAttribute('aria-pressed')).toBe('true');
    await act(async () => button().click());
    await settle(() => calls.length > 0);
    expect(calls[0].method).toBe('DELETE');
    expect(calls[0].url).toBe('/api/v1/runs/run-1/keep');
    await settle(() => !('kept' in client.getQueryData<Run>(['run', 'run-1'])!));
  });

  it('invalidates the run lists so a poll shows the mark', async () => {
    client.setQueryData(['runs'], { runs: [] });
    await mount(run());
    await act(async () => button().click());
    await settle(() => client.getQueryState(['runs'])!.isInvalidated);
    expect(client.getQueryState(['runs'])!.isInvalidated).toBe(true);
  });

  it('shows a refusal inline', async () => {
    respond = () => json({ code: 'forbidden', error: 'forbidden: only an admin may change a run another principal submitted' }, 403);
    await mount(run());
    await act(async () => button().click());
    await settle(() => !!host.querySelector('[role=alert]'));
    expect(host.querySelector('[role=alert]')!.textContent).toBe("Only the run's owner or an admin may keep or unkeep it.");
    expect(button().textContent).toBe('Keep');
  });

  it('is disabled for a role without run.delete and names the permission', async () => {
    client.setQueryData(['session'], { principal: { id: 'user:vic', kind: 'user' }, permissions: ['run.read'] });
    await mount(run());
    expect(button().disabled).toBe(true);
    expect(button().title).toBe('Needs the run.delete permission, which your role does not have');
  });
});
