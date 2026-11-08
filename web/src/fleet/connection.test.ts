import { describe, expect, it } from 'vitest';
import { checkChip, checkTitle, connectionVerdict, durationText } from './connection';

const ok = (name: string) => ({ name, status: 'ok', message: 'fine', duration_ms: 3 });

describe('connection', () => {
  it('names steps and keeps unknown ones', () => {
    expect(checkTitle('live_capture')).toBe('Live capture plugin');
    expect(checkTitle('later')).toBe('later');
  });

  it('words each status', () => {
    expect(checkChip({ status: 'ok' }).tone).toBe('pass');
    expect(checkChip({ status: 'failed' }).label).toBe('Failed');
    expect(checkChip({ status: 'skipped' }).label).toBe('Not checked');
    expect(checkChip({ status: 'new' }).label).toBe('new');
  });

  it('sums up a check', () => {
    expect(connectionVerdict({ configured: false, checks: [] })).toBeUndefined();
    expect(connectionVerdict({ configured: true, checks: [ok('fleet'), ok('live_capture'), { ...ok('config_management'), status: 'off' }] })?.tone).toBe('pass');
    expect(connectionVerdict({ configured: true, checks: [ok('fleet'), { ...ok('live_capture'), status: 'failed', message: 'no plugin' }] }))
      .toEqual({ tone: 'warn', title: 'Fleet is connected; live capture is not', body: 'no plugin' });
    const refused = connectionVerdict({ configured: true, checks: [{ ...ok('fleet'), status: 'failed', message: "fleet refused the debugger's credential" }] });
    expect(refused?.tone).toBe('fail');
    expect(refused?.body).toContain('credential');
  });

  it('shows durations', () => {
    expect(durationText(42)).toBe('42 ms');
    expect(durationText(1500)).toBe('1.5 s');
  });
});
