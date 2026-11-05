// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Run, RunComparison } from '../api/types';
import { ComparePanel } from './ComparePanel';
import { different } from './compareFixture';

const runs: Run[] = [
  { id: 'run-b', project_id: 'default', created: '2026-10-13T11:00:00Z', state: 'done' },
  { id: 'run-a', project_id: 'default', created: '2026-10-13T10:00:00Z', state: 'done' },
  { id: 'run-live', project_id: 'default', created: '2026-10-13T12:00:00Z', state: 'running' },
];

let host: HTMLDivElement, root: Root, comparison: RunComparison | { status: number; error: string }, requested: string[];
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  comparison = different;
  requested = [];
  vi.stubGlobal('fetch', vi.fn(async (url: string) => {
    requested.push(url);
    if (url === '/api/v1/runs') return new Response(JSON.stringify({ runs }), { status: 200 });
    if ('status' in comparison) return new Response(JSON.stringify({ error: comparison.error }), { status: comparison.status });
    return new Response(JSON.stringify(comparison), { status: 200 });
  }));
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
});
afterEach(() => { act(() => root.unmount()); host.remove(); vi.unstubAllGlobals(); });

async function render(runId: string, other?: string, onOther = vi.fn()) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  await act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(ComparePanel, { runId, other, onOther }))));
  for (let i = 0; i < 100 && (host.textContent?.includes('Loading') || host.textContent?.includes('Comparing')); i++) await act(() => new Promise((r) => setTimeout(r, 5)));
  return onOther;
}

describe('comparing two runs', () => {
  it('picks another finished run and compares the older one before', async () => {
    const onOther = await render('run-b');
    const select = host.querySelector('select')!;
    expect([...select.options].map((o) => o.value)).toEqual(['', 'run-a']);
    expect(requested.some((u) => u.includes('/compare/'))).toBe(false);
    act(() => { select.value = 'run-a'; select.dispatchEvent(new Event('change', { bubbles: true })); });
    expect(onOther).toHaveBeenCalledWith('run-a');
    await render('run-b', 'run-a');
    expect(requested).toContain('/api/v1/runs/run-a/compare/run-b');
  });

  it('shows the outcome, every exporter and one row per changed record', async () => {
    await render('run-a', 'run-b');
    expect(host.querySelector('.compare-outcome')?.textContent).toBe('different Different: 1 of 2 exporter outputs differ; 2 injected records changed.');
    expect([...host.querySelectorAll('table.grid tbody tr')].map((r) => r.textContent)).toEqual(['otlp/backendlogs32different records', 'debuglogs11same records']);
    const rows = [...host.querySelectorAll('.rv-row')];
    expect(rows.map((r) => r.querySelector('.rv-why')?.textContent)).toEqual(['Changed at 1 exporter', 'Ends differently']);
    expect(rows[1].textContent).toContain('medium-confidence match');
    // The first record is open: its ends and its changed fields at the exporter.
    const panel = host.querySelector('.rv-panel')!;
    expect(panel.textContent).toContain('delivered to otlp/backend (logs)');
    expect([...panel.querySelectorAll('tr.rv-field')].map((r) => r.querySelector('.rv-path')?.textContent)).toEqual(['resource.attributes["env"]', 'log.severity_text', 'log.attributes["note"]']);
    act(() => (rows[1] as HTMLButtonElement).click());
    expect(host.querySelector('.rv-panel')?.textContent).toContain('dropped at filter/health (logs)');
  });

  it('states every inconclusive reason openly', async () => {
    comparison = { ...different, outcome: 'inconclusive', records: undefined, inputs_identical: false,
      reasons: [{ code: 'after_truncated', message: 'The after run’s evidence was truncated.' }, { code: 'builds_differ', message: 'Different builds admitted the runs.' },
        { code: 'inputs_differ', message: 'The runs’ captured inputs are not proven identical.' }] };
    await render('run-a', 'run-b');
    expect([...host.querySelectorAll('[role="alert"]')].map((p) => p.textContent)).toEqual(['The after run’s evidence was truncated.', 'Different builds admitted the runs.',
      'The runs’ captured inputs are not proven identical.']);
    expect(host.querySelector('details')).toBeNull();
    expect(host.textContent).toContain('not proven identical, so the runs cannot be compared');
    expect(host.querySelector('.rv-row')).toBeNull();
  });

  it('waits for an unfinished run and reports a refused comparison', async () => {
    await render('run-live');
    expect(host.textContent).toContain('has not finished');
    comparison = { status: 409, error: 'run run-a has no report' };
    await render('run-a', 'run-b');
    expect(host.querySelector('[role="alert"]')?.textContent).toBe('Could not compare the runs: run run-a has no report');
  });
});
