import { describe, expect, it } from 'vitest';
import type { UpgradeCheck } from '../../api/types';
import {
  blocksLeaving, checkReducer, initialCheck, isAbort, leaveQuestion, type CheckState, type CheckSubject,
} from './upgradeMachine';

const subject: CheckSubject = { configLabel: 'checkout-logs.yaml', scenarios: 3, from: '', to: 'v0163', startedAt: 1000 };
const check = { verdict: 'compatible' } as UpgradeCheck;
const failure = { message: 'refused', details: [] };

const running = checkReducer(initialCheck, { type: 'start', subject });

describe('checkReducer', () => {
  it('starts a check with the next id', () => {
    expect(running).toEqual({ phase: 'running', id: 1, subject });
  });

  it('does not start a second check while one runs', () => {
    expect(checkReducer(running, { type: 'start', subject: { ...subject, to: 'other' } })).toBe(running);
  });

  it('finishes the running check and measures how long it took', () => {
    expect(checkReducer(running, { type: 'finish', id: 1, check, now: 3500 })).toEqual({
      phase: 'done', id: 1, subject, check, elapsedMs: 2500,
    });
  });

  it('records a failure of the running check', () => {
    expect(checkReducer(running, { type: 'fail', id: 1, failure })).toEqual({ phase: 'failed', id: 1, subject, failure });
  });

  it('cancels the running check and then ignores its late answer', () => {
    const cancelled = checkReducer(running, { type: 'cancel' });
    expect(cancelled).toEqual({ phase: 'cancelled', id: 1, subject });
    expect(checkReducer(cancelled, { type: 'finish', id: 1, check, now: 5000 })).toBe(cancelled);
    expect(checkReducer(cancelled, { type: 'fail', id: 1, failure })).toBe(cancelled);
  });

  it('ignores the answer of an earlier check after a new one started', () => {
    const cancelled = checkReducer(running, { type: 'cancel' });
    const second = checkReducer(cancelled, { type: 'start', subject });
    expect(second).toMatchObject({ phase: 'running', id: 2 });
    expect(checkReducer(second, { type: 'finish', id: 1, check, now: 9000 })).toBe(second);
    expect(checkReducer(second, { type: 'finish', id: 2, check, now: 9000 })).toMatchObject({ phase: 'done', id: 2 });
  });

  it.each<[string, CheckState]>([
    ['idle', initialCheck],
    ['done', { phase: 'done', id: 1, subject, check, elapsedMs: 1 }],
    ['failed', { phase: 'failed', id: 1, subject, failure }],
  ])('has nothing to cancel in %s', (_, state) => {
    expect(checkReducer(state, { type: 'cancel' })).toBe(state);
  });

  it('resets a finished check but never a running one', () => {
    const done = checkReducer(running, { type: 'finish', id: 1, check, now: 2000 });
    expect(checkReducer(done, { type: 'reset' })).toEqual({ phase: 'idle', id: 1 });
    expect(checkReducer(running, { type: 'reset' })).toBe(running);
  });
});

describe('leaving', () => {
  it('blocks navigation only while the check runs', () => {
    expect(blocksLeaving(initialCheck)).toBe(false);
    expect(blocksLeaving(running)).toBe(true);
    expect(blocksLeaving(checkReducer(running, { type: 'cancel' }))).toBe(false);
    expect(blocksLeaving(checkReducer(running, { type: 'finish', id: 1, check, now: 2000 }))).toBe(false);
    expect(blocksLeaving(checkReducer(running, { type: 'fail', id: 1, failure }))).toBe(false);
  });

  it('asks naming where the user is going', () => {
    expect(leaveQuestion('/runtimes/storage')).toBe('Cancel the check and leave for /runtimes/storage? Its runs are cancelled and no result is kept.');
    expect(leaveQuestion(undefined)).toBe('Cancel the check and leave? Its runs are cancelled and no result is kept.');
  });
});

describe('isAbort', () => {
  it('recognises the rejection of an aborted fetch', () => {
    expect(isAbort(new DOMException('aborted', 'AbortError'))).toBe(true);
    expect(isAbort(new Error('network'))).toBe(false);
    expect(isAbort(undefined)).toBe(false);
  });
});
