import { describe, expect, it } from 'vitest';
import { componentSelection, componentTabs, INVESTIGATE_TABS, inspectorSelection, PREPARE_TABS, RUN_TABS } from './inspectorTabs';

describe('inspector information architecture', () => {
  const processor = { kind: 'processor', type: 'attributes' };
  it('separates whole-run views from component views', () => {
    expect(inspectorSelection({ tab: 'journey' }, processor).scope).toBe('run');
    expect(inspectorSelection({ tab: 'events' }, processor).tabs).toEqual(RUN_TABS);
    expect(inspectorSelection({}, processor).tab.id).toBe('how');
    expect(inspectorSelection({}, processor).tabs).toEqual(PREPARE_TABS);
    expect(inspectorSelection({ run: 'r' }, processor).tab.id).toBe('changes');
    expect(inspectorSelection({ tab: 'changes' }, processor).scope).toBe('component');
    expect(inspectorSelection({ tab: 'changes' }).tab.id).toBe('run');
  });
  it('offers only meaningful tabs for each component kind', () => {
    const ids = (kind: string, type = 'otlp') => componentTabs({ kind, type }).map((t) => t.id);
    expect(ids('receiver')).toEqual(['telemetry', 'component']);
    expect(ids('exporter')).toEqual(['telemetry', 'component']);
    expect(ids('processor', 'attributes')).toEqual(['changes', 'component']);
    expect(ids('processor', 'transform')).toEqual(['changes', 'ottl', 'component']);
    expect(ids('processor', 'tail_sampling')).toEqual(['decisions', 'changes', 'component']);
    expect(ids('processor', 'groupbyattrs')).toEqual(['grouping', 'changes', 'component']);
    expect(ids('processor', 'filter')).toEqual(['filter', 'changes', 'component']);
    expect(ids('connector', 'count')).toEqual(['routes', 'telemetry', 'component']);
    expect(ids('connector', 'routing')).toEqual(['routes', 'telemetry', 'component']);
    expect(ids('extension', 'livecapture')).toEqual(['activity', 'telemetry', 'component']);
    expect(componentTabs({ kind: 'connector', type: 'count' })[0].label).toBe('Aggregation');
    expect(componentTabs({ kind: 'connector', type: 'routing' })[0].label).toBe('Routing');
  });
  it('preserves legacy bookmarks and falls back when a tab does not fit the node', () => {
    expect(inspectorSelection({ run: 'r', tab: 'telemetry', view: 'changes' }, processor).tab.id).toBe('changes');
    expect(inspectorSelection({ run: 'r', tab: 'telemetry', view: 'ottl' }, { kind: 'processor', type: 'transform' }).tab.id).toBe('ottl');
    expect(inspectorSelection({ run: 'r', tab: 'telemetry', view: 'json' }, processor).tab.id).toBe('changes');
    expect(inspectorSelection({ run: 'r', tab: 'changes' }, { kind: 'exporter', type: 'otlp' }).tab.id).toBe('telemetry');
    expect(inspectorSelection({ run: 'r', tab: 'nonsense' }, processor).tab.id).toBe('changes');
  });
  it('names the left pane views and the views of a component before any run', () => {
    expect(INVESTIGATE_TABS.map((t) => t.label)).toEqual(['Overview', 'Assertions', 'Records', 'Timeline', 'Recommendations']);
    expect(PREPARE_TABS.map((t) => t.label)).toEqual(['How it will run', 'Effective configuration']);
    expect(componentTabs({ kind: 'exporter', type: 'otlp' }).map((t) => t.label)).toEqual(['Data', 'Configuration']);
  });
  it('shows a component view whatever whole-run tab the URL names', () => {
    expect(componentSelection({ run: 'r', tab: 'journey' }, processor).tab.id).toBe('changes');
    expect(componentSelection({ run: 'r', tab: 'run' }, { kind: 'connector', type: 'routing' }).tab.id).toBe('routes');
    expect(componentSelection({ run: 'r', tab: 'ottl' }, { kind: 'processor', type: 'transform' }).tab.id).toBe('ottl');
    expect(componentSelection({ tab: 'component' }, processor).tab.id).toBe('component');
    expect(componentSelection({ tab: 'changes' }, processor).tab.id).toBe('how');
  });
  it('gives every primary view a distinct purpose and excludes technical formats', () => {
    const tabs = [...RUN_TABS, ...PREPARE_TABS, ...componentTabs({ kind: 'processor', type: 'transform' }), ...componentTabs({ kind: 'connector', type: 'routing' })];
    expect(tabs.every((t) => t.purpose.length > 40)).toBe(true);
    expect(tabs.some((t) => ['json', 'tree', 'side', 'table', 'unified'].includes(t.id))).toBe(false);
  });
});
