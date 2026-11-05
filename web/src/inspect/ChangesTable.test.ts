// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Change } from '../api/types';
import { ChangesTable } from './ChangesTable';

let host: HTMLDivElement, root: Root;
beforeEach(() => { (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true; host = document.createElement('div'); document.body.append(host); root = createRoot(host); });
afterEach(() => { act(() => root.unmount()); host.remove(); });

const changes = Array.from({ length: 60 }, (_, i) => ({ entity: `log_record[${i}]`, path: 'attributes["n"]', op: 'value_changed', before: i, after: i + 1 }) as Change);
const rows = () => [...host.querySelectorAll('tbody tr')];
const button = (l: string) => [...host.querySelectorAll<HTMLButtonElement>('.signal-pagination button')].find((b) => b.textContent === l)!;

describe('changes table', () => {
  it('renders 25 changes at a time and pages through the rest', () => {
    act(() => root.render(createElement(ChangesTable, { changes, onRecord: vi.fn() })));
    expect(rows()).toHaveLength(25);
    expect(host.querySelector('.signal-pagination')!.textContent).toContain('1–25 of 60');
    act(() => button('Next changes').click());
    act(() => button('Next changes').click());
    expect(rows()).toHaveLength(10);
    expect(rows()[0].textContent).toContain('log_record[50]');
    expect(button('Next changes').disabled).toBe(true);
  });
  it('sorts by a column, numbers within names in natural order', () => {
    act(() => root.render(createElement(ChangesTable, { changes: changes.slice(0, 12).reverse(), onRecord: vi.fn() })));
    const header = () => [...host.querySelectorAll('th')].find((h) => h.textContent?.startsWith('Record'))!;
    act(() => header().click());
    expect(header().getAttribute('aria-sort')).toBe('ascending');
    expect(rows().map((r) => r.querySelector('td')!.textContent)).toEqual(changes.slice(0, 12).map((c) => c.entity));
    act(() => header().click());
    expect(header().getAttribute('aria-sort')).toBe('descending');
    expect(rows()[0].textContent).toContain('log_record[11]');
  });
  it('shows no pagination for one page', () => {
    act(() => root.render(createElement(ChangesTable, { changes: changes.slice(0, 3), onRecord: vi.fn() })));
    expect(rows()).toHaveLength(3);
    expect(host.querySelector('.signal-pagination')).toBeNull();
  });
});
