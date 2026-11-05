// @vitest-environment jsdom
import { act, createElement, type ReactNode } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Run, RunComparison } from '../api/types';
import { different } from '../runs/compareFixture';
import { ComparisonView } from './ComparisonView';
import { RunListPage } from './RunListPage';
import { RunsDrawer } from './RunsDrawer';

const done = (id: string, created: string, extra: Record<string, unknown> = {}): Run => ({
  id, created, state: 'done', bundle: { mode: 'scenario' },
  ...extra,
}) as unknown as Run;
const withReport = (r: Run, status: string, assertions?: unknown): Run => ({
  ...r, report: { report_schema: '1', scenario: 'debug-error-logs', runtime: { kind: 'built_in', collector_version: '0.162.0', verified: true }, result: { status, mode: 'scenario', inputs: [{ injected: true }] }, assertions },
}) as unknown as Run;

const builtIn = { kind: 'built_in', name: 'built-in', collector_version: '0.162.0', verified: true };
// Newest first, each finished run with the platform's summary of its report, as the platform lists them.
const listed: Run[] = [
  { id: 'run-cccccccccccccccc', created: '2026-10-23T14:20:00Z', state: 'running' } as Run,
  done('run-bbbbbbbbbbbbbbbb', '2026-10-23T14:09:00Z', { summary: { scenario: 'debug-error-logs', runtime: builtIn, status: 'completed', inputs: 1, injected: 1, assertions: { outcome: 'pass', pass: 2, fail: 0, inconclusive: 0, error: 0 } } }),
  done('run-aaaaaaaaaaaaaaaa', '2026-10-23T14:02:00Z', { summary: { scenario: 'debug-error-logs', runtime: builtIn, status: 'partial', inputs: 1, injected: 1 } }),
];
/** resultOf is the platform's result filter: the execution status of a run with a report, else its state. */
const resultOf = (r: Run) => r.summary?.status ?? r.state;
const reports: Record<string, Run> = {
  'run-bbbbbbbbbbbbbbbb': withReport(listed[1], 'completed', { outcome: 'pass', results: [{ outcome: 'pass' }, { outcome: 'pass' }] }),
  'run-aaaaaaaaaaaaaaaa': withReport(listed[2], 'partial'),
};

let comparison: RunComparison;
/** pageSize is how many runs the mocked list returns per page; the next cursor is the offset of the following page. */
let pageSize: number;
let configs: Record<string, string>;
let calls: { method: string; url: string }[];
let host: HTMLDivElement, root: Root;

beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  comparison = { ...different, before: { ...different.before, id: 'run-aaaaaaaaaaaaaaaa' }, after: { ...different.after, id: 'run-bbbbbbbbbbbbbbbb' } };
  calls = [];
  configs = {};
  pageSize = 50;
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    calls.push({ method, url });
    if (url === '/api/v1/runs' || url.startsWith('/api/v1/runs?')) {
      const query = new URLSearchParams(url.split('?')[1] ?? '');
      const result = query.get('result');
      const rows = listed.filter((r) => !result || resultOf(r) === result);
      const start = Number(query.get('cursor') ?? 0);
      const next = start + pageSize;
      return new Response(JSON.stringify({ runs: rows.slice(start, next), next_cursor: next < rows.length ? String(next) : undefined }), { status: 200 });
    }
    const one = /^\/api\/v1\/runs\/([^/]+)$/.exec(url);
    if (one && method === 'GET') return new Response(JSON.stringify(reports[one[1]] ?? listed.find((r) => r.id === one[1])), { status: 200 });
    if (one && method === 'DELETE') return new Response(null, { status: 204 });
    const inputs = /^\/api\/v1\/runs\/([^/]+)\/inputs$/.exec(url);
    if (inputs) {
      const text = configs[inputs[1]] ?? 'receivers: {}';
      return new Response(JSON.stringify({ run_id: inputs[1], mode: 'scenario', config: { text, sha256: `sha-${text}` } }), { status: 200 });
    }
    if (url.includes('/compare/')) return new Response(JSON.stringify(comparison), { status: 200 });
    return new Response(JSON.stringify({ error: `unexpected ${url}` }), { status: 404 });
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

async function render(node: ReactNode, path = '/') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const r = createRouter({ routeTree: createRootRoute({ component: () => node }), history: createMemoryHistory({ initialEntries: [path] }) });
  await act(async () => {
    root.render(createElement(QueryClientProvider, { client }, createElement(RouterProvider, { router: r as never })));
  });
  return r;
}
const text = () => host.textContent ?? '';
const click = (el: Element | null | undefined) => {
  if (!el) throw new Error(`nothing to click in ${host.innerHTML}`);
  act(() => (el as HTMLElement).click());
};
const button = (label: string) => [...host.querySelectorAll('button')].find((b) => b.textContent?.includes(label));

describe('the runs drawer', () => {
  it('lists runs newest first with execution, evidence and assertions as separate chips', async () => {
    await render(createElement(RunsDrawer, { open: true, onClose: vi.fn(), currentRunId: 'run-aaaaaaaaaaaaaaaa' }));
    await settle(() => text().includes('2 of 2 passed'));
    const rows = [...host.querySelectorAll('a')].filter((a) => a.textContent?.includes('#'));
    expect(rows.map((a) => a.querySelector('.t-code-strong')?.textContent)).toEqual(['#cccccccc', '#bbbbbbbb', '#aaaaaaaa']);
    const [, passed, partial] = rows;
    expect([...passed.querySelectorAll('.od-chip')].map((c) => c.textContent)).toEqual(['Completed', 'Complete', '2 of 2 passed']);
    // The viewing run ended early: three facts, none folded into another.
    expect([...partial.querySelectorAll('.od-chip')].map((c) => c.textContent)).toEqual(['Viewing', 'Ended early', 'Partial 1 of 1', 'Not evaluated']);
    expect(text()).toContain('built-in 0.162.0');
    expect(text()).toContain('3 runs');
    // The list's summaries are enough: no row reads its run.
    expect(calls.filter((c) => c.url.startsWith('/api/v1/runs/'))).toEqual([]);
  });

  it('opens a run in the debugger and offers every run', async () => {
    const onClose = vi.fn();
    await render(createElement(RunsDrawer, { open: true, onClose }));
    await settle(() => text().includes('#bbbbbbbb'));
    const link = [...host.querySelectorAll('a')].find((a) => a.textContent?.includes('#bbbbbbbb'))!;
    expect(link.getAttribute('href')).toBe('/?run=run-bbbbbbbbbbbbbbbb');
    expect([...host.querySelectorAll('a')].find((a) => a.textContent === 'All runs')?.getAttribute('href')).toBe('/runs');
  });

  it('closes on Escape and on a click outside, but not inside', async () => {
    const onClose = vi.fn();
    await render(createElement('div', null, createElement('button', { id: 'outside' }, 'outside'), createElement(RunsDrawer, { open: true, onClose })));
    await settle(() => text().includes('#bbbbbbbb'));
    act(() => { host.querySelector('aside')!.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true })); });
    expect(onClose).not.toHaveBeenCalled();
    act(() => { document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true })); });
    expect(onClose).toHaveBeenCalledTimes(1);
    act(() => { host.querySelector('#outside')!.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true })); });
    expect(onClose).toHaveBeenCalledTimes(2);
  });

  it('moves focus into the drawer and picks a run to compare with', async () => {
    await render(createElement(RunsDrawer, { open: true, onClose: vi.fn(), currentRunId: 'run-aaaaaaaaaaaaaaaa', picking: true }));
    await settle(() => text().includes('#bbbbbbbb'));
    expect(document.activeElement).toBe(host.querySelector('aside'));
    expect(text()).toContain('Pick a run to compare with #aaaaaaaa');
    const hrefs = [...host.querySelectorAll('a')].filter((a) => a.querySelector('.t-code-strong')).map((a) => a.getAttribute('href'));
    // The running run and the current run cannot be picked.
    expect(hrefs).toEqual(['/?run=run-aaaaaaaaaaaaaaaa&compare=run-bbbbbbbbbbbbbbbb']);
  });

  it('renders nothing while closed', async () => {
    await render(createElement(RunsDrawer, { open: false, onClose: vi.fn() }));
    expect(host.querySelector('aside')).toBeNull();
  });
});

describe('the full run list', () => {
  const box = (label: string) => host.querySelector<HTMLInputElement>(`input[aria-label="${label}"]`)!;

  it('lists the runs with their summaries and lets the server filter by result', async () => {
    await render(createElement(RunListPage));
    await settle(() => text().includes('2 of 2 passed'));
    expect(text()).toContain('All runs');
    expect(host.querySelectorAll('tbody tr')).toHaveLength(3);
    expect(calls.filter((c) => c.url.startsWith('/api/v1/runs/'))).toEqual([]);
    const result = [...host.querySelectorAll('label')].find((l) => l.textContent?.startsWith('Result'))!.querySelector('select')!;
    act(() => { result.value = 'partial'; result.dispatchEvent(new Event('change', { bubbles: true })); });
    await settle(() => host.querySelectorAll('tbody tr').length === 1);
    expect(calls.some((c) => c.url === '/api/v1/runs?result=partial')).toBe(true);
    expect(host.querySelector('tbody tr')?.textContent).toContain('#aaaaaaaa');
    expect(text()).toContain('1 run matches');
  });

  it('keeps the filters in the URL and reads them back on load', async () => {
    const router = await render(createElement(RunListPage));
    await settle(() => text().includes('2 of 2 passed'));
    const result = [...host.querySelectorAll('label')].find((l) => l.textContent?.startsWith('Result'))!.querySelector('select')!;
    act(() => { result.value = 'partial'; result.dispatchEvent(new Event('change', { bubbles: true })); });
    await settle(() => host.querySelectorAll('tbody tr').length === 1);
    expect(router.state.location.search).toEqual({ result: 'partial' });

    // A link with a filter opens the filtered list.
    act(() => root.unmount());
    root = createRoot(host);
    calls = [];
    await render(createElement(RunListPage), '/?result=partial');
    await settle(() => host.querySelectorAll('tbody tr').length === 1);
    expect(host.querySelector('tbody tr')?.textContent).toContain('#aaaaaaaa');
    expect(calls.filter((c) => c.url.startsWith('/api/v1/runs?')).map((c) => c.url)).toEqual(['/api/v1/runs?result=partial']);
    expect(([...host.querySelectorAll('label')].find((l) => l.textContent?.startsWith('Result'))!.querySelector('select') as HTMLSelectElement).value).toBe('partial');
  });

  it('loads the next page with Load more, passing the cursor of the same filters', async () => {
    pageSize = 2;
    await render(createElement(RunListPage));
    await settle(() => host.querySelectorAll('tbody tr').length === 2);
    expect(text()).toContain('2+ runs');
    expect(text()).toContain('older runs follow');
    click(button('Load more'));
    await settle(() => host.querySelectorAll('tbody tr').length === 3);
    expect(calls.filter((c) => c.url.startsWith('/api/v1/runs?cursor=')).map((c) => c.url)).toEqual(['/api/v1/runs?cursor=2']);
    expect([...host.querySelectorAll('tbody tr')].map((r) => /#[a-f]{8}/.exec(r.textContent ?? '')?.[0])).toEqual(['#cccccccc', '#bbbbbbbb', '#aaaaaaaa']);
    expect(text()).toContain('no older runs');
    expect(text()).toContain('3 runs');
    expect(button('Load more')).toBeUndefined();
  });

  it('loads the next page when the end of the list scrolls into view', async () => {
    pageSize = 2;
    let seen: ((entries: { isIntersecting: boolean }[]) => void) | undefined;
    vi.stubGlobal('IntersectionObserver', class {
      constructor(cb: typeof seen) { seen = cb; }
      observe() {}
      disconnect() {}
    });
    await render(createElement(RunListPage));
    await settle(() => host.querySelectorAll('tbody tr').length === 2 && seen !== undefined);
    expect(calls.some((c) => c.url.includes('cursor='))).toBe(false);
    act(() => seen!([{ isIntersecting: false }]));
    expect(calls.some((c) => c.url.includes('cursor='))).toBe(false);
    act(() => seen!([{ isIntersecting: true }]));
    await settle(() => host.querySelectorAll('tbody tr').length === 3);
    expect(calls.filter((c) => c.url.includes('cursor=')).length).toBe(1);
  });

  it('starts again from the first page when a filter changes', async () => {
    pageSize = 1;
    const router = await render(createElement(RunListPage));
    await settle(() => host.querySelectorAll('tbody tr').length === 1);
    click(button('Load more'));
    await settle(() => host.querySelectorAll('tbody tr').length === 2);
    calls = [];
    act(() => { void router.navigate({ to: '/', search: { result: 'completed' } as never }); });
    await settle(() => calls.some((c) => c.url === '/api/v1/runs?result=completed'));
    await settle(() => host.querySelectorAll('tbody tr').length === 1);
    expect(host.querySelector('tbody tr')?.textContent).toContain('#bbbbbbbb');
    expect(calls.some((c) => c.url.includes('cursor='))).toBe(false);
  });

  it('compares two selected runs with the earlier admitted one as baseline', async () => {
    await render(createElement(RunListPage));
    await settle(() => text().includes('2 of 2 passed'));
    click(box('Select run #aaaaaaaa'));
    expect(text()).toContain('Select one more run to compare');
    expect(button('Compare 1 run')?.disabled).toBe(true);
    click(box('Select run #bbbbbbbb'));
    expect(text()).toContain('#aaaaaaaa is the baseline (older), #bbbbbbbb the candidate');
    expect(button('Compare 2 runs')?.disabled).toBe(false);
  });

  it('deletes only after an inline confirmation', async () => {
    await render(createElement(RunListPage));
    await settle(() => text().includes('2 of 2 passed'));
    click(box('Select run #bbbbbbbb'));
    click(button('Delete'));
    expect(host.querySelector('[role="alertdialog"]')?.textContent).toContain('Delete this run? Its report');
    expect(calls.some((c) => c.method === 'DELETE')).toBe(false);
    click(button('Delete run'));
    await settle(() => calls.some((c) => c.method === 'DELETE'));
    expect(calls.filter((c) => c.method === 'DELETE').map((c) => c.url)).toEqual(['/api/v1/runs/run-bbbbbbbbbbbbbbbb']);
  });

  it('cannot delete a run that has not finished', async () => {
    await render(createElement(RunListPage));
    await settle(() => text().includes('2 of 2 passed'));
    click(box('Select run #cccccccc'));
    expect(button('Delete')?.disabled).toBe(true);
  });
});

describe('the comparison page', () => {
  const view = (props: Partial<Parameters<typeof ComparisonView>[0]> = {}) =>
    createElement(ComparisonView, { runId: 'run-bbbbbbbbbbbbbbbb', compareId: 'run-aaaaaaaaaaaaaaaa', onChangeRuns: vi.fn(), ...props });

  it('puts the earlier admitted run first as the baseline whichever is selected', async () => {
    await render(view());
    await settle(() => text().includes('Outputs'));
    expect(host.querySelector('.od-crumbs')?.textContent).toContain('Compare runs #aaaaaaaa and #bbbbbbbb');
    expect(calls.some((c) => c.url === '/api/v1/runs/run-aaaaaaaaaaaaaaaa/compare/run-bbbbbbbbbbbbbbbb')).toBe(true);
    const sides = [...host.querySelectorAll('.od-cmp__side')];
    expect(sides.map((s) => s.querySelector('.od-cmp__role')?.textContent)).toEqual(['Baseline · earlier run', 'Candidate · later run']);
    expect(sides[0].textContent).toContain('Run #aaaaaaaa');
    expect(text()).not.toContain('Swap');
  });

  it('opens with eligibility, the exporters, and one row per changed record', async () => {
    await render(view());
    await settle(() => text().includes('Outputs'));
    expect(host.querySelector('.od-cmp-verdict')?.textContent).toContain('Comparable');
    expect([...host.querySelectorAll('section[aria-label="Outputs"] tbody tr')].map((r) => r.querySelector('td')?.textContent)).toEqual(['otlp/backend', 'debug']);
    expect([...host.querySelectorAll('.rv-row')].map((r) => r.querySelector('.rv-why')?.textContent)).toEqual(['Changed at 1 exporter', 'Ends differently']);
    const open = [...host.querySelectorAll('a')].filter((a) => a.textContent?.startsWith('Open in'));
    expect(open.slice(0, 2).map((a) => a.getAttribute('href'))).toEqual(['/?run=run-aaaaaaaaaaaaaaaa', '/?run=run-bbbbbbbbbbbbbbbb']);
  });

  it('diffs the configurations only when the recorded inputs differ', async () => {
    await render(view());
    await settle(() => text().includes('Outputs') && calls.filter((c) => c.url.endsWith('/inputs')).length === 2);
    expect(host.querySelector('section[aria-label="Input differences"]')).toBeNull();

    act(() => root.unmount());
    root = createRoot(host);
    configs = { 'run-aaaaaaaaaaaaaaaa': 'condition: a', 'run-bbbbbbbbbbbbbbbb': 'condition: b' };
    await render(view());
    await settle(() => !!host.querySelector('section[aria-label="Input differences"]'));
    expect(text()).toContain('The runs differ in their configuration.');
    expect(host.querySelector('[aria-label="Configuration diff"]')).toBeNull();
    click(button('Configuration diff'));
    const diff = host.querySelector('[aria-label="Configuration diff"]');
    expect([...diff!.querySelectorAll('.u-del, .u-add')].map((l) => l.textContent?.slice(-12))).toEqual(['condition: a', 'condition: b']);
  });

  it('states every inconclusive reason in the open and shows differences only on request', async () => {
    comparison = { ...comparison, outcome: 'inconclusive', inputs_identical: false, records: undefined,
      reasons: [{ code: 'after_truncated', message: 'the after run did not keep all of its evidence (budget)' }, { code: 'inputs_differ', message: 'the runs’ captured inputs are not proven identical' }] };
    await render(view());
    await settle(() => text().includes('Not comparable'));
    const items = [...host.querySelectorAll('.od-cmp-verdict li')].map((li) => li.textContent);
    expect(items).toEqual(['The after run did not keep all of its evidence (budget).', 'The runs’ captured inputs are not proven identical.']);
    // The exporter outputs stay in the open, marked not conclusive; the rest waits.
    expect(host.querySelectorAll('section[aria-label="Outputs"] tbody tr')).toHaveLength(2);
    expect(host.querySelector('section[aria-label="Outputs"]')?.textContent).toContain('Not conclusive');
    expect(host.querySelector('section[aria-label="Errors"]')).toBeNull();
    click(button('Show differences anyway'));
    expect(host.querySelector('section[aria-label="Errors"]')).not.toBeNull();
  });

  it('reports a run that is gone or unfinished, and changes the pair through the picker', async () => {
    const onChange = vi.fn();
    await render(view({ compareId: 'run-gone', onChangeRuns: onChange }));
    await settle(() => text().includes('not in the run list'));
    expect(text()).toContain('#gone is not in the run list');
    click(button('Change runs'));
    const picker = host.querySelector('section[aria-label="Change runs"]')!;
    const selects = picker.querySelectorAll('select');
    act(() => { selects[1].value = 'run-aaaaaaaaaaaaaaaa'; selects[1].dispatchEvent(new Event('change', { bubbles: true })); });
    click(button('Compare these'));
    expect(onChange).toHaveBeenCalledWith('run-bbbbbbbbbbbbbbbb', 'run-aaaaaaaaaaaaaaaa');
  });

  it('waits for a run that is still running', async () => {
    await render(view({ compareId: 'run-cccccccccccccccc' }));
    await settle(() => text().includes('has not finished'));
    expect(calls.some((c) => c.url.includes('/compare/'))).toBe(false);
  });
});
