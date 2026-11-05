// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { OTTLReport } from '../api/types';
import { OTTLPanel } from './OTTLPanel';
import { getWorkspace, updateWorkspace } from '../workspace/store';

const config = 'processors:\n  transform:\n    log_statements:\n      - set(log.body, "a")\n';
const report = {
  processor: 'transform',
  signal: 'logs',
  verification: 'replay_verified',
  groups: [{ index: 0, context: 'log', error_mode: 'propagate', status: 'evaluated', statements: [{ index: 0, text: 'set(log.body, "a")', matched: 1, not_matched: 0 }] }],
} as unknown as OTTLReport;

let reply: (body: Record<string, unknown>) => [number, unknown];
const sent: Record<string, unknown>[] = [];
let host: HTMLDivElement, root: Root;
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  sent.length = 0;
  updateWorkspace({ configYAML: config });
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    expect(url).toBe('/api/v1/candidates');
    const body = JSON.parse(init!.body as string);
    sent.push(body);
    const [status, data] = reply(body);
    return new Response(JSON.stringify(data), { status });
  }));
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
});
afterEach(() => { act(() => root.unmount()); host.remove(); vi.unstubAllGlobals(); });

async function edit(text: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  await act(async () => root.render(createElement(QueryClientProvider, { client },
    createElement(OTTLPanel, { report, input: {}, observed: {}, onRecord: () => {}, onRerun: () => {} }))));
  click('Edit');
  const area = host.querySelector('textarea')!;
  const set = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
  act(() => { set.call(area, text); area.dispatchEvent(new Event('input', { bubbles: true })); });
  click('Apply to config');
}
function click(text: string) {
  const b = [...host.querySelectorAll('button')].find((e) => e.textContent === text);
  if (!b) throw new Error(`no button ${text} in ${host.innerHTML}`);
  act(() => b.click());
}
async function settle(until: () => boolean) {
  for (let i = 0; i < 100 && !until(); i++) await act(() => new Promise((r) => setTimeout(r, 5)));
}

describe('editing a statement', () => {
  it("sends the edit to the server and puts its proposal in the configuration draft", async () => {
    const proposed = config.replace('"a"', '"b"');
    reply = () => [200, { proposed_yaml: proposed, patch: '', affected_pipelines: ['logs'], valid: true, diagnostics: [] }];
    await edit('set(log.body, "b")');
    await settle(() => getWorkspace().configYAML === proposed);
    expect(sent).toEqual([{ config_yaml: config, edit: { processor: 'transform', signal: 'logs', group: 0, index: 0, op: 'replace', text: 'set(log.body, "b")', expected: 'set(log.body, "a")' } }]);
    expect(getWorkspace().configYAML).toBe(proposed);
  });

  it('leaves the draft alone when the statement changed since the run', async () => {
    reply = () => [409, { code: 'conflict', error: 'stale statement edit', current_statement: 'set(log.body, "z")' }];
    await edit('set(log.body, "b")');
    await settle(() => host.textContent!.includes('rerun first'));
    expect(host.textContent).toContain('it now reads set(log.body, "z")');
    expect(getWorkspace().configYAML).toBe(config);
  });
});
