import { describe, expect, it } from 'vitest';
import type { Batching } from '../api/types';
import { metadataBatching, sampleBatching } from '../inspect/batching.fixture';
import { batchingChanges, batchingHeadline, type BatchingRow } from './batchingCompare';

const pick = (rows: BatchingRow[], label: string) => rows.find((r) => r.label === label);

describe('batchingChanges', () => {
  it('puts the settings and the batches of one processor side by side', () => {
    const [c] = batchingChanges([sampleBatching], [metadataBatching]);
    expect(c.only).toBeUndefined();
    expect(pick(c.settings, 'send_batch_size')).toEqual({ label: 'send_batch_size', before: '4', after: '4', changed: false });
    expect(pick(c.settings, 'send_batch_max_size')).toEqual({ label: 'send_batch_max_size', before: '6', after: 'no limit', changed: true });
    expect(pick(c.settings, 'metadata_keys')).toEqual({ label: 'metadata_keys', before: 'none', after: 'tenant', changed: true });
    expect(pick(c.settings, 'metadata_cardinality_limit')?.changed).toBe(false);
    expect(pick(c.outcome, 'Batches out')).toMatchObject({ before: '4', after: '3', changed: true });
    expect(pick(c.outcome, 'Log records per batch')).toMatchObject({ before: '1–6', after: '1–4' });
    expect(pick(c.outcome, 'Why batches left')).toMatchObject({ before: '2 size reached, 1 timer, 1 shutdown', after: '1 size reached, 2 timer' });
    expect(pick(c.outcome, 'Longest added wait')).toMatchObject({ before: '99.8ms', after: '300.4ms' });
    expect(pick(c.outcome, 'Split batches')).toMatchObject({ before: '1', after: '0' });
    expect(c.batches.after.map((b) => b.batcher)).toEqual(['tenant=acme', 'no tenant', 'tenant=globex']);
    expect(c.batches.before.map((b) => [b.items, b.trigger])).toEqual([[5, 'size'], [1, 'timeout'], [6, 'size'], [3, 'shutdown']]);
  });

  it('treats one timeout spelled two ways as unchanged', () => {
    const later: Batching = { ...sampleBatching, settings: { ...sampleBatching.settings, timeout: '0.3s' } };
    const [c] = batchingChanges([sampleBatching], [later]);
    expect(pick(c.settings, 'timeout')?.changed).toBe(false);
    expect(batchingHeadline(c)).toBe('Same settings and the same batches.');
  });

  it('says which settings changed and what that did to the batches', () => {
    const later: Batching = { ...sampleBatching, settings: { ...sampleBatching.settings, timeout: '1s' }, flushes: sampleBatching.flushes!.slice(0, 2) };
    expect(batchingHeadline(batchingChanges([sampleBatching], [later])[0])).toBe('timeout 300ms → 1s: 4 → 2 batches.');
  });

  it('keeps a processor that batched in one run only', () => {
    const other: Batching = { ...sampleBatching, pipeline: 'logs/2' };
    const changes = batchingChanges([sampleBatching], [other]);
    expect(changes.map((c) => [c.pipeline, c.only])).toEqual([['logs', 'before'], ['logs/2', 'after']]);
    expect(pick(changes[0].outcome, 'Batches out')).toMatchObject({ before: '4', after: '—' });
    expect(batchingHeadline(changes[1])).toBe('Batches only in the candidate run.');
  });

  it('has nothing to compare without batch processors', () => {
    expect(batchingChanges(undefined, [])).toEqual([]);
  });
});
