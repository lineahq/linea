// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Event, RecordLineage, Stage } from '../api/types';
import type { LayoutNode } from '../graph/layout';
import { TelemetryInspector } from './TelemetryInspector';

vi.mock('../ottl/OTTLPanel', () => ({ OTTLPanel: () => null }));
vi.mock('./DiffViews', () => ({ RawJSON: () => 'Raw details', SideBySide: () => null, TreePair: () => null, UnifiedDiff: () => null }));
const snapshot = (ref?: string) => ref ? { resourceLogs: [{ scopeLogs: [{ logRecords: Array.from({ length: ref === 'before' ? 2 : 1 }, (_, i) => ({ body: { stringValue: `Message ${i}` } })) }] }] } : undefined;
vi.mock('./useSnapshot', () => ({
  useSnapshot: (_run: string, ref: string) => ({ data: snapshot(ref), isLoading: false }),
  useSnapshots: (_run: string, refs: string[]) => ({ data: refs.map(snapshot), isLoading: false, error: null }),
}));
let host: HTMLDivElement, root: Root;
beforeEach(() => {
  (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
});
afterEach(() => { act(() => root.unmount()); host.remove(); });
const node: LayoutNode = { key: 'logs|processor/attributes', component: { id: 'attributes', ref: 'processor/attributes', kind: 'processor', type: 'attributes', mode: 'real' }, pipeline: 'logs', pipelines: ['logs'], layer: 0, x: 0, y: 0 };
const events: Event[] = [1, 2].map((seq) => ({ seq, id: String(seq), run_id: 'run', kind: seq === 1 ? 'stage.input' : 'stage.output', component: 'attributes', pipeline: 'logs', source: 'processor_wrapper', time: '', snapshot: seq === 1 ? 'before' : 'after', input_seq: seq === 2 ? 1 : undefined }));
const stage: Stage = { component: 'attributes', kind: 'processor', type: 'attributes', pipeline: 'logs', emission: 'synchronous', signal: 'logs', inputs: [{ seq: 1, outcome: 'emitted', outputs: [2] }] };
function render(search = { view: 'explore' }, stages = [stage], setSearch = vi.fn()) { act(() => root.render(createElement(TelemetryInspector, { runId: 'run', node, events, stages, live: false, search, setSearch }))); return setSearch; }
describe('focused component data views', () => {
  it('lists the input and output records when there is no comparison, claiming nothing about changes', () => {
    render();
    expect(host.querySelector('[aria-label="Input records"]')!.textContent).toContain('Message 1');
    expect(host.querySelector('[aria-label="Output records"]')!.textContent).toContain('Message 0');
    expect(host.querySelectorAll('select, input, [role=tab]')).toHaveLength(0);
    expect(host.querySelector('.rv-pill')).toBeNull();
  });
  it('does not present a buffered input as dropped or fabricate an output', () => {
    render({ view: 'explore' }, [{ ...stage, inputs: [{ seq: 1, outcome: 'deferred' }] }]);
    expect(host.textContent).toContain('processor buffered this input');
    expect(host.querySelector('[aria-label="Input records"]')!.textContent).toContain('Message 1');
    expect(host.querySelector('[aria-label="Output records"]')!.textContent).toContain('processor buffered this input');
    expect(host.querySelector('.rv-pill-drop')).toBeNull();
  });
  it('keeps Changes focused on differences rather than duplicating the data browser', () => {
    render({ view: 'changes' });
    expect(host.querySelector('.signal-explorer')).toBeNull();
    expect(host.querySelector('.technical-tools')).toBeNull();
    expect(host.textContent).toContain('No before/after comparison is available');
    expect(host.querySelectorAll('select, input')).toHaveLength(0);
  });
  it('shows the actual attribute change, with its path and level, on the first record without a click', () => {
    render({ view: 'changes' }, [{ ...stage, inputs: [{ ...stage.inputs![0], diff: { identity: 'content', before: { items: 2, resources: 1, scopes: 1 }, after: { items: 1, resources: 1, scopes: 1 }, changes: [{ entity: 'resource[0]', path: 'attributes["environment"]', op: 'added', after: 'demo' }] } }] }]);
    const row = host.querySelector('.rv-field-added')!;
    expect(row.querySelector('.rv-path')!.textContent).toBe('resource.attributes["environment"]');
    expect(row.querySelector('[aria-label="Not present before"]')).not.toBeNull();
    expect(row.querySelector('.rv-after')!.textContent).toBe('"demo"');
    expect(host.querySelector('.rv-group-head b')!.textContent).toBe('Resource');
  });
  it('follows input and output records by their exact event, even when their positional IDs coincide', () => {
    const entity = 'resource[0].scope[0].log_record[0]';
    const lineage: RecordLineage[] = [1, 2].map((seq) => ({ id: `journey-${seq}`, name: 'Message', signal: 'logs',
      hops: [{ seq, kind: seq === 1 ? 'stage.input' : 'stage.output', parent: -1, ref: entity }] }));
    const setSearch = vi.fn();
    act(() => root.render(createElement(TelemetryInspector, { runId: 'run', node, events, live: false, search: { view: 'changes' }, setSearch, lineage,
      stages: [{ ...stage, inputs: [{ ...stage.inputs![0], diff: { identity: 'content', before: { items: 2, resources: 1, scopes: 1 }, after: { items: 1, resources: 1, scopes: 1 },
        removed: [{ ref: entity, name: 'Input' }], added: [{ ref: entity, name: 'Output' }] } }] }] })));
    act(() => host.querySelector<HTMLButtonElement>('.rv-panel .rv-follow')!.click());
    expect(setSearch).toHaveBeenLastCalledWith({ trace: 'journey-1', tab: 'journey', view: undefined });
    act(() => [...host.querySelectorAll<HTMLButtonElement>('.rv-row')].find((b) => b.textContent?.includes('Output') || b.querySelector('.rv-pill-keep'))!.click());
    act(() => host.querySelector<HTMLButtonElement>('.rv-panel .rv-follow')!.click());
    expect(setSearch).toHaveBeenLastCalledWith({ trace: 'journey-2', tab: 'journey', view: undefined });
  });
  it.each(['receiver', 'exporter'])('restores following and clearing journey focus in readable %s data', (kind) => {
    const entity = 'resource[0].scope[0].log_record[0]', setSearch = vi.fn();
    const payloadNode: LayoutNode = { ...node, key: `${kind}/otlp`, component: { ...node.component, id: 'otlp', ref: `${kind}/otlp`, type: 'otlp', kind } };
    const event: Event = { ...events[0], seq: 42, component: 'otlp', source: 'runtime', kind: kind === 'receiver' ? 'input.injected' : 'exporter.captured' };
    const lineage: RecordLineage[] = [{ id: 'payload-journey', name: 'Message', signal: 'logs', hops: [{ seq: 42, kind: event.kind, parent: -1, ref: entity }] }];
    act(() => root.render(createElement(TelemetryInspector, { runId: 'run', node: payloadNode, events: [event], live: false, search: { record: entity }, setSearch, lineage })));
    expect(host.querySelectorAll('.signal-row')).toHaveLength(1);
    const clear = [...host.querySelectorAll('button')].find((b) => b.textContent === 'Show all records')!;
    act(() => clear.click()); expect(setSearch).toHaveBeenLastCalledWith({ record: undefined });
    act(() => host.querySelector<HTMLButtonElement>('.signal-expand')!.click());
    const follow = [...host.querySelectorAll('button')].find((b) => b.textContent === 'Follow record')!;
    act(() => follow.click());
    expect(setSearch).toHaveBeenLastCalledWith({ trace: 'payload-journey', tab: 'journey', view: undefined });
    expect(host.textContent).not.toContain('Select record in inspector');
  });
  it('keeps the raw JSON diff available in the transform statements view', () => {
    render({ view: 'ottl' });
    expect(host.querySelector('.raw-json-diff > summary')!.textContent).toBe('Raw JSON diff');
  });
  it('compares a buffered input through the reconciliation of everything emitted later', () => {
    const entity = 'resource[0].scope[0].log_record[1]';
    const lineage: RecordLineage[] = [{ id: 'dropped', name: 'Message', signal: 'logs', hops: [{ seq: 1, kind: 'stage.input', parent: -1, ref: entity }] }];
    const setSearch = vi.fn();
    act(() => root.render(createElement(TelemetryInspector, { runId: 'run', node, events, live: false, search: { view: 'changes' }, setSearch, lineage,
      stages: [{ ...stage, emission: 'asynchronous', inputs: [{ seq: 1, outcome: 'deferred' }], emissions: [2],
        reconciliation: { identity: 'content', before: { items: 2, resources: 1, scopes: 1 }, after: { items: 1, resources: 1, scopes: 1 }, removed: [{ ref: entity, name: 'Message 1' }] } }] })));
    expect(host.textContent).toContain('attributes buffers what it receives');
    expect(host.textContent).toContain('all 1 later emission(s)');
    expect(host.textContent).not.toContain('No before/after comparison is available');
    act(() => [...host.querySelectorAll<HTMLButtonElement>('.rv-row')].find((b) => b.querySelector('.rv-pill-drop'))!.click());
    act(() => host.querySelector<HTMLButtonElement>('.rv-panel .rv-follow')!.click());
    expect(setSearch).toHaveBeenLastCalledWith({ trace: 'dropped', tab: 'journey', view: undefined });
  });
  it('points a tail sampling processor from its value diff to the reasons for its decisions', () => {
    const setSearch = vi.fn();
    act(() => root.render(createElement(TelemetryInspector, { runId: 'run', node, events, live: false, search: { view: 'changes' }, setSearch,
      stages: [{ ...stage, type: 'tail_sampling', emission: 'asynchronous', inputs: [{ seq: 1, outcome: 'deferred' }], emissions: [2],
        reconciliation: { identity: 'content', before: { items: 2, resources: 1, scopes: 1 }, after: { items: 1, resources: 1, scopes: 1 } } }] })));
    expect(host.textContent).toContain('keeps or drops whole traces by its policies');
    act(() => [...host.querySelectorAll<HTMLButtonElement>('button')].find((b) => b.textContent?.includes('see why'))!.click());
    expect(setSearch).toHaveBeenLastCalledWith({ tab: 'decisions', view: undefined, record: undefined });
  });
  it('compares connector input with the explicitly selected outgoing route, never with a fabricated output', () => {
    const connectorNode: LayoutNode = { ...node, key: 'connector/routing', component: { ...node.component, id: 'routing', ref: 'connector/routing', kind: 'connector', type: 'routing' } };
    const connectorEvents: Event[] = events.map((e) => ({ ...e, component: 'routing', source: 'connector_wrapper' }));
    const connectorStage: Stage = { ...stage, component: 'routing', kind: 'connector', type: 'routing', inputs: [{ seq: 1, outcome: 'emitted', routes: [{ seq: 2, pipeline: 'logs/output', items: 1 }] }] };
    act(() => root.render(createElement(TelemetryInspector, { runId: 'run', node: connectorNode, events: connectorEvents, stages: [connectorStage], live: false, search: {}, setSearch: vi.fn() })));
    const details = host.querySelector<HTMLDetailsElement>('.raw-json-diff')!;
    act(() => { details.open = true; details.dispatchEvent(new Event('toggle')); });
    expect(host.textContent).toContain('Select an outgoing recorded payload');
    expect(host.querySelector('.raw-json-labels')).toBeNull();
    act(() => { const select = host.querySelector<HTMLSelectElement>('[aria-label="Connector payload"]')!; select.value = '2'; select.dispatchEvent(new Event('change', { bubbles: true })); });
    expect(host.querySelector('.raw-json-labels')).not.toBeNull();
    expect(host.textContent).toContain('Complete recorded snapshots');
  });
});
