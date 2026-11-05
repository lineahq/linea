// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { RecordLineage, Routing, StageInput } from '../api/types';
import { RoutingList } from './RoutingList';

let host: HTMLDivElement, root: Root;
beforeEach(() => { (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true; host = document.createElement('div'); document.body.append(host); root = createRoot(host); });
afterEach(() => { act(() => root.unmount()); host.remove(); });

const input = { seq: 4, routes: [{ seq: 6, pipeline: 'logs/audit', items: 2 }, { seq: 8, pipeline: 'logs/default', items: 3 }] } as unknown as StageInput;
const journey = (i: number, name: string, deliveredTo: number[], end?: string): RecordLineage => ({
  id: `j${i}`, name, signal: 'logs',
  hops: [{ parent: -1, seq: 1, kind: 'input.injected', ref: `r${i}` }, { parent: 0, seq: 4, kind: 'stage.input', ref: `r${i}`, end }, ...deliveredTo.map((seq) => ({ parent: 1, seq, kind: 'stage.input', ref: `r${i}` }))],
});
const lineage = [journey(0, 'login ok', [6, 8]), journey(1, 'health', [8]), journey(2, 'nobody', [], 'not_routed')];
const rows = () => [...host.querySelectorAll<HTMLButtonElement>('.rv-row')];
const click = (el: Element) => act(() => (el as HTMLElement).click());
function render(routing?: Routing) {
  const follow = vi.fn(), openRoute = vi.fn();
  act(() => root.render(createElement(RoutingList, { input, lineage, routing, follow, openRoute })));
  return { follow, openRoute };
}

describe('routing list', () => {
  it('states for each record where it was delivered, copies included', () => {
    render();
    expect(rows().map((r) => r.querySelector('.rv-why')!.textContent)).toEqual(['Copied to logs/audit and logs/default', 'Delivered to logs/default', 'Not routed to any pipeline']);
    expect([...rows()[0].querySelectorAll('.rv-pill')].map((p) => p.textContent)).toEqual(['logs/audit', 'logs/default']);
    expect(rows()[2].querySelector('.rv-pill-drop')!.textContent).toBe('not routed');
  });
  it('opens to the deliveries and follows the record or a delivery', () => {
    const { follow, openRoute } = render();
    click(host.querySelectorAll('.rv-panel .chip')[1]);
    expect(openRoute).toHaveBeenCalledWith(8, 'logs/default');
    click(host.querySelector('.rv-follow')!);
    expect(follow).toHaveBeenCalledWith('j0');
  });
  it('filters copied and unrouted records', () => {
    render();
    const pill = (l: string) => [...host.querySelectorAll<HTMLButtonElement>('.rv-filters button')].find((b) => b.textContent!.startsWith(l))!;
    expect([...host.querySelectorAll('.rv-filters button')].map((b) => b.textContent)).toEqual(['All3', 'Routed2', 'Copied1', 'Not routed1']);
    click(pill('Not routed'));
    expect(rows().map((r) => r.querySelector('.rv-title')!.textContent)).toEqual(['nobody']);
    click(rows()[0]);
    expect(host.querySelector('.rv-panel')!.textContent).toContain('did not deliver this record');
  });

  const routing: Routing = {
    connector: 'routing', input: 4, verification: 'replay_verified',
    rules: [
      { index: 0, condition: 'log.severity_number >= SEVERITY_NUMBER_ERROR', action: 'copy', pipelines: ['logs/audit'] },
      { index: 1, condition: 'resource.attributes["service.name"] == "audit"', action: 'move', pipelines: ['logs/default'] },
    ],
    default_pipelines: ['logs/default'],
    records: [{ ref: 'r0', rules: [0], default: true }, { ref: 'r1', default: true }, { ref: 'r2' }],
  };
  it('says which rule routed each record when the replay confirmed it', () => {
    render(routing);
    expect(rows().map((r) => r.querySelector('.rv-why')!.textContent)).toEqual([
      'Matched rule 0 · copied to logs/audit, and the original continued to the default',
      'No rule matched · sent to the default (logs/default)',
      'No rule matched and there is no default pipeline',
    ]);
    expect([...rows()[0].querySelectorAll('.rv-pill')].map((p) => p.textContent)).toEqual(['rule 0', 'default']);
  });
  it('opens to the rules in table order with what each one did', () => {
    render(routing);
    const steps = [...host.querySelectorAll('.rv-step')];
    expect(steps.map((s) => s.querySelector('.rv-step-result')!.textContent)).toEqual(['matched', 'no match', 'received it']);
    expect(steps[0].textContent).toContain('log.severity_number >= SEVERITY_NUMBER_ERROR');
    expect(steps[0].textContent).toContain('copies to logs/audit');
    expect(steps.map((s) => s.classList.contains('rv-decisive'))).toEqual([true, false, true]);
  });
  it('shows only deliveries when the replay did not confirm the rules', () => {
    render({ ...routing, verification: 'replay_differs', records: undefined });
    expect(rows()[0].querySelector('.rv-why')!.textContent).toBe('Copied to logs/audit and logs/default');
    expect(host.querySelector('.rv-step')).toBeNull();
  });
});
