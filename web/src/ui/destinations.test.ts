import { describe, expect, it } from 'vitest';
import { activeBuildCount, activeCaptureCount, destinationOf } from './destinations';

describe('destinationOf', () => {
  it.each([
    ['/', 'debugger'],
    ['/runs', 'debugger'],
    ['/playground', 'playground'],
    ['/captures', 'capture'],
    ['/fleet', 'fleet'],
    ['/fleet/rollouts/r-1', 'fleet'],
    ['/collectors', 'fleet'],
    ['/captures/cap-1', 'capture'],
    ['/runtimes/upgrade', 'runtimes'],
    ['/saved', 'saved'],
    ['/saved/suites/s-1', 'saved'],
    ['/audit', 'audit'],
    ['/access', 'access'],
  ] as const)('%s belongs to %s', (path, want) => {
    expect(destinationOf(path)).toBe(want);
  });
});

describe('activeCaptureCount', () => {
  it('counts sessions that are not terminal', () => {
    expect(activeCaptureCount([{ terminal: false }, { terminal: true }, { terminal: false }])).toBe(2);
    expect(activeCaptureCount(undefined)).toBe(0);
  });
});

describe('activeBuildCount', () => {
  it('counts builds whose state has not finished', () => {
    const done = (s: string) => s !== 'running';
    expect(activeBuildCount([{ state: 'running' }, { state: 'succeeded' }], done)).toBe(1);
    expect(activeBuildCount(undefined, done)).toBe(0);
  });
});
