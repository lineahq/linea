import { describe, expect, it } from 'vitest';
import type { Collector } from '../api/types';
import { availabilityChip, collectorStatus, connectionChip, countBadge, facetValues, filterCollectors, isCapturable, locationText, noFilter, selectionTarget, setupState, versionText } from './collectorView';

const make = (patch: Partial<Collector>): Collector => ({
  id: 'id', generation: 1, credential_id: 'cred', pod: 'gw-0', cluster: 'prod-eu-1', namespace: 'payments', collector_group: 'payments-gw', collector_version: '0.162.0', distribution_version: '',
  enabled: true, point_count: 2, raw_point_count: 0, filter_limits: { max_conditions: 16, max_condition_chars: 256, max_regex_chars: 256 }, ...patch,
});

describe('setupState', () => {
  it.each([
    ['nothing loaded yet', undefined, 'loading'],
    ['no fleet connection', { enabled: false, items: [] }, 'not-configured'],
    ['a Collector reports', { enabled: true, items: [{}] }, 'ready'],
    ['connected with no Collectors', { enabled: true, items: [] }, 'waiting'],
    ['more pages exist even if this one is empty', { enabled: true, items: [], next_cursor: 'x' }, 'ready'],
  ] as const)('%s', (_name, page, state) => {
    expect(setupState(page)).toBe(state);
  });
});

describe('collector status and chips', () => {
  it.each([
    [{ enabled: true }, 'connected', 'Connected', 'pass', 'Available'],
    [{ enabled: false }, 'capture-disabled', 'Capture disabled', 'plain', 'Capture not allowed'],
  ])('%j', (c, status, label, tone, availability) => {
    expect(collectorStatus(c)).toBe(status);
    expect(connectionChip(c)).toMatchObject({ label, tone });
    expect(availabilityChip(c).label).toBe(availability);
    expect(isCapturable(c)).toBe(status === 'connected');
  });
});

describe('filterCollectors and facets', () => {
  const items = [
    make({ id: '1', pod: 'payments-gw-0' }),
    make({ id: '2', pod: 'edge-agent-7f2', namespace: 'kube-system', collector_group: 'edge-agents', enabled: false }),
    make({ id: '3', pod: 'checkout-sidecar-a', cluster: 'staging', collector_group: '', collector_version: '0.134.0', agent_id: 'agent-3' }),
  ];
  const ids = (filter: Partial<typeof noFilter>) => filterCollectors(items, { ...noFilter, ...filter }).map((c) => c.id);
  it('passes everything with no filter', () => expect(ids({})).toEqual(['1', '2', '3']));
  it('searches name, cluster, namespace, group and version', () => {
    expect(ids({ search: 'EDGE' })).toEqual(['2']);
    expect(ids({ search: 'staging' })).toEqual(['3']);
    expect(ids({ search: 'kube-system' })).toEqual(['2']);
    expect(ids({ search: '0.134' })).toEqual(['3']);
    expect(ids({ search: 'payments' })).toEqual(['1', '3']);
  });
  it('combines the facets', () => {
    expect(ids({ cluster: 'prod-eu-1' })).toEqual(['1', '2']);
    expect(ids({ group: 'edge-agents' })).toEqual(['2']);
    expect(ids({ status: 'capture-disabled' })).toEqual(['2']);
    expect(ids({ search: 'agent-3' })).toEqual(['3']);
    expect(ids({ cluster: 'prod-eu-1', status: 'capture-disabled' })).toEqual(['2']);
    expect(ids({ search: 'zzz' })).toEqual([]);
  });
  it('lists distinct facet values without blanks', () => {
    expect(facetValues(items, 'cluster')).toEqual(['prod-eu-1', 'staging']);
    expect(facetValues(items, 'collector_group')).toEqual(['edge-agents', 'payments-gw']);
  });
});

describe('small text helpers', () => {
  it('marks counts that may continue on another page', () => {
    expect(countBadge(14, false)).toBe('14');
    expect(countBadge(25, true)).toBe('25+');
    expect(countBadge(undefined, false)).toBeUndefined();
  });
  it('writes location and version compactly', () => {
    expect(locationText({ cluster: 'a', namespace: 'b' })).toBe('a / b');
    expect(locationText({})).toBe('Local');
    expect(versionText({ collector_version: '0.136.0', distribution_version: '' })).toBe('0.136.0');
    expect(versionText({ collector_version: '0.136.0', distribution_version: '0.136.0' })).toBe('0.136.0');
    expect(versionText({ collector_version: '0.136.0', distribution_version: 'dev' })).toBe('0.136.0 · dev');
    expect(versionText({})).toBe('Unknown');
  });
  it('carries at most 16 distinct Collectors to New capture', () => {
    expect(selectionTarget([])).toBeUndefined();
    expect(selectionTarget(['a', 'b', 'a'])).toEqual({ collectors: ['a', 'b'] });
    expect(selectionTarget(Array.from({ length: 20 }, (_, i) => `c${i}`))?.collectors).toHaveLength(16);
  });
});
