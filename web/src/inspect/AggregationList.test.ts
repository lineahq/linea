// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Aggregation, RecordLineage } from '../api/types';
import { AggregationList } from './AggregationList';

let host: HTMLDivElement, root: Root;
beforeEach(() => { (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true; host = document.createElement('div'); document.body.append(host); root = createRoot(host); });
afterEach(() => { act(() => root.unmount()); host.remove(); });

const rec = (i: number) => `resource[0].scope[0].log_record[${i}]`;
const journey = (i: number, name: string): RecordLineage => ({ id: `1:${rec(i)}`, name, signal: 'logs', hops: [{ parent: -1, seq: 4, kind: 'stage.input', ref: rec(i) }] });
const point = 'resource[0].scope[0].metric[0].data_point[0]';
const agg: Aggregation = {
  connector: 'count', input: 4, output: 5, verification: 'replay_verified',
  points: [{ ref: point, metric: 'app.log.errors', attributes: { 'service.name': 'payments' }, value: 2, records: [rec(0), rec(1)] }],
  uncounted: [rec(2)],
};
const lineage = [journey(0, 'card declined'), journey(1, 'card expired'), journey(2, 'cache miss'), { id: `5:${point}`, name: 'app.log.errors', signal: 'metrics', origin: 'aggregated', connector: 'count' } as RecordLineage];
const rows = () => [...host.querySelectorAll<HTMLButtonElement>('.rv-row')];
const click = (el: Element) => act(() => (el as HTMLElement).click());
const render = (a = agg, follow = vi.fn()) => { act(() => root.render(createElement(AggregationList, { a, lineage, follow }))); return follow; };

describe('aggregation list', () => {
  it('shows a row per data point and one per record nothing counted', () => {
    render();
    expect(rows()).toHaveLength(2);
    expect(rows()[0].querySelector('.rv-metric')!.textContent).toBe('app.log.errors');
    expect(rows()[0].textContent).toContain('service.name=payments');
    expect(rows()[0].querySelector('.rv-why')!.textContent).toBe('Counted from 2 records');
    expect(rows()[0].querySelector('.rv-pill')!.textContent).toBe('2');
    expect(rows()[1].textContent).toContain('cache miss');
    expect(rows()[1].querySelector('.rv-why')!.textContent).toBe('Not counted by any metric');
    expect(rows().map((r) => r.getAttribute('aria-expanded'))).toEqual(['true', 'false']);
  });
  it('opens a data point to the records it counted and follows them', () => {
    const follow = render();
    const chips = [...host.querySelectorAll<HTMLButtonElement>('.rv-panel .chip')];
    expect(chips.map((c) => c.textContent)).toEqual(['card declined', 'card expired']);
    click(chips[1]);
    expect(follow).toHaveBeenCalledWith(`1:${rec(1)}`);
    click(host.querySelector('.rv-follow')!);
    expect(follow).toHaveBeenLastCalledWith(`5:${point}`);
  });
  it('filters data points from records that were not counted', () => {
    render();
    const pill = (l: string) => [...host.querySelectorAll<HTMLButtonElement>('.rv-filters button')].find((b) => b.textContent!.startsWith(l))!;
    expect([...host.querySelectorAll('.rv-filters button')].map((b) => b.textContent)).toEqual(['All2', 'Data points1', 'Not counted1']);
    click(pill('Not counted'));
    expect(rows().map((r) => r.textContent)).toEqual([expect.stringContaining('cache miss')]);
  });
  it('does not claim which records a point counted when the replay did not verify it', () => {
    render({ ...agg, verification: 'replay_differs', points: [{ ...agg.points![0], records: [] }], uncounted: [] });
    expect(rows()[0].querySelector('.rv-why')!.textContent).toBe('The records it counted are not attributed');
    expect(host.querySelector('.rv-panel')!.textContent).toContain('no records are attributed');
  });
  it('links the counted records of a data point 25 at a time', () => {
    const many = { ...agg, points: [{ ...agg.points![0], value: 30, records: Array.from({ length: 30 }, (_, i) => rec(i)) }], uncounted: [] };
    render(many);
    expect(host.querySelectorAll('.rv-panel .journey-sources > *')).toHaveLength(25);
    const next = [...host.querySelectorAll<HTMLButtonElement>('.rv-panel .signal-pagination button')].find((b) => b.textContent === 'Next counted records')!;
    click(next);
    expect(host.querySelectorAll('.rv-panel .journey-sources > *')).toHaveLength(5);
  });
});
