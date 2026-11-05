// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Diff } from '../api/types';
import { GroupedChanges } from './GroupedChanges';

let host: HTMLDivElement;
let root: Root;
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
});
afterEach(() => { act(() => root.unmount()); host.remove(); });
const before = { resourceLogs: [{ scopeLogs: [{ logRecords: [{ body: { stringValue: 'Payment declined' } }, { body: { stringValue: 'Login retry' } }, { body: { stringValue: 'Health check' } }] }] }] };
const entity = (i: number) => `resource[0].scope[0].log_record[${i}]`;
const diff: Diff = { identity: 'content', before: { resources: 1, scopes: 1, items: 3 }, after: { resources: 1, scopes: 1, items: 2 },
  changes: [0, 1].map((i) => ({ entity: entity(i), path: 'severity_text', op: 'modified', before: 'info', after: 'INFO' })), removed: [{ ref: entity(2), name: 'Health check' }] };
function render(d = diff, extra = {}) { act(() => root.render(createElement(GroupedChanges, { diff: d, before, onRecord: vi.fn(), ...extra }))); }
function click(label: string, selector = 'button') {
  const button = [...host.querySelectorAll<HTMLButtonElement>(selector)].find((b) => b.textContent?.trim() === label);
  expect(button, label).toBeTruthy(); act(() => button!.click());
}
function search(value: string) { act(() => {
  const input = host.querySelector<HTMLInputElement>('input[type=search]')!;
  Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(input, value);
  input.dispatchEvent(new Event('input', { bubbles: true }));
}); }

describe('Changes overview interactions', () => {
  it('summarizes repeated edits once and expands affected records with selection and journey actions', () => {
    const onRecord = vi.fn(), follow = vi.fn();
    render(diff, { onRecord, onFollow: () => follow });
    expect(host.querySelectorAll('.change-field')).toHaveLength(1);
    expect([...host.querySelectorAll('.change-card strong')].map((e) => e.textContent)).toEqual(['2', '2', '1', '0']);
    expect(host.querySelector('.change-field')!.textContent).toContain('2 records');
    act(() => host.querySelector<HTMLButtonElement>('.change-field-toggle')!.click());
    expect(host.querySelectorAll('.change-edit-table tbody tr')).toHaveLength(2);
    click('Select record', '.change-edit-table button');
    expect(onRecord).toHaveBeenCalledWith(entity(0));
    click('Follow record', '.change-edit-table button');
    expect(follow).toHaveBeenCalledOnce();
  });

  it('filters field edits and dropped records independently and clears search', () => {
    render();
    act(() => { const select = host.querySelector<HTMLSelectElement>('[aria-label="Change type"]')!; select.value = 'dropped'; select.dispatchEvent(new Event('change', { bubbles: true })); });
    expect(host.querySelectorAll('.change-field')).toHaveLength(0);
    expect(host.querySelector('.change-record-list')!.textContent).toContain('Health check');
    click('Clear filters');
    search('Login retry');
    expect(host.querySelector('.change-field')!.textContent).toContain('1 record');
    expect(host.querySelector('.change-record-list')).toBeNull();
    search('missing');
    expect(host.textContent).toContain('No changes match');
    click('Clear filters');
    expect(host.querySelector('.change-record-list')).not.toBeNull();
  });

  it('keeps the resource/record detail view and its unchanged-attributes control available', () => {
    render();
    click('Open resource / record details');
    expect(host.querySelector('.change-field-table')).toBeNull();
    expect(host.querySelectorAll('.chg-record')).toHaveLength(3);
    expect(host.querySelector('input[type=checkbox]')).not.toBeNull();
    click('← Back to field overview');
    expect(host.querySelector('.change-field-table')).not.toBeNull();
  });

  it('distinguishes unestablished matching from unchanged telemetry', () => {
    render({ ...diff, identity: 'unresolved', changes: [], removed: [], unresolved: ['Records could not be matched.'] });
    expect(host.textContent).toContain('No field or record changes could be established');
    expect(host.textContent).toContain('Matching limitations');
    expect(host.textContent).not.toContain('passed the telemetry through');
  });

  it('makes unmatched input/output counts explicit without inventing dropped or created records', () => {
    render({ ...diff, changes: [], removed: [], matching: { object: 0, exact: 1, high: 0, medium: 0, unmatched_before: 2, unmatched_after: 2 }, unresolved: ['Two records before and after could not be matched.'] });
    expect(host.querySelector('.change-matching')!.textContent).toContain('2 input records · 2 output records');
    expect([...host.querySelectorAll('.change-card strong')].map((e) => e.textContent)).toEqual(['0', '0', '0', '0']);
    expect(host.querySelector('.change-record-list')).toBeNull();
  });

  it('limits expanded edits to a page without losing the total affected count', () => {
    const d = { ...diff, changes: Array.from({ length: 30 }, (_, i) => ({ entity: entity(i), path: 'severity_text', op: 'modified', before: 'info', after: 'INFO' })), removed: [] };
    render(d);
    expect(host.querySelector('.change-field')!.textContent).toContain('30 records');
    act(() => host.querySelector<HTMLButtonElement>('.change-field-toggle')!.click());
    expect(host.querySelectorAll('.change-edit-table tbody tr')).toHaveLength(25);
    click('Next page');
    expect(host.querySelectorAll('.change-edit-table tbody tr')).toHaveLength(5);
  });
});
