// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { RecordLineage, Sampling } from '../api/types';
import { SamplingPanel } from './SamplingPanel';

let host: HTMLDivElement, root: Root;
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
});
afterEach(() => { act(() => root.unmount()); host.remove(); });

const sampling: Sampling = {
  processor: 'tail_sampling', pipeline: 'traces', decision_wait: '2s',
  policies: [{ name: 'errors', type: 'status_code', rule: 'samples traces with a span whose status is ERROR' }, { name: 'slow', type: 'latency', rule: 'samples traces lasting more than 500ms' }],
  notes: ['rate note'],
  traces: [
    { trace_id: '7da011b9ba25a325f48bd855e30e82e1', name: 'GET /search', decision: 'sampled', policy: 'slow', spans: 1, duration: '850ms', verification: 'replay_verified',
      records: ['4:resource[0].scope[0].span[1]'],
      policies: [{ policy: 'errors', result: 'not_sampled', evidence: 'span statuses: UNSET' }, { policy: 'slow', result: 'sampled', evidence: 'lasted 850ms' }] },
    { trace_id: '5b8efff798038103d269b633813fc60c', name: 'GET /health', decision: 'not_sampled', spans: 1, duration: '2ms', verification: 'replay_verified',
      policies: [{ policy: 'errors', result: 'not_sampled', evidence: 'span statuses: UNSET' }, { policy: 'slow', result: 'not_sampled', evidence: 'lasted 2ms' }] },
  ],
};
const lineage: RecordLineage[] = [{ id: 'j1', name: 'GET /search', signal: 'traces', hops: [{ seq: 4, kind: 'stage.input', parent: -1, ref: 'resource[0].scope[0].span[1]' }] }];
const rows = () => [...host.querySelectorAll<HTMLButtonElement>('.rv-row')];
const click = (el: Element) => act(() => (el as HTMLElement).click());

describe('sampling decisions panel', () => {
  it('lists each trace as a row that names the deciding policy, with the first one open', () => {
    act(() => root.render(createElement(SamplingPanel, { sampling, lineage, setSearch: vi.fn() })));
    expect(host.textContent).toContain('1 of 2 traces kept');
    expect(host.textContent).toContain('rate note');
    expect(rows().map((r) => r.querySelector('.rv-title')!.textContent)).toEqual(['GET /search', 'GET /health']);
    expect(rows()[0].querySelector('.rv-why')!.textContent).toBe('Policy slow sampled it');
    expect(rows()[1].querySelector('.rv-why')!.textContent).toBe('No policy sampled it');
    expect(rows().map((r) => r.getAttribute('aria-expanded'))).toEqual(['true', 'false']);
  });
  it('shows the ladder of policies with the evidence each read and the deciding one marked', () => {
    act(() => root.render(createElement(SamplingPanel, { sampling, lineage, setSearch: vi.fn() })));
    const steps = [...host.querySelectorAll('.rv-step')];
    expect(steps.map((s) => s.classList.contains('rv-decisive'))).toEqual([false, true]);
    expect(steps[0].textContent).toContain('no match');
    expect(steps[0].textContent).toContain('span statuses: UNSET');
    expect(steps[1].textContent).toContain('samples');
    expect(steps[1].textContent).toContain('decided');
    expect(steps[1].textContent).toContain('lasted 850ms');
    expect(steps[1].textContent).toContain('samples traces lasting more than 500ms');
    click(rows()[1]);
    expect(host.querySelector('.rv-decisive')).toBeNull();
    expect(host.textContent).toContain('lasted 2ms');
  });
  it('filters by outcome', () => {
    act(() => root.render(createElement(SamplingPanel, { sampling, lineage, setSearch: vi.fn() })));
    const pill = (l: string) => [...host.querySelectorAll<HTMLButtonElement>('.rv-filters button')].find((b) => b.textContent!.startsWith(l))!;
    expect([...host.querySelectorAll('.rv-filters button')].map((b) => b.textContent)).toEqual(['All2', 'Kept1', 'Dropped1']);
    click(pill('Dropped'));
    expect(rows().map((r) => r.querySelector('.rv-title')!.textContent)).toEqual(['GET /health']);
  });
  it('follows a trace into its record journey', () => {
    const setSearch = vi.fn();
    act(() => root.render(createElement(SamplingPanel, { sampling, lineage, setSearch })));
    click(host.querySelector('.rv-follow')!);
    expect(setSearch).toHaveBeenCalledWith({ trace: 'j1', tab: 'journey', view: undefined });
  });
  it('says when nothing was recorded rather than showing an empty list', () => {
    act(() => root.render(createElement(SamplingPanel, { sampling: undefined, setSearch: vi.fn() })));
    expect(host.textContent).toContain('No sampling decisions were recorded');
    act(() => root.render(createElement(SamplingPanel, { sampling: undefined, live: true, setSearch: vi.fn() })));
    expect(host.textContent).toContain('explained when the run ends');
  });
});
