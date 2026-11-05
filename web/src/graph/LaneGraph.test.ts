// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Graph } from '../api/types';
import type { PathHighlight } from '../inspect/journey';
import type { EdgeStats, NodeStats } from '../runs/annotations';
import { LaneGraph, type LaneGraphProps } from './LaneGraph';

let host: HTMLDivElement;
let root: Root;
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  host = document.createElement('div');
  document.body.append(host);
  root = createRoot(host);
});
afterEach(() => {
  act(() => root.unmount());
  host.remove();
});

const comp = (kind: string, id: string, pipelines: string[], mode = 'real') => ({ ref: `${kind}/${id}`, kind, id, type: id, mode, pipelines });
const graph: Graph = {
  pipelines: [
    { id: 'logs/in', signal: 'logs', receivers: ['receiver/otlp'], processors: ['processor/batch'], exporters: ['connector/route'] },
    { id: 'logs/out', signal: 'logs', receivers: ['connector/route'], processors: [], exporters: ['exporter/dbg'] },
    { id: 'traces', signal: 'traces', receivers: ['receiver/otlp'], processors: [], exporters: ['exporter/dbg'] },
  ],
  components: [
    comp('receiver', 'otlp', ['logs/in', 'traces']),
    comp('processor', 'batch', ['logs/in']),
    comp('connector', 'route', ['logs/in', 'logs/out']),
    comp('exporter', 'dbg', ['logs/out', 'traces'], 'captured'),
    comp('extension', 'health_check', [], 'simulated'),
  ],
  edges: [
    { pipeline: 'logs/in', signal: 'logs', from: 'receiver/otlp', to: 'processor/batch' },
    { pipeline: 'logs/in', signal: 'logs', from: 'processor/batch', to: 'connector/route' },
    { pipeline: 'logs/out', signal: 'logs', from: 'connector/route', to: 'exporter/dbg' },
    { pipeline: 'traces', signal: 'traces', from: 'receiver/otlp', to: 'exporter/dbg' },
  ],
};

function render(props: Partial<LaneGraphProps> = {}) {
  const onSelect = vi.fn();
  act(() => root.render(createElement(LaneGraph, { graph, onSelect, ...props })));
  return onSelect;
}
const $$ = (sel: string) => [...host.querySelectorAll<HTMLElement>(sel)];
const node = (label: string) => $$('button.od-node').filter((b) => b.getAttribute('aria-label')!.startsWith(label));

describe('LaneGraph', () => {
  it('draws one lane per pipeline with nodes, hand-offs, links and the extension row', () => {
    render();
    expect($$('.od-lane__head').map((h) => h.textContent)).toEqual(['logs/in', 'logs/out', 'traces']);
    expect($$('.od-lane')[0]!.querySelectorAll('button.od-node')).toHaveLength(3);
    expect($$('.od-lane')[1]!.querySelector('.od-handoff')!.textContent).toContain('route');
    expect($$('.od-lane')[0]!.querySelectorAll('.od-link')).toHaveLength(2);
    expect(host.querySelector('.od-section-label')!.textContent).toBe('Extensions');
    expect(host.querySelector('[aria-label^="extension health_check"]')).toBeTruthy();
    expect(host.querySelector('.od-graph--draft')).toBeNull();
    expect(node('exporter dbg')[0]!.querySelector('.od-mode--captured')).toBeTruthy();
  });

  it('adds the draft canvas and the stamp', () => {
    render({ draft: true, stamp: 'Recorded · run #18 · read-only' });
    expect(host.querySelector('.od-graph--draft')).toBeTruthy();
    expect(host.querySelector('.od-graph__stamp')!.textContent).toBe('Recorded · run #18 · read-only');
  });

  it('selects every occurrence of a shared node, and deselects on the canvas', () => {
    const onSelect = render({ selected: 'exporter/dbg' });
    const dbg = node('exporter dbg');
    expect(dbg).toHaveLength(2);
    expect(dbg.every((b) => b.classList.contains('od-node--selected') && b.getAttribute('aria-pressed') === 'true')).toBe(true);
    act(() => node('receiver otlp')[0]!.click());
    expect(onSelect).toHaveBeenLastCalledWith('receiver/otlp');
    act(() => node('processor batch')[0]!.click());
    expect(onSelect).toHaveBeenLastCalledWith('logs/in|processor/batch');
    act(() => host.querySelector<HTMLElement>('.od-lane')!.click());
    expect(onSelect).toHaveBeenLastCalledWith(undefined);
    act(() => $$('.od-lane .od-handoff')[0]!.click());
    expect(onSelect).toHaveBeenLastCalledWith('connector/route');
  });

  it('shows run stats, link counts and a dropped link', () => {
    const nodeStats = new Map<string, NodeStats>([
      ['receiver/otlp', { itemsIn: 5 }],
      ['logs/in|processor/batch', { itemsIn: 5, itemsOut: 0, dropped: 5 }],
      ['exporter/dbg', { itemsIn: 0 }],
    ]);
    const edgeStats = new Map<string, EdgeStats>([
      ['logs/in|receiver/otlp->logs/in|processor/batch', { items: 5 }],
      ['logs/in|logs/in|processor/batch->connector/route', { items: 0 }],
    ]);
    render({ nodeStats, edgeStats });
    expect(node('processor batch')[0]!.querySelector('.od-node__meta')!.textContent).toBe('5 → 0 · dropped 5');
    expect(node('exporter dbg')[0]!.querySelector('.od-node__meta')!.textContent).toBe('received 0');
    const links = $$('.od-lane')[0]!.querySelectorAll('.od-link');
    expect(links[0]!.textContent).toBe('5');
    expect(links[1]!.classList.contains('od-link--dropped')).toBe(true);
  });

  it('emphasizes a followed journey and dims the rest', () => {
    const path: PathHighlight = {
      nodes: new Map([
        ['receiver/otlp', 'passed'],
        ['logs/in|processor/batch', 'stopped'],
      ]),
      edges: new Set(['receiver/otlp->logs/in|processor/batch']),
      pipelines: new Set(['logs/in']),
    };
    render({ path });
    const lane = $$('.od-lane')[0]!;
    expect(node('receiver otlp')[0]!.classList.contains('od-node--path')).toBe(true);
    const batch = node('processor batch')[0]!;
    expect(batch.classList.contains('od-node--stopped')).toBe(true);
    expect(batch.querySelector('.od-node__flag')!.textContent).toBe('Stopped here');
    expect(node('exporter dbg')[0]!.classList.contains('od-node--dim')).toBe(true);
    expect(lane.querySelectorAll('.od-link')[0]!.classList.contains('od-link--active')).toBe(true);
    expect(lane.querySelectorAll('.od-link')[1]!.classList.contains('od-link--active')).toBe(false);
  });

  it('filters by signal', () => {
    render();
    act(() => $$('.od-seg button').find((b) => b.textContent === 'Traces')!.click());
    expect($$('.od-lane__head').map((h) => h.textContent)).toEqual(['traces']);
    expect($$('.od-seg button').find((b) => b.textContent === 'Traces')!.getAttribute('aria-pressed')).toBe('true');
    act(() => $$('.od-seg button').find((b) => b.textContent === 'Metrics')!.click());
    expect(host.textContent).toContain('No metrics pipelines');
  });

  it('searches and dims what does not match', () => {
    render();
    const input = host.querySelector<HTMLInputElement>('input[aria-label="Search graph"]')!;
    act(() => {
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(input, 'batch');
      input.dispatchEvent(new Event('input', { bubbles: true }));
    });
    expect(node('processor batch')[0]!.classList.contains('od-node--dim')).toBe(false);
    expect(node('receiver otlp')[0]!.classList.contains('od-node--dim')).toBe(true);
    expect(host.querySelector('[role="status"]')!.textContent).toBe('1 match');
  });

  it('zooms within 50-150 percent', () => {
    render();
    const zoom = () => host.querySelector('.lg-zoom')!.textContent;
    const press = (label: string) => act(() => host.querySelector<HTMLElement>(`[aria-label="${label}"]`)!.click());
    expect(zoom()).toBe('100%');
    press('Zoom in');
    expect(zoom()).toBe('110%');
    expect(host.querySelector<HTMLElement>('.lg-content')!.style.transform).toBe('scale(1.1)');
    for (let i = 0; i < 12; i++) press('Zoom out');
    expect(zoom()).toBe('50%');
    expect(host.querySelector<HTMLButtonElement>('[aria-label="Zoom out"]')!.disabled).toBe(true);
    press('Fit to view');
    expect(zoom()).toBe('100%');
  });

  it('opens the execution modes legend and closes it with Escape', () => {
    render();
    const toggle = $$('button').find((b) => b.textContent === 'Execution modes')!;
    expect(host.querySelector('.lg-legend')).toBeNull();
    act(() => toggle.click());
    const legend = host.querySelector('.lg-legend')!;
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    expect([...legend.querySelectorAll('strong')].map((s) => s.textContent)).toEqual([
      'Real',
      'Dependency mocked',
      'Captured',
      'Simulated',
      'Stand-in',
    ]);
    act(() => legend.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true })));
    expect(host.querySelector('.lg-legend')).toBeNull();
  });
});
