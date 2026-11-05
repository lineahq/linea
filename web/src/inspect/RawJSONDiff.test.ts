// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { RawJSONDiff } from './RawJSONDiff';

vi.mock('./DiffViews', () => ({
  SideBySide: ({ before, after }: { before: string; after: string }) => createElement('div', { 'data-testid': 'raw-diff' }, before, after),
  RawJSON: ({ before, after }: { before: string; after: string }) => createElement('div', { 'data-testid': 'raw-snapshots' }, before, after),
}));
let host: HTMLDivElement, root: Root;
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
});
afterEach(() => { act(() => root.unmount()); host.remove(); });
function toggle(open: boolean) {
  act(() => { const details = host.querySelector('details')!; details.open = open; details.dispatchEvent(new Event('toggle')); });
}
describe('complete raw JSON diff', () => {
  it('mounts only when opened and preserves metadata absent from the readable summary', () => {
    const before = { resourceLogs: [{ scopeLogs: [{ scope: { name: 'app', version: '1' }, logRecords: [{ severityNumber: 17, timeUnixNano: '1712345678901234567', traceId: 'trace-id', spanId: 'span-id', body: { stringValue: 'hello' } }] }] }] };
    act(() => root.render(createElement(RawJSONDiff, { before, after: before })));
    expect(host.querySelector('[data-testid]')).toBeNull();
    toggle(true);
    expect(host.querySelector('[data-testid="raw-diff"]')).not.toBeNull();
    for (const field of ['severityNumber', '1712345678901234567', 'trace-id', 'span-id', '"version": "1"']) expect(host.textContent).toContain(field);
    toggle(false); expect(host.querySelector('[data-testid]')).toBeNull();
  });
  it('does not present missing buffered output as a raw deletion diff', () => {
    act(() => root.render(createElement(RawJSONDiff, { before: { recorded: true }, afterMissingReason: 'This input was buffered; no attributable output.' })));
    toggle(true);
    expect(host.textContent).toContain('buffered');
    expect(host.querySelector('[data-testid="raw-diff"]')).toBeNull();
    expect(host.querySelector('[data-testid="raw-snapshots"]')!.textContent).toContain('"recorded": true');
    expect(host.textContent).not.toContain('{}');
  });
  it('does not invent a Before snapshot for output-only data', () => {
    act(() => root.render(createElement(RawJSONDiff, { before: undefined, after: { recorded: true } })));
    toggle(true);
    expect(host.textContent).toContain('not treated as an addition');
    expect(host.querySelector('[data-testid="raw-diff"]')).toBeNull();
  });
  it('compares empty and unmatched-but-recorded payloads as text without fabricating record identity', () => {
    act(() => root.render(createElement(RawJSONDiff, { before: { resourceLogs: [] }, after: { resourceMetrics: [] } })));
    toggle(true);
    expect(host.querySelector('[data-testid="raw-diff"]')).not.toBeNull();
    expect(host.textContent).toContain('does not establish record identity');
  });
});
