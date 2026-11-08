// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { BatchingPanel } from './BatchingPanel';
import { metadataBatching, sampleBatching } from './batching.fixture';

let host: HTMLDivElement, root: Root;
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
});
afterEach(() => { act(() => root.unmount()); host.remove(); });

const all = (sel: string) => [...host.querySelectorAll(sel)];
const cells = (table: string) => all(`table[aria-label="${table}"] tbody tr`).map((tr) => [...tr.children].map((c) => c.textContent));

describe('batches panel', () => {
  it('shows the impact and every batch with why it left', () => {
    act(() => root.render(createElement(BatchingPanel, { batching: sampleBatching })));
    expect(cells('What batching changed')[0]).toEqual(['Requests', '5', '4', 'fewer, larger requests to the exporters']);
    expect(host.querySelector('table[aria-label="Batchers"]')).toBeNull();
    expect(host.textContent).toContain('Every request shares one batcher');
    expect(all('.rv-row .rv-title-line').map((t) => t.textContent)).toEqual([
      expect.stringContaining('Batch 1: 5 log records'), expect.stringContaining('Batch 2: 1 log record'),
      expect.stringContaining('Batch 3: 6 log records'), expect.stringContaining('Batch 4: 3 log records'),
    ]);
  });
  it('opens a batch to the requests it carries', () => {
    act(() => root.render(createElement(BatchingPanel, { batching: sampleBatching })));
    act(() => { (all('.rv-row')[0] as HTMLElement).click(); });
    expect(cells('Contents')).toEqual([['Request 1', 'checkout', '2'], ['Request 2', 'cart', '1'], ['Request 3', 'checkout', '2']]);
    expect(host.textContent).toContain('3 resource blocks for 2 distinct resources');
  });
  it('lists one batcher per metadata value and tags each batch with it', () => {
    act(() => root.render(createElement(BatchingPanel, { batching: metadataBatching })));
    expect(cells('Batchers')).toEqual([
      ['tenant=acme', '2', '4', '1', '1 size reached'],
      ['tenant=globex', '1', '2', '1', '1 timer'],
      ['no tenant', '1', '1', '1', '1 timer'],
    ]);
    expect(all('.rv-row')[2].textContent).toContain('tenant=globex');
  });
  it('says when there is nothing to explain yet', () => {
    act(() => root.render(createElement(BatchingPanel, { live: true })));
    expect(host.textContent).toContain('explained when the run ends');
  });
});
