// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ApiError } from '../api/client';
import { RunSummary } from './RunSummary';
import type { Run } from '../api/types';

let host: HTMLDivElement, root: Root;
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
});
afterEach(() => { act(() => root.unmount()); host.remove(); });
const counts = (items: number) => ({ items, resources: 1, scopes: 1 });
function run(): Run { return { id: 'test-run', project_id: 'default', created: '', state: 'done', report: { report_schema: '1',
  result: { mode: 'quick', status: 'completed', inputs: [
    { receiver: 'otlp', signal: 'logs', injected: true, counts: counts(4) },
    { receiver: 'rejected', signal: 'logs', injected: false, counts: counts(9), error: 'bad payload' },
  ], outputs: [{ exporter: 'archive', signal: 'logs', counts: counts(4), batches: 1 }, { exporter: 'pager', signal: 'logs', counts: counts(2), batches: 1 }] },
  assertions: { outcome: 'pass', run_status: 'completed', results: [{ name: 'output', outcome: 'pass', target: 'archive' }] },
} }; }
function render(r: Run) { act(() => root.render(createElement(RunSummary, { view: { run: r, events: [], live: false } }))); }

describe('run overview progressive disclosure', () => {
  it('shows honest captured totals without equating fan-out to drops and keeps technical tables closed', () => {
    render(run());
    expect([...host.querySelectorAll('.overview-cards strong')].map((n) => n.textContent)).toEqual(['4', '6', '2']);
    expect(host.textContent).toContain('not a dropped-record count');
    expect(host.querySelector('.detail-disclosure')!.hasAttribute('open')).toBe(false);
    expect(host.querySelector('.evidence-limits')!.hasAttribute('open')).toBe(false);
    expect(host.querySelector('.lim-warning')!.closest('details')).toBeNull();
    expect(host.querySelector('.assertions')!.closest('details')!.open).toBe(false);
  });
  it('opens failed checks automatically and keeps engine failures visible', () => {
    const r = run(); r.report!.assertions!.outcome = 'fail'; render(r);
    expect(host.querySelector('.assertions')!.closest('details')!.open).toBe(true);
    render({ id: 'error', project_id: 'default', created: '', state: 'error', error: 'startup failed' });
    expect(host.querySelector('[role=alert]')!.textContent).toContain('startup failed');
  });
  it('lists stand-ins in the open', () => {
    const r = run();
    r.report!.topology = { components: [
      { ref: 'processor/geoip', kind: 'processor', id: 'geoip', type: 'geoip', mode: 'stand_in', note: 'not in the distribution; a stand-in passes data through unchanged' },
      { ref: 'exporter/otlp', kind: 'exporter', id: 'otlp', type: 'otlp', mode: 'captured', stand_in_values: ['file:/certs/ca.pem'] },
    ] } as never;
    r.report!.references = [
      { scheme: 'file', target: '/certs/ca.pem', source: 'stand_in', reason: 'missing', stand_in: 'standin-1' },
      { scheme: 'env', target: 'REGION', source: 'scenario' },
    ];
    render(r);
    const section = host.querySelector('.stand-ins')!;
    expect(section.closest('details')).toBeNull();
    expect([...section.querySelectorAll('li code')].map((n) => n.textContent)).toEqual(['processor/geoip', 'exporter/otlp', '${file:/certs/ca.pem}']);
    expect(section.textContent).toContain('passes data through unchanged');
    expect(section.textContent).toContain('mocks.files');
    expect(host.querySelector('.evidence-limits')!.textContent).not.toContain('geoip');
  });
  it('names the runtime, says unverified and lists custom modules', () => {
    const r = run();
    r.report!.runtime = { kind: 'container', name: 'v0163', collector_version: '0.163.0', verified: false,
      custom_modules: [{ module: 'example.com/stamp', source: 'git https://example.com/stamp.git', commit: 'abcdef0123456789' }] };
    render(r);
    const facts = host.querySelector('.facts')!;
    expect(facts.textContent).toContain('v0163 · Collector 0.163.0');
    expect(facts.querySelector('.badge.unverified')!.textContent).toBe('unverified');
    expect(facts.querySelector('.custom-modules')!.textContent).toContain('example.com/stamp from git https://example.com/stamp.git @ abcdef012345');
    expect(facts.querySelector('.missing-features')).toBeNull();
    r.report!.runtime = { kind: 'container', name: 'lean', collector_version: '0.160.0', verified: true, missing_features: ['routing_explanation'] };
    render(r);
    expect(host.querySelector('.missing-features')!.textContent).toBe('This runtime lacks routing explanations (no connector/routing); that evidence is absent.');
    r.report!.runtime = { kind: 'built_in', collector_version: '0.162.0', verified: true };
    render(r);
    expect(host.querySelector('.facts')!.textContent).toContain('Built-in · Collector 0.162.0');
    expect(host.querySelector('.badge.unverified')).toBeNull();
  });
  it('reports load errors instead of an indefinite loading placeholder', () => {
    act(() => root.render(createElement(RunSummary, { view: { error: new Error('Unknown run'), events: [], live: false } })));
    expect(host.textContent).toContain('Could not load this run: Unknown run');
  });
  it('says a deleted run was deleted', () => {
    const error = new ApiError(404, 'run was deleted', { error: 'run was deleted' });
    act(() => root.render(createElement(RunSummary, { view: { error, events: [], live: false } })));
    expect(host.textContent).toBe('This run was deleted.');
  });
});
