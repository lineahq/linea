import { describe, expect, it } from 'vitest';
import type { Stage } from '../api/types';
import { concatPayloads, deferredWindow, eventRecord, resourceCount } from './deferred';

const stage: Stage = {
  component: 'tail_sampling', kind: 'processor', type: 'tail_sampling', pipeline: 'traces', emission: 'asynchronous', signal: 'traces',
  inputs: [{ seq: 4, outcome: 'deferred' }, { seq: 8, outcome: 'emitted', outputs: [9] }, { seq: 10, outcome: 'deferred' }],
  emissions: [5, 7, 11],
  reconciliation: { identity: 'span_id', before: { items: 6, resources: 2, scopes: 2 }, after: { items: 3, resources: 3, scopes: 3 } },
};

describe('deferred stage window', () => {
  it('pairs the unpaired inputs with the later emissions', () => {
    expect(deferredWindow(stage)).toEqual({ inputs: [4, 10], emissions: [5, 7, 11] });
  });
  it('has no window without a reconciliation, which only a completed run has', () => {
    expect(deferredWindow({ ...stage, reconciliation: undefined })).toBeUndefined();
    expect(deferredWindow(undefined)).toBeUndefined();
  });
  it('appends resources in order without merging, as the analysis indexes them', () => {
    const a = { resourceSpans: [{ resource: { n: 1 } }] };
    const b = { resourceSpans: [{ resource: { n: 1 } }, { resource: { n: 2 } }] };
    expect(concatPayloads([a, b])).toEqual({ resourceSpans: [{ resource: { n: 1 } }, { resource: { n: 1 } }, { resource: { n: 2 } }] });
    expect(concatPayloads([])).toEqual({});
  });
  it('waits for every payload instead of showing a partial side', () => {
    expect(concatPayloads([{ resourceLogs: [] }, undefined])).toBeUndefined();
  });
  it('resolves a merged record back to the event that recorded it', () => {
    expect(resourceCount({ resourceSpans: [{}, {}] })).toBe(2);
    expect(resourceCount(undefined)).toBe(0);
    expect(eventRecord('resource[0].scope[0].span[1]', [5, 7], [1, 2])).toEqual({ seq: 5, entity: 'resource[0].scope[0].span[1]' });
    expect(eventRecord('resource[2].scope[0].span[0]', [5, 7], [1, 2])).toEqual({ seq: 7, entity: 'resource[1].scope[0].span[0]' });
    expect(eventRecord('resource[3].scope[0].span[0]', [5, 7], [1, 2])).toBeUndefined();
    expect(eventRecord('scope[0]', [5], [1])).toBeUndefined();
  });
});
