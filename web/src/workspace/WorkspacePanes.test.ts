// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { WorkspacePanes } from './WorkspacePanes';
import { DEFAULT_PANES, PANE_STORAGE_KEY } from './panes';

let host: HTMLDivElement, root: Root;
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  localStorage.clear();
  vi.stubGlobal('ResizeObserver', class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get').mockReturnValue(1200);
  vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get').mockReturnValue(800);
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
});
afterEach(() => { act(() => root.unmount()); host.remove(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });
function render(bottom: string | null = 'Component content') {
  act(() => root.render(createElement(WorkspacePanes, { left: 'Left content', graph: 'Graph content', bottom: bottom ?? undefined })));
}
function key(element: Element, name: string) { act(() => element.dispatchEvent(new KeyboardEvent('keydown', { key: name, bubbles: true }))); }

describe('resizable workspace', () => {
  it('shows the left pane, the graph and the bottom pane', () => {
    render();
    expect(host.querySelector('.od-wbpanes__left')!.textContent).toBe('Left content');
    expect(host.querySelector('.od-wbpanes__graph')!.textContent).toBe('Graph content');
    expect(host.querySelector('.od-wbpanes__bottom')!.textContent).toBe('Component content');
  });
  it('has no horizontal divider without a bottom pane', () => {
    render(null);
    expect(host.querySelectorAll('[role="separator"]')).toHaveLength(1);
    expect(host.querySelector('[aria-orientation="horizontal"]')).toBeNull();
  });
  it('supports keyboard resizing and persists the chosen split', () => {
    render();
    const left = host.querySelector('[aria-label="Resize left pane"]')!;
    const before = Number(left.getAttribute('aria-valuenow'));
    key(left, 'ArrowRight');
    expect(Number(left.getAttribute('aria-valuenow'))).toBe(before + 2);
    expect(JSON.parse(localStorage.getItem(PANE_STORAGE_KEY)!).left).toBeCloseTo(0.36);
    key(left, 'Home'); expect(left.getAttribute('aria-valuenow')).toBe(left.getAttribute('aria-valuemin'));
    key(left, 'End'); expect(left.getAttribute('aria-valuenow')).toBe(left.getAttribute('aria-valuemax'));
    const horizontal = host.querySelector('[aria-orientation="horizontal"]')!;
    const upper = Number(horizontal.getAttribute('aria-valuenow'));
    key(horizontal, 'ArrowDown'); expect(Number(horizontal.getAttribute('aria-valuenow'))).toBe(upper + 2);
    key(horizontal, 'ArrowUp'); key(horizontal, 'ArrowUp'); expect(Number(horizontal.getAttribute('aria-valuenow'))).toBe(upper - 2);
  });
  it('loads persisted sizes and resets each divider independently', () => {
    localStorage.setItem(PANE_STORAGE_KEY, '{"left":0.4,"upper":0.5}'); render();
    const left = host.querySelector('[aria-orientation="vertical"]')!;
    expect(left.getAttribute('aria-valuenow')).toBe('40');
    act(() => left.dispatchEvent(new MouseEvent('dblclick', { bubbles: true })));
    expect(JSON.parse(localStorage.getItem(PANE_STORAGE_KEY)!)).toEqual({ left: DEFAULT_PANES.left, upper: 0.5 });
  });
  it('sizes the left pane and graph from the ratios', () => {
    localStorage.setItem(PANE_STORAGE_KEY, '{"left":0.4,"upper":0.5}'); render();
    const style = (host.querySelector('.od-wbpanes') as HTMLElement).style;
    expect(style.getPropertyValue('--left-pane')).toBe(`${0.4 * 1192}px`);
    expect(style.getPropertyValue('--upper-pane')).toBe(`${0.5 * 792}px`);
  });
  it('keeps the split of another page under its own key, from its own defaults', () => {
    localStorage.setItem(PANE_STORAGE_KEY, '{"left":0.4,"upper":0.5}');
    act(() => root.render(createElement(WorkspacePanes, {
      left: 'Input', graph: 'Statements', bottom: 'Result', storageKey: 'page.panes', defaults: { left: 0.3, upper: 0.25 }, upperLabel: 'Resize statements and result panes',
    })));
    const upper = host.querySelector('[aria-label="Resize statements and result panes"]')!;
    expect(upper.getAttribute('aria-valuenow')).toBe('25');
    key(upper, 'ArrowDown');
    act(() => host.querySelector('[aria-label="Resize left pane"]')!.dispatchEvent(new MouseEvent('dblclick', { bubbles: true })));
    expect(JSON.parse(localStorage.getItem('page.panes')!)).toEqual({ left: 0.3, upper: 0.27 });
    expect(localStorage.getItem(PANE_STORAGE_KEY)).toBe('{"left":0.4,"upper":0.5}');
  });
  it('works when local storage is disabled', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('disabled'); });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('disabled'); });
    expect(render).not.toThrow();
  });
});
