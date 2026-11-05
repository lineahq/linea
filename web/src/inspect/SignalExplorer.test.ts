// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { SignalExplorer } from './SignalExplorer';

let host: HTMLDivElement;
let root: Root;
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  host = document.createElement('div');
  document.body.append(host);
  root = createRoot(host);
});
afterEach(() => { act(() => root.unmount()); host.remove(); });

const logs = (count: number) => ({ resourceLogs: [{ resource: { attributes: [{ key: 'service.name', value: { stringValue: 'api' } }] }, scopeLogs: [{ logRecords: Array.from({ length: count }, (_, i) => ({ body: { stringValue: `Message ${i}` }, severityNumber: i === 0 ? 17 : 9, attributes: [{ key: 'index', value: { intValue: String(i) } }] })) }] }] });
const click = (label: string) => {
  const button = [...host.querySelectorAll('button')].find((b) => b.textContent === label);
  expect(button, `button ${label}`).toBeTruthy();
  act(() => button!.click());
};
const input = (value: string) => act(() => {
  const element = host.querySelector('input')!;
  Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(element, value);
  element.dispatchEvent(new Event('input', { bubbles: true }));
});

describe('SignalExplorer interactions', () => {
  it('keeps secondary filters closed until requested and identifies active hidden filters', () => {
    act(() => root.render(createElement(SignalExplorer, { payload: logs(2) })));
    const filters = host.querySelector<HTMLButtonElement>('[aria-controls]')!;
    const panel = host.querySelector<HTMLElement>(`[id="${filters.getAttribute('aria-controls')}"]`)!;
    expect(panel.hidden).toBe(true);
    click('Filters & order');
    expect(panel.hidden).toBe(false);
    act(() => { const severity = panel.querySelectorAll('select')[1]; severity.value = 'ERROR'; severity.dispatchEvent(new Event('change', { bubbles: true })); });
    expect(host.querySelectorAll('.signal-row')).toHaveLength(1);
    click('Filters & order · active');
    expect(panel.hidden).toBe(true);
    expect(filters.textContent).toContain('active');
  });
  it('bounds rendering, navigates pages and returns to page one when searching', () => {
    act(() => root.render(createElement(SignalExplorer, { payload: logs(27) })));
    expect(host.querySelectorAll('.signal-row')).toHaveLength(25);
    click('Next page');
    expect(host.querySelectorAll('.signal-row')).toHaveLength(2);
    expect(host.textContent).toContain('Page 2 of 2');
    input('message 26');
    expect(host.querySelectorAll('.signal-row')).toHaveLength(1);
    expect(host.textContent).toContain('1 of 27 log records');
    click('Clear filters');
    expect(host.querySelectorAll('.signal-row')).toHaveLength(25);
  });

  it('expands readable context and connects selection and following to existing entities', () => {
    const onRecord = vi.fn(), follow = vi.fn();
    const entity = 'resource[0].scope[0].log_record[0]';
    act(() => root.render(createElement(SignalExplorer, { payload: logs(1), onRecord, onFollow: (e) => e === entity ? follow : undefined })));
    const expand = host.querySelector<HTMLButtonElement>('.signal-expand')!;
    expect(expand.getAttribute('aria-expanded')).toBe('false');
    act(() => expand.click());
    expect(expand.getAttribute('aria-expanded')).toBe('true');
    expect(host.textContent).toContain('Instrumentation scope');
    expect(host.textContent).toContain('service.name');
    click('Select record in inspector');
    expect(onRecord).toHaveBeenCalledWith(entity);
    click('Follow record');
    expect(follow).toHaveBeenCalledOnce();
  });

  it('shows empty filter results and allows clearing an externally selected record', () => {
    const onRecord = vi.fn();
    act(() => root.render(createElement(SignalExplorer, { payload: logs(2), record: 'missing', onRecord })));
    expect(host.textContent).toContain('The selected record is not present');
    click('Show all records');
    expect(onRecord).toHaveBeenCalledWith(undefined);
  });

  it('renders log bodies as text and handles payloads with no records', () => {
    const payload = logs(1);
    payload.resourceLogs[0].scopeLogs[0].logRecords[0].body.stringValue = '<img src=x onerror=alert(1)>';
    act(() => root.render(createElement(SignalExplorer, { payload })));
    expect(host.querySelector('img')).toBeNull();
    expect(host.textContent).toContain('<img src=x onerror=alert(1)>');
    act(() => root.render(createElement(SignalExplorer, { payload: logs(0) })));
    expect(host.textContent).toContain('This payload contains no records.');
  });
});
