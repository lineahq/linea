// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { JourneyPanel } from './JourneyPanel';
import type { RecordLineage } from '../api/types';

let host: HTMLDivElement, root: Root;
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
});
afterEach(() => { act(() => root.unmount()); host.remove(); });
const lineage: RecordLineage[] = Array.from({ length: 30 }, (_, i) => ({ id: `record-${i}`, name: `Message ${i}`, receiver: 'otlp', signal: 'logs', deliveries: [{ exporter: 'backend', pipeline: 'logs', hop: 0, changed: false }] }));
describe('journey overview', () => {
  it('bounds the record matrix and supports searching and opening a journey', () => {
    const setSearch = vi.fn();
    act(() => root.render(createElement(JourneyPanel, { lineage, events: [], live: false, search: {}, setSearch })));
    expect(host.querySelectorAll('tbody tr')).toHaveLength(25);
    act(() => [...host.querySelectorAll('button')].find((b) => b.textContent === 'Next records')!.click());
    expect(host.querySelectorAll('tbody tr')).toHaveLength(5);
    act(() => {
      const input = host.querySelector('input')!;
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(input, 'Message 29');
      input.dispatchEvent(new Event('input', { bubbles: true }));
    });
    expect(host.querySelectorAll('tbody tr')).toHaveLength(1);
    act(() => host.querySelector<HTMLButtonElement>('tbody button')!.click());
    expect(setSearch).toHaveBeenCalledWith({ trace: 'record-29' });
  });
});
