import { describe, expect, it } from 'vitest';
import type { Batching } from '../api/types';
import { batcherRows, batchRows, metadataNote, settingNames, settingsText, timerNote, verificationText } from './batching';
import { metadataBatching, sampleBatching } from './batching.fixture';
import { batchingChanges } from '../runs/batchingCompare';

const qbSettings = { type: 'queue_batch', send_batch_size: 4, send_batch_max_size: 6, timeout: '300ms', sizer: 'items' };
const qb: Batching = { ...sampleBatching, processor: 'queue_batch', settings: { ...qbSettings } };
const partitioned: Batching = {
  ...metadataBatching, processor: 'queue_batch',
  settings: { ...qbSettings, send_batch_max_size: 0, metadata_keys: ['tenant'], cache_size: 2 },
  batchers: metadataBatching.batchers!.map((x, i) => (i === 0 ? { ...x, evicted: true } : x)),
};

describe('queue_batch', () => {
  it('names the settings of its batch section', () => {
    expect(settingNames(qb.settings).size).toBe('batch::min_size');
    expect(settingsText(qb)).toBe('batch::min_size 4 · batch::flush_timeout 300ms · batch::max_size 6');
    expect(settingsText({ ...qb, settings: { ...qb.settings, sizer: 'bytes' } })).toBe('batch::min_size 4 bytes · batch::flush_timeout 300ms · batch::max_size 6 bytes');
    expect(settingsText({ ...qb, settings: { type: 'queue_batch', send_batch_size: 0, send_batch_max_size: 0, timeout: '0s', unbatched: true } }))
      .toBe('no batch section: every request is queued and sent as it is');
  });
  it('explains sends in its own terms', () => {
    const why = batchRows(qb).map((r) => r.why).join('\n');
    expect(why).toContain('at least batch::min_size 4');
    expect(why).not.toContain('send_batch');
    expect(verificationText({ ...qb, verification: 'metrics_agree' }).text).toBe('The queue_batch processor’s own metrics count the same batches.');
  });
  it('marks evicted partitions and names cache_size', () => {
    expect(batcherRows(partitioned)[0].why).toMatch(/; removed beyond batch::partition::cache_size$/);
    expect(metadataNote(partitioned)).toContain('Beyond batch::partition::cache_size 2 partitions');
    const evicted = batchRows({ ...partitioned, flushes: [{ ...partitioned.flushes![0], trigger: 'evicted' }] });
    expect(evicted[0].trigger).toBe('evicted');
    expect(evicted[0].why).toContain('went past batch::partition::cache_size 2');
  });
  it('says its timer restarts when a batch starts', () => {
    const short: Batching = { ...qb, flushes: [{ ...qb.flushes![0], trigger: 'timeout', wait_ms: { min: 10, max: 10 } }] };
    expect(timerNote(short, batchRows(short)[0])).toContain('when a request reaches an empty batcher');
  });
  it('compares its settings by their names', () => {
    const [change] = batchingChanges([qb], [{ ...qb, settings: { ...qb.settings, send_batch_size: 8 } }]);
    const labels = change.settings.map((r) => r.label);
    expect(labels).toEqual(['batch::min_size', 'batch::flush_timeout', 'batch::max_size', 'batch::partition::metadata_keys', 'batch::sizer']);
    expect(change.settings[0].changed).toBe(true);
  });
});
