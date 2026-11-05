// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Event, Inspection, Report, Run } from '../api/types';
import type { LayoutNode } from '../graph/layout';
import type { WorkspaceSearch } from '../router';
import { InvestigatePane } from './InvestigatePane';
import { ComponentPane } from './ComponentPane';

const counts = (items: number) => ({ items, resources: 1, scopes: 1 });
const comp = (kind: string, id: string, mode = 'real', extra = {}) => ({ kind, id, type: id.split('/')[0], ref: `${kind}/${id}`, mode, ...extra });

const report = {
  report_schema: '0.3',
  topology: {
    components: [comp('receiver', 'otlp'), comp('processor', 'filter/pager'), comp('exporter', 'otlp/pager', 'captured'), comp('exporter', 'otlp/archive', 'captured')],
    pipelines: [{ id: 'logs/pager', signal: 'logs', receivers: ['receiver/otlp'], processors: ['processor/filter/pager'], exporters: ['exporter/otlp/pager'] }],
  },
  result: {
    mode: 'scenario', status: 'completed',
    inputs: [{ receiver: 'otlp', signal: 'logs', injected: true, counts: counts(2) }],
    outputs: [{ exporter: 'otlp/archive', signal: 'logs', batches: 1, counts: counts(3) }],
  },
  stages: [{
    component: 'filter/pager', kind: 'processor', pipeline: 'logs/pager', type: 'filter', signal: 'logs', emission: 'x',
    inputs: [{ seq: 2, outcome: 'dropped', diff: { identity: 'x', before: counts(2), after: counts(0), removed: [{ name: 'a' }, { name: 'b' }] } }],
  }],
  lineage: [
    { id: 'l1', name: 'card authorization failed', signal: 'logs', receiver: 'otlp', stops: [{ end: 'dropped', component: 'filter/pager', hop: 0 }], hops: [{ kind: 'stage.input', component: 'filter/pager', pipeline: 'logs/pager', parent: -1, ref: 'r', seq: 2, end: 'dropped' }] },
    { id: 'l2', name: 'checkout completed', signal: 'logs', receiver: 'otlp', deliveries: [{ exporter: 'otlp/archive', hop: 0, changed: false }] },
  ],
  assertions: { outcome: 'fail', run_status: 'completed', results: [{ name: 'pager gets 2', outcome: 'fail', target: 'otlp/pager', checks: [{ check: 'count', expected: '2', observed: '0', outcome: 'fail' }] }] },
} as unknown as Report;

const run: Run = { id: 'run-0123456789abcdef', project_id: 'default', created: '', state: 'done', started: '2026-10-23T14:02:00Z', finished: '2026-10-23T14:02:01Z', report };
const events = [{ seq: 2, kind: 'stage.input', component: 'filter/pager', pipeline: 'logs/pager', source: 'collector', counts: counts(2), id: '2', run_id: 'r', time: '' }] as unknown as Event[];
const view = { run, events, live: false };

let host: HTMLDivElement, root: Root;
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ run_id: run.id, mode: 'scenario', config: { text: 'processors:\n  filter/pager:\n    a: 1\n', sha256: 'aa', label: 'cfg.yaml' } }), { status: 200 })));
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
});
afterEach(() => { act(() => root.unmount()); host.remove(); vi.unstubAllGlobals(); });

const settle = async () => { for (let i = 0; i < 20; i++) await act(() => new Promise((r) => setTimeout(r, 5))); };
async function mount(el: ReturnType<typeof createElement>) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  await act(async () => root.render(createElement(QueryClientProvider, { client }, el)));
  await settle();
}
const text = (s: string) => [...host.querySelectorAll<HTMLElement>('button, [role="option"], [role="tab"]')].find((b) => b.textContent?.includes(s));
const click = (s: string) => { const b = text(s); if (!b) throw new Error(`no ${s} in ${host.innerHTML}`); return act(() => b.click()); };

function investigate(search: WorkspaceSearch, setSearch = vi.fn(), v = view) {
  return mount(createElement(InvestigatePane, { runId: run.id, view: v, search, setSearch }));
}

describe('Investigate left pane', () => {
  it('opens on the overview: three separate facts, findings with provenance, assertions and open sections', async () => {
    await investigate({ run: run.id });
    expect([...host.querySelectorAll('.od-facet__name')].map((n) => n.textContent)).toEqual(['Execution', 'Evidence', 'Assertions']);
    expect(host.textContent).toContain('1 of 1 failed');
    expect(host.textContent).toContain('otlp/pager received 0 records');
    expect(host.textContent).toContain('2 records were dropped at filter/pager.');
    expect(host.textContent).toContain('Observed in run #01234567');
    expect(host.textContent).toContain('count: expected 2, observed 0');
    expect(host.querySelector('section[aria-label="Stand-ins"]')).not.toBeNull();
    expect(host.textContent).toContain('Mocks, stand-ins and missing references');
    expect(host.textContent).toContain("Open the run's configuration and scenario");
    expect(host.textContent).not.toContain('Likely cause');
  });

  it('opens the stage a finding names and follows its dropped record', async () => {
    const setSearch = vi.fn();
    await investigate({ run: run.id }, setSearch);
    await click('Open filter/pager');
    expect(setSearch).toHaveBeenCalledWith(expect.objectContaining({ node: 'logs/pager|processor/filter/pager' }));
    await click('Follow a dropped record');
    expect(setSearch).toHaveBeenLastCalledWith(expect.objectContaining({ trace: 'l1' }));
  });

  it('lists record journeys with their fate and follows one by choosing its row', async () => {
    const setSearch = vi.fn();
    await investigate({ run: run.id, tab: 'journey' }, setSearch);
    expect(host.querySelector('[role="tab"][aria-selected="true"]')!.textContent).toContain('Records');
    expect(host.querySelector('.od-tab__count')!.textContent).toBe('2');
    expect(host.textContent).toContain('dropped at filter/pager');
    expect(host.textContent).toContain('delivered to otlp/archive');
    await click('Dropped 1');
    expect(host.querySelectorAll('[role="option"]')).toHaveLength(1);
    await click('card authorization failed');
    expect(setSearch).toHaveBeenCalledWith(expect.objectContaining({ trace: 'l1', node: 'logs/pager|processor/filter/pager' }));
  });

  it('keeps its own tab while a component tab owns the URL, and writes the URL only when no component is chosen', async () => {
    const setSearch = vi.fn();
    await investigate({ run: run.id, node: 'x', tab: 'changes' }, setSearch);
    await click('Timeline');
    expect(host.querySelector('[role="tab"][aria-selected="true"]')!.textContent).toBe('Timeline');
    expect(setSearch).not.toHaveBeenCalled();
    await click('Overview');
    expect(setSearch).not.toHaveBeenCalled();
  });

  it('writes the tab to the URL when nothing is selected in the graph', async () => {
    const setSearch = vi.fn();
    await investigate({ run: run.id }, setSearch);
    await click('Timeline');
    expect(setSearch).toHaveBeenCalledWith({ tab: 'events', view: undefined });
  });

  it('says an unfinished run has no records yet', async () => {
    await investigate({ run: run.id, tab: 'journey' }, vi.fn(), { run: { ...run, state: 'running', report: undefined }, events: [], live: true });
    expect(host.textContent).toContain('Records are available when the run finishes.');
  });
});

const layoutNode = (c: ReturnType<typeof comp>, pipeline?: string): LayoutNode => ({ key: pipeline ? `${pipeline}|${c.ref}` : c.ref, component: c as never, pipeline, pipelines: pipeline ? [pipeline] : [], layer: 0, x: 0, y: 0 });
const inspection = { valid: true, topology: report.topology, effective: { exporters: { 'otlp/pager': { endpoint: 'pager.example:4317' } } } } as unknown as Inspection;

describe('component pane', () => {
  it('shows the component header and the tabs of its kind, with the mode', async () => {
    await mount(createElement(ComponentPane, { node: layoutNode(comp('processor', 'transform/normalize'), 'logs/ingest'), runId: run.id, view, search: { run: run.id }, setSearch: vi.fn() }));
    expect(host.textContent).toContain('transform/normalize');
    expect(host.textContent).toContain('Real');
    expect([...host.querySelectorAll('[role="tab"]')].map((t) => t.textContent)).toEqual(['Changes', 'Statements', 'Configuration']);
  });

  it('says nothing reached an exporter, names the stage that dropped records and does not call it a drop after an early end', async () => {
    const setSearch = vi.fn();
    const pager = layoutNode(comp('exporter', 'otlp/pager', 'captured'));
    await mount(createElement(ComponentPane, { node: pager, runId: run.id, view, search: { run: run.id }, setSearch }));
    expect(host.textContent).toContain('No records reached otlp/pager');
    expect(host.textContent).toContain('filter/pager recorded dropping 2 of 2 records in logs/pager, so nothing was exported.');
    await click('Open filter/pager');
    expect(setSearch).toHaveBeenCalledWith(expect.objectContaining({ node: 'logs/pager|processor/filter/pager' }));

    const early = { ...view, run: { ...run, report: { ...report, result: { ...report.result!, status: 'partial' } } } };
    await mount(createElement(ComponentPane, { node: pager, runId: run.id, view: early, search: { run: run.id }, setSearch }));
    expect(host.textContent).toContain('Nothing was recorded at otlp/pager');
    expect(host.textContent).toContain('does not show that records were dropped');
  });

  it('lists a followed record’s path above the view and lets the user stop following', async () => {
    const setSearch = vi.fn();
    await mount(createElement(ComponentPane, {
      node: layoutNode(comp('exporter', 'otlp/pager', 'captured')), runId: run.id, view, search: { run: run.id, trace: 'l1' }, setSearch,
    }));
    expect(host.textContent).toContain('Following “card authorization failed”');
    expect(host.textContent).toContain('dropped at filter/pager');
    await click('Stop following');
    expect(setSearch).toHaveBeenCalledWith({ trace: undefined });
  });

  it('before any run explains how the component will run and shows its effective configuration', async () => {
    const node = layoutNode(comp('exporter', 'otlp/pager', 'captured'));
    await mount(createElement(ComponentPane, { node, view: { events: [], live: false }, inspection, search: {}, setSearch: vi.fn() }));
    expect([...host.querySelectorAll('[role="tab"]')].map((t) => t.textContent)).toEqual(['How it will run', 'Effective configuration']);
    expect(host.textContent).toContain('Output captured, not sent');
    expect(host.textContent).toContain('Available in the selected runtime');
    await mount(createElement(ComponentPane, { node, view: { events: [], live: false }, inspection, search: { tab: 'component' }, setSearch: vi.fn() }));
    expect(host.textContent).toContain('pager.example:4317');
  });

  it('switches a tab through the URL and can pop the inspector out', async () => {
    const setSearch = vi.fn();
    await mount(createElement(ComponentPane, { node: layoutNode(comp('exporter', 'otlp/pager', 'captured')), runId: run.id, view, inspection, search: { run: run.id }, setSearch }));
    await click('Configuration');
    expect(setSearch).toHaveBeenCalledWith({ tab: 'component', view: undefined });
    const section = host.querySelector('section')!;
    const before = section.style.height;
    await act(() => host.querySelector<HTMLButtonElement>('[aria-label="Pop out inspector"]')!.click());
    expect(host.querySelector('section')!.style.height).not.toBe(before);
    expect(host.querySelector('[aria-label="Restore inspector"]')).not.toBeNull();
  });

  it('reads the configuration as written from the run that is shown', async () => {
    await mount(createElement(ComponentPane, {
      node: layoutNode(comp('processor', 'filter/pager'), 'logs/pager'), runId: run.id, view, inspection, search: { run: run.id, tab: 'component' }, setSearch: vi.fn(),
    }));
    await click('As written');
    expect(host.textContent).toContain('a: 1');
  });
});
