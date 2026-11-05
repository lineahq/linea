// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { updateWorkspace, useWorkspace, type WorkspaceState } from '../workspace/store';
import { RunControls } from './RunControls';
import type { RunView } from './useRun';

const blank: WorkspaceState = {
  configYAML: '', configPath: '', scenarioYAML: '', scenarioPath: '', compare: false,
  baselines: undefined, saved: undefined, runtime: undefined,
};
const configYAML = `receivers:
  otlp:
    protocols:
      http: {}
exporters:
  debug: {}
service:
  pipelines:
    logs:
      receivers: [otlp]
      exporters: [debug]
`;
const inputJSON = JSON.stringify({
  resourceLogs: [{ scopeLogs: [{ logRecords: [{ body: { stringValue: 'quick input' } }] }] }],
});
const quickYAML = `version: 1
environment:
  REGION: eu
inputs:
  - receiver: otlp
    client_address: 10.0.0.1
    data: '${inputJSON}'
`;
const scenarioYAML = `version: 1
name: scenario draft
inputs:
  - receiver: otlp
    signal: logs
    data: '{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"scenario input"}}]}]}]}'
assertions:
  - count: {exporter: debug, equals: 1}
`;
const view: RunView = { events: [], live: false };

interface FetchCall { method: string; url: string; body: unknown }
let calls: FetchCall[];
let onRun = vi.fn<(id: string | undefined) => void>();
let host: HTMLDivElement, root: Root, client: QueryClient;

beforeEach(() => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
  updateWorkspace(blank);
  calls = [];
  onRun = vi.fn<(id: string | undefined) => void>();
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    calls.push({ method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    if (method === 'GET' && url === '/api/v1/runs') {
      return new Response(JSON.stringify({ runs: [] }), { status: 200 });
    }
    if (method === 'POST' && url === '/api/v1/runs') {
      return new Response(JSON.stringify({ id: 'r1', state: 'queued', created: '2026-10-13T12:00:00Z' }), { status: 200 });
    }
    if (method === 'DELETE' && url === '/api/v1/runs/r1') return new Response(null, { status: 204 });
    if (method === 'DELETE' && url === '/api/v1/runs/m1') {
      return new Response(JSON.stringify({ code: 'conflict', error: 'run belongs to suite suite-1; delete the suite to delete its runs' }), { status: 409 });
    }
    throw new Error(`Unexpected request: ${method} ${url}`);
  }));
  client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
});
afterEach(() => {
  act(() => root.unmount());
  client.clear();
  host.remove();
  updateWorkspace(blank);
  vi.unstubAllGlobals();
});

function Controls() {
  return createElement(RunControls, { ws: useWorkspace(), configValid: true, view, runId: undefined, onRun });
}
async function render() {
  await act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(Controls))));
}
async function run(label: string) {
  const buttons = [...host.querySelectorAll('button')];
  expect(buttons).toHaveLength(1);
  expect(buttons[0].textContent).toBe(label);
  expect(buttons[0].disabled).toBe(false);
  await act(async () => buttons[0].click());
  for (let i = 0; i < 100 && !onRun.mock.calls.length; i++) await act(() => new Promise((r) => setTimeout(r, 5)));
  expect(onRun).toHaveBeenCalledExactlyOnceWith('r1');
}

describe('RunControls submissions', () => {
  it('runs pasted config and one inline payload in one POST without saving artifacts, candidates or scenarios', async () => {
    updateWorkspace({ configYAML, scenarioYAML: `version: 1
inputs:
  - data: '${inputJSON}'
` });
    await render();
    const compare = host.querySelector<HTMLInputElement>('input[type="checkbox"]')!;
    expect(compare.checked).toBe(false);
    await act(async () => compare.click());
    expect(compare.checked).toBe(true);
    await run('▶ Run input');
    expect(calls.filter((c) => c.method === 'POST')).toEqual([
      { method: 'POST', url: '/api/v1/runs', body: { config_yaml: configYAML, fixture_json: inputJSON, compare: true } },
    ]);
  });

  for (const mode of ['quick', 'scenario'] as const) {
    it(`runs the document as a ${mode} run and labels the Run action`, async () => {
      updateWorkspace({ configYAML, scenarioYAML: mode === 'quick' ? quickYAML : scenarioYAML });
      await render();
      await run(mode === 'quick' ? '▶ Run input' : '▶ Run scenario');
      expect(calls.filter((c) => c.method === 'POST')).toEqual([
        {
          method: 'POST', url: '/api/v1/runs', body: {
            config_yaml: configYAML, compare: false,
            ...(mode === 'quick'
              ? { fixture_json: inputJSON, setup_yaml: `version: 1
environment:
  REGION: eu
`, receiver: 'otlp', client_address: '10.0.0.1' }
              : { scenario_yaml: scenarioYAML }),
          },
        },
      ]);
    });

    it(`sends the runtime chosen beside Run with a ${mode} run`, async () => {
      updateWorkspace({ configYAML, scenarioYAML: mode === 'quick' ? quickYAML : scenarioYAML, runtime: 'v0163' });
      await render();
      await run(mode === 'quick' ? '▶ Run input' : '▶ Run scenario');
      const posts = calls.filter((c) => c.method === 'POST');
      expect(posts).toHaveLength(1);
      expect((posts[0].body as { runtime?: string }).runtime).toBe('v0163');
    });
  }
});

describe('RunControls deletion', () => {
  const done: RunView = { events: [], live: false, run: { id: 'r1', state: 'done', created: '2026-10-13T12:00:00Z' } as RunView['run'] };
  async function show(v: RunView, runId: string) {
    await act(async () => root.render(createElement(QueryClientProvider, { client },
      createElement(() => createElement(RunControls, { ws: useWorkspace(), configValid: true, view: v, runId, onRun })))));
  }
  const button = (text: string) => [...host.querySelectorAll('button')].find((b) => b.textContent === text);
  const settle = async (until: () => boolean) => {
    for (let i = 0; i < 100 && !until(); i++) await act(() => new Promise((r) => setTimeout(r, 5)));
  };

  it('deletes a finished run only after the inline confirmation', async () => {
    await show(done, 'r1');
    await act(async () => button('Delete')!.click());
    expect(host.querySelector('[role="alertdialog"]')?.textContent).toContain('removed for good');
    expect(calls.some((c) => c.method === 'DELETE')).toBe(false);
    await act(async () => button('Keep')!.click());
    expect(host.querySelector('[role="alertdialog"]')).toBeNull();
    await act(async () => button('Delete')!.click());
    await act(async () => button('Delete run')!.click());
    await settle(() => onRun.mock.calls.length > 0);
    expect(calls.filter((c) => c.method === 'DELETE').map((c) => c.url)).toEqual(['/api/v1/runs/r1']);
    expect(onRun).toHaveBeenCalledExactlyOnceWith(undefined);
  });

  it('offers no deletion for a queued or running run', async () => {
    await show({ events: [], live: true, run: { ...done.run!, state: 'running' } }, 'r1');
    expect(button('Delete')).toBeUndefined();
    expect(button('Cancel')).toBeDefined();
  });

  it('shows why a suite member was not deleted and keeps it selected', async () => {
    await show({ ...done, run: { ...done.run!, id: 'm1' } }, 'm1');
    await act(async () => button('Delete')!.click());
    await act(async () => button('Delete run')!.click());
    await settle(() => !!host.querySelector('.error'));
    expect(host.querySelector('.error')?.textContent).toBe('run belongs to suite suite-1; delete the suite to delete its runs');
    expect(onRun).not.toHaveBeenCalled();
  });
});
