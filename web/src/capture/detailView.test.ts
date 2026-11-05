import { describe, expect, it } from 'vitest';
import type { CaptureSession } from '../api/types';
import { anyLive, canStop, captureTitle, collectorsText, dataNotice, dataStateText, deleteBlocker, filterLines, isReceiving, metricsView, pointLines, receivedDetail, sessionsNewestFirst, startedBy, stopAfterText, storageNote, targetLabel } from './detailView';

const data = { enabled: true, state: 'open', contains_raw: false, memory_only: false, received_records: 1204, received_bytes: 3_250_585, rejected_records: 0, record_limit: 10000, byte_limit: 8 * 1024 * 1024 };
const stats = { selected: 0, enqueued_records: 612, enqueued_bytes: 0, filter_errors: 0 };
const session = (patch: Partial<CaptureSession> = {}): CaptureSession => ({
  id: 'capture-0123456789ab', project_id: 'default',
  state: 'active',
  terminal: false,
  created_at: '2026-10-23T12:00:00Z',
  expires_at: '2026-10-23T12:02:00Z',
  caller: { principal: 'local-owner', client: 'http' },
  data,
  requested_limits: { bytes_per_second: 1, records_per_second: 1, total_bytes: 50 * 1024 ** 2, total_records: 5000, record_bytes: 1 },
  selection: { collector_ids: ['c1'], capture_points: [{ pipeline: 'logs', point: 'egress' }], signals: ['logs'], duration_seconds: 120, filter: { logs: { log: ['severity_number >= 17'] } }, limits: { bytes_per_second: 1, records_per_second: 1, total_bytes: 1, total_records: 1, record_bytes: 1 } },
  targets: [
    { collector_id: 'c1', generation: 1, state: 'active', terminal: false, stats, source: { pod: 'payments-gw-0', collector_group: 'g', collector_version: '0.162.0' } },
    { collector_id: 'c2', generation: 1, state: 'active', terminal: false, stats: { ...stats, enqueued_records: 598 } },
  ],
  ...patch,
});

describe('titles and labels', () => {
  it('uses the name, else the short ID', () => {
    expect(captureTitle({ id: 'capture-0123456789ab' })).toBe('456789ab');
    expect(captureTitle({ id: 'x', metadata: { name: '  Checkout errors ' } })).toBe('Checkout errors');
    expect(captureTitle({ id: 'abcdefghijkl', metadata: { name: '   ' } })).toBe('efghijkl');
  });
  it('labels targets by pod, else ID', () => {
    expect(targetLabel({ collector_id: 'c1', source: { pod: 'gw-0' } })).toBe('gw-0');
    expect(targetLabel({ collector_id: 'c1' })).toBe('c1');
  });
  it('words who started it', () => {
    expect(startedBy({ principal: 'local-owner', client: 'http' })).toBe('you, from the web client or API');
    expect(startedBy({ principal: 'local-owner', client: 'cli' })).toBe('you, from the CLI');
    expect(startedBy({ principal: 'ci', client: 'other' })).toBe('ci, from other');
  });
  it('counts Collectors', () => {
    expect([1, 3].map(collectorsText)).toEqual(['1 Collector', '3 Collectors']);
  });
});

describe('metricsView', () => {
  it('sums what Collectors reported and reads what was received', () => {
    const m = metricsView(session(), Date.parse('2026-10-23T12:01:12Z'));
    expect(m.reported).toBe(1210);
    expect(m.received).toBe(1204);
    expect(m.elapsed).toMatchObject({ text: '1:12', percent: 60 });
    expect(m.storage).toEqual({ headline: 'Server storage', detail: 'kept until deleted' });
    expect(receivedDetail(m)).toBe('3.1 MiB');
  });
  it('mentions rejected records and omits storage when ingress is off', () => {
    const m = metricsView(session({ data: { ...data, enabled: false, rejected_records: 3 } }), Date.parse('2026-10-23T12:01:12Z'));
    expect(receivedDetail(m)).toContain('3 rejected');
    expect(m.storage.headline).toBe('—');
  });
});

describe('provenance lines', () => {
  it('lists only the filters that are set', () => {
    expect(filterLines(session().selection.filter)).toEqual([{ label: 'Logs', conditions: ['severity_number >= 17'] }]);
    expect(filterLines({ resource: ['r'], metrics: { datapoint: ['d'] } })).toEqual([{ label: 'Resource', conditions: ['r'] }, { label: 'Data points', conditions: ['d'] }]);
    expect(filterLines(undefined)).toEqual([]);
  });
  it('states the bounds a capture stops at', () => {
    const s = session();
    expect(stopAfterText(s.selection, s.requested_limits)).toBe('2m or 5,000 records or 50 MiB');
  });
  it('lists the chosen points', () => {
    expect(pointLines(session().selection)).toEqual(['logs · egress']);
  });
});

describe('actions', () => {
  it('allows one stop request at a time', () => {
    expect(canStop({ terminal: false, state: 'active' })).toBe(true);
    expect(canStop({ terminal: false, state: 'pending' })).toBe(true);
    expect(canStop({ terminal: false, state: 'stopping' })).toBe(false);
    expect(canStop({ terminal: true, state: 'completed' })).toBe(false);
  });
  it('explains why a capture cannot be deleted yet', () => {
    expect(deleteBlocker({ terminal: false, data })).toMatch(/Stop the capture/);
    expect(deleteBlocker({ terminal: true, data })).toMatch(/still receiving/);
    expect(deleteBlocker({ terminal: true, data: { ...data, state: 'draining' } })).toMatch(/still receiving/);
    expect(deleteBlocker({ terminal: true, data: { ...data, state: 'frozen' } })).toBeUndefined();
  });
});

describe('data state', () => {
  it('names notices that replace the records', () => {
    expect(dataNotice({ ...data, enabled: false })).toMatch(/unavailable/);
    expect(dataNotice({ ...data, error: 'disk full' })).toContain('disk full');
    expect(dataNotice({ ...data, state: 'lost_on_restart' })).toMatch(/discarded/);
    expect(dataNotice(data)).toBeUndefined();
  });
  it('describes storage and state', () => {
    expect(storageNote(data)).toBe('Accepted payloads are committed to the server storage and kept until the capture is deleted. Captures are filtered and sampled subsets, redacted unless taken at a raw point; retries may duplicate records. Limit: 10,000 records / 8 MiB.');
    expect(storageNote({ ...data, memory_only: true })).toContain('held in memory');
    expect(['open', 'frozen', 'lost_on_restart'].map(dataStateText)).toEqual(['Receiving', 'Complete', 'lost on restart']);
    expect([isReceiving({ state: 'open' }), isReceiving({ state: 'draining' }), isReceiving({ state: 'frozen' })]).toEqual([true, true, false]);
  });
});

describe('lists', () => {
  it('orders newest first without mutating the input', () => {
    const items = [{ created_at: '2026-10-23T10:00:00Z' }, { created_at: '2026-10-23T12:00:00Z' }];
    expect(sessionsNewestFirst(items).map((i) => i.created_at)).toEqual(['2026-10-23T12:00:00Z', '2026-10-23T10:00:00Z']);
    expect(items[0].created_at).toBe('2026-10-23T10:00:00Z');
    expect(sessionsNewestFirst(undefined)).toEqual([]);
  });
  it('knows when any capture is still live', () => {
    expect(anyLive([{ terminal: true }, { terminal: false }])).toBe(true);
    expect(anyLive([{ terminal: true }])).toBe(false);
    expect(anyLive(undefined)).toBe(false);
  });
});
