import { describe, expect, it } from 'vitest';
import { buildElapsed, buildOutcome, logText, numberedLog } from './buildView';

describe('buildElapsed', () => {
  const now = Date.parse('2026-10-15T09:01:16Z');
  it('counts up to now while the build runs', () => {
    expect(buildElapsed({ state: 'running', started: '2026-10-15T09:00:00Z' }, now)).toBe('1:16');
  });
  it('stops at the end of a finished build', () => {
    expect(buildElapsed({ state: 'done', started: '2026-10-15T09:00:00Z', finished: '2026-10-15T09:00:40Z' }, now)).toBe('0:40');
  });
  it('has none for a queued build or unreadable times', () => {
    expect(buildElapsed({ state: 'queued' }, now)).toBeUndefined();
    expect(buildElapsed({ state: 'running', started: 'soon' }, now)).toBeUndefined();
  });
});

describe('numberedLog', () => {
  it('counts the lines the server no longer keeps', () => {
    expect(numberedLog(['a', 'b'], 2000)).toEqual([{ n: 2001, text: 'a' }, { n: 2002, text: 'b' }]);
    expect(numberedLog([], 0)).toEqual([]);
  });
  it('copies the log, saying when earlier lines are missing', () => {
    expect(logText(['a', 'b'], 0)).toBe('a\nb');
    expect(logText(['a'], 3)).toBe('[3 earlier log lines are not kept]\na');
  });
});

describe('buildOutcome', () => {
  const b = { name: 'acme', reused: false };
  it('words each way a build ends and nothing for one in progress', () => {
    expect(buildOutcome({ ...b, state: 'done' })).toMatchObject({ tone: 'pass', title: 'Built acme' });
    expect(buildOutcome({ ...b, state: 'cancelled' })?.tone).toBe('warn');
    expect(buildOutcome({ ...b, state: 'failed', error: 'compile error' })).toMatchObject({ tone: 'fail', body: 'compile error' });
    expect(buildOutcome({ ...b, state: 'failed' })?.body).toContain('log');
    expect(buildOutcome({ ...b, state: 'running' })).toBeUndefined();
  });
  it('says when the binary of an identical earlier build was reused', () => {
    expect(buildOutcome({ ...b, state: 'done', reused: true })?.body).toContain('nothing was compiled');
    expect(buildOutcome({ ...b, state: 'done' })?.body).not.toContain('nothing was compiled');
  });
});
