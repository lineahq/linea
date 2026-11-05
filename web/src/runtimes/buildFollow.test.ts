import { describe, expect, it } from 'vitest';
import type { RuntimeBuild } from '../api/types';
import { follow, NOT_FOLLOWED, parseBuildEvent } from './buildFollow';
import { mergeLog, nextLogFrom } from './runtimes';

const build = (state: string, log: string[], log_from: number, patch: Partial<RuntimeBuild> = {}): RuntimeBuild => ({
  id: 'b1', name: 'mine', collector_version: '0.162.0', created: '2026-10-31T10:00:00Z', log, log_from, log_lines: log_from + log.length,
  reused: false, state, verified: true, ...patch,
});

describe('follow', () => {
  it('adds the lines of each build event to the log and continues from the end', () => {
    let s = follow(NOT_FOLLOWED, 'read', build('queued', [], 0));
    expect(s).toMatchObject({ ended: false, log: { lines: [], skipped: 0 } });
    s = follow(s, 'build', build('running', ['a', 'b'], 0));
    s = follow(s, 'build', build('running', ['c'], 2));
    expect(s.log.lines).toEqual(['a', 'b', 'c']);
    expect(nextLogFrom(s.log)).toBe(3);
    expect(s.ended).toBe(false);
  });

  it('keeps the log it has when the end event repeats the build without lines', () => {
    let s = follow(NOT_FOLLOWED, 'build', build('running', ['a', 'b'], 0));
    s = follow(s, 'end', build('done', [], 2));
    expect(s.ended).toBe(true);
    expect(s.build?.state).toBe('done');
    expect(s.log.lines).toEqual(['a', 'b']);
  });

  it('keeps the manifest of the read through events that do not carry it', () => {
    let s = follow(NOT_FOLLOWED, 'read', { ...build('queued', [], 0), manifest_yaml: 'dist: {}\n' });
    s = follow(s, 'build', build('running', ['a'], 0));
    s = follow(s, 'end', build('done', [], 1));
    expect(s.build?.manifest_yaml).toBe('dist: {}\n');
  });

  it('does not take a finished state in a build event for the end of the stream', () => {
    const s = follow(NOT_FOLLOWED, 'build', build('failed', ['x'], 0));
    expect(s.ended).toBe(false);
  });

  it('takes a read of a finished build as the whole story', () => {
    const s = follow(NOT_FOLLOWED, 'read', build('cancelled', ['x'], 0));
    expect(s.ended).toBe(true);
    expect(s.log.lines).toEqual(['x']);
  });

  it('does not repeat lines when a reopened stream sends an overlapping page', () => {
    let s = follow(NOT_FOLLOWED, 'build', build('running', ['a', 'b', 'c'], 0));
    s = follow(s, 'build', build('running', ['c', 'd'], 2));
    expect(s.log.lines).toEqual(['a', 'b', 'c', 'd']);
  });

  it('notes lines the server no longer keeps', () => {
    const s = follow(NOT_FOLLOWED, 'read', build('running', ['x', 'y'], 5000));
    expect(s.log).toEqual({ lines: ['x', 'y'], skipped: 5000 });
    expect(nextLogFrom(s.log)).toBe(5002);
  });
});

describe('mergeLog', () => {
  it('jumps ahead when a page starts past what was read', () => {
    expect(mergeLog({ lines: ['a'], skipped: 0 }, { log: ['z'], log_from: 10 })).toEqual({ lines: ['z'], skipped: 10 });
  });
  it('treats a missing log as no lines', () => {
    expect(mergeLog({ lines: ['a'], skipped: 0 }, { log_from: 1 })).toEqual({ lines: ['a'], skipped: 0 });
  });
});

describe('parseBuildEvent', () => {
  it('reads a build and nothing else', () => {
    expect(parseBuildEvent(JSON.stringify(build('running', [], 0)))?.id).toBe('b1');
    expect(parseBuildEvent('not json')).toBeUndefined();
    expect(parseBuildEvent('null')).toBeUndefined();
    expect(parseBuildEvent('{"id":"b1"}')).toBeUndefined();
    expect(parseBuildEvent('{"state":"done"}')).toBeUndefined();
  });
});
