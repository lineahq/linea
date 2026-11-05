import { describe, expect, it } from 'vitest';
import { capturePhase, captureLifecycle, elapsedView, formatClock, reasonText, sessionCounts, targetChip, type PhaseInput } from './phase';

const base: PhaseInput = { state: 'active', terminal: false, target_count: 3, active_count: 3, uncertain_count: 0, failed_count: 0 };

describe('capturePhase', () => {
  const cases: [string, Partial<PhaseInput>, string, string][] = [
    ['waiting for the Collectors', { state: 'starting', active_count: 0 }, 'starting', 'Starting'],
    ['receiving', {}, 'receiving', 'Receiving'],
    ['stop requested', { state: 'stopping' }, 'stopping', 'Stopping'],
    ['stopped', { state: 'stopped', terminal: true, active_count: 0 }, 'closed', 'Closed'],
    ['completed', { state: 'completed', terminal: true, active_count: 0 }, 'closed', 'Closed'],
    ['window ended', { state: 'expired', terminal: true, active_count: 0 }, 'closed', 'Closed'],
    ['failed', { state: 'failed', terminal: true }, 'failed', 'Failed'],
    ['interrupted by fleet', { state: 'interrupted', terminal: true }, 'partial', 'Closed · interrupted'],
    ['lost by the debugger', { state: 'uncertain', terminal: true, uncertain_count: 3 }, 'partial', 'Closed · uncertain'],
    ['completed with a failed target', { state: 'completed', terminal: true, failed_count: 1 }, 'partial', 'Closed · incomplete'],
    ['an unknown state keeps its text', { state: 'mystery' }, 'unknown', 'mystery'],
  ];
  it.each(cases)('%s', (_name, patch, id, label) => {
    const phase = capturePhase({ ...base, ...patch });
    expect(phase.id).toBe(id);
    expect(phase.label).toBe(label);
  });

  it('never claims Collectors are still sending while stopping', () => {
    const phase = capturePhase({ ...base, state: 'stopping' });
    expect(phase.headline).toBe('Stop requested · waiting for the Collectors to release the capture');
    expect(phase.headline).not.toMatch(/sending/);
  });

  it('marks only non-terminal phases as live', () => {
    expect(capturePhase(base).live).toBe(true);
    expect(capturePhase({ ...base, state: 'stopping' }).live).toBe(true);
    expect(capturePhase({ ...base, state: 'completed', terminal: true }).live).toBe(false);
  });

  it('explains an incomplete close', () => {
    const phase = capturePhase({ ...base, state: 'stopped', terminal: true, uncertain_count: 2 });
    expect(phase.headline).toBe('Closed · 2 Collectors did not finish cleanly');
  });

  it('gives the reason of a failure or a lost capture', () => {
    expect(capturePhase({ ...base, state: 'failed', terminal: true, reason: 'collector_rejected' }).detail).toBe('A Collector rejected the capture.');
    const lost = capturePhase({ ...base, state: 'uncertain', terminal: true, reason: 'platform_restarted; capture_not_resumed' });
    expect(lost.detail).toBe('Platform restarted. Capture not resumed. Its records may be incomplete. Fleet still ends the capture at its expiry.');
  });
});

describe('reasonText', () => {
  it('describes known codes and spells out unknown ones', () => {
    expect(reasonText('absolute_expiry')).toBe('The capture window ended.');
    expect(reasonText('new_reason')).toBe('New reason.');
    expect(reasonText('')).toBeUndefined();
  });
});

describe('captureLifecycle', () => {
  const times = { created_at: '2026-10-23T14:30:02Z', expires_at: '2026-10-23T14:32:02Z', received_records: 1204 };
  const statuses = (input: PhaseInput) => captureLifecycle({ ...input, ...times }).map((s) => `${s.id}:${s.status}`);

  it('is receiving while records arrive', () => {
    expect(statuses(base)).toEqual(['requested:done', 'accepted:done', 'receiving:current', 'stopping:pending', 'closed:pending']);
  });
  it('waits at acceptance while starting', () => {
    expect(statuses({ ...base, state: 'starting' })).toEqual(['requested:done', 'accepted:current', 'receiving:pending', 'stopping:pending', 'closed:pending']);
  });
  it('warns at stopping until the Collectors release the capture', () => {
    const steps = captureLifecycle({ ...base, state: 'stopping', open_count: 1, ...times });
    expect(steps.map((s) => `${s.id}:${s.status}`)).toEqual(['requested:done', 'accepted:done', 'receiving:done', 'stopping:warn', 'closed:pending']);
    expect(steps[3].detail).toBe('stop requested · 1 Collector still open');
  });
  it('is done once closed and warns on an incomplete close', () => {
    expect(statuses({ ...base, state: 'completed', terminal: true })).toEqual(['requested:done', 'accepted:done', 'receiving:done', 'stopping:done', 'closed:done']);
    expect(statuses({ ...base, state: 'interrupted', terminal: true })).toEqual(['requested:done', 'accepted:done', 'receiving:done', 'stopping:warn', 'closed:warn']);
    expect(statuses({ ...base, state: 'failed', terminal: true })).toEqual(['requested:done', 'accepted:done', 'receiving:done', 'stopping:warn', 'closed:failed']);
  });
  it('names why the capture ended', () => {
    expect(captureLifecycle({ ...base, state: 'expired', terminal: true, reason: 'absolute_expiry', ...times })[3].detail).toBe('The capture window ended.');
  });
  it('reports how many targets were accepted and the received records', () => {
    const steps = captureLifecycle({ ...base, accepted_count: 2, ...times });
    expect(steps[1]).toMatchObject({ detail: '2 of 3 Collectors', status: 'warn' });
    expect(steps[2].detail).toBe('1,204 records');
    expect(steps[0].time).toBe(times.created_at);
  });
});

describe('elapsedView', () => {
  const s = { created_at: '2026-10-23T14:30:00Z', expires_at: '2026-10-23T14:32:00Z', terminal: false };
  const at = (iso: string) => Date.parse(iso);
  it('measures a live capture to now against its window', () => {
    expect(elapsedView(s, at('2026-10-23T14:31:12Z'))).toEqual({ seconds: 72, percent: 60, text: '1:12' });
  });
  it('caps a live capture at its window', () => {
    expect(elapsedView(s, at('2026-10-23T15:00:00Z'))).toMatchObject({ seconds: 120, percent: 100, text: '2:00' });
  });
  it('shows a closed capture as its whole window', () => {
    expect(elapsedView({ ...s, terminal: true }, at('2026-10-23T14:30:10Z'))).toMatchObject({ seconds: 120, percent: 100 });
  });
  it('does not go negative when the clock is behind', () => {
    expect(elapsedView(s, at('2026-10-23T14:29:00Z')).seconds).toBe(0);
  });
});

describe('formatClock', () => {
  it.each([[0, '0:00'], [5, '0:05'], [72, '1:12'], [600, '10:00'], [3725, '1:02:05'], [-4, '0:00']])('%s -> %s', (n, text) => {
    expect(formatClock(n)).toBe(text);
  });
});

describe('targetChip', () => {
  it.each([
    ['pending', 'Starting', 'plain'],
    ['active', 'Receiving', 'running'],
    ['stopping', 'Stopping', 'warn'],
    ['unknown', 'No longer reported', 'warn'],
    ['failed', 'Failed', 'fail'],
    ['expired', 'Window ended', 'pass'],
    ['completed', 'Completed', 'pass'],
  ])('%s -> %s', (state, label, tone) => {
    expect(targetChip({ state, terminal: false })).toMatchObject({ label, tone });
  });
});

describe('sessionCounts', () => {
  it('reduces targets to the counters a summary carries', () => {
    const counts = sessionCounts({
      state: 'stopping',
      terminal: false,
      stop_reason: 'stop_requested',
      created_at: 'a',
      expires_at: 'b',
      data: { received_records: 7 },
      targets: [
        { state: 'completed', terminal: true },
        { state: 'stopping', terminal: false },
        { state: 'unknown', terminal: true },
        { state: 'pending', terminal: false },
      ],
    });
    expect(counts).toMatchObject({ target_count: 4, uncertain_count: 1, failed_count: 0, active_count: 0, accepted_count: 3, open_count: 2, received_records: 7, reason: 'stop_requested' });
  });
  it('tolerates a session without targets', () => {
    expect(sessionCounts({ state: 'starting', terminal: false, created_at: '', expires_at: '', data: { received_records: 0 } }).target_count).toBe(0);
  });
});
