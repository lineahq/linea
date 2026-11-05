import { describe, expect, it } from 'vitest';
import { fromOf, rebuildHref, rebuildOf, returnTarget, safeFrom, setupTitle, withFrom } from './returnPath';

describe('safeFrom', () => {
  it.each([
    ['/', '/'],
    ['/?run=r1&node=a', '/?run=r1&node=a'],
    ['/saved/suites', '/saved/suites'],
  ])('keeps the in-app location %s', (input, want) => {
    expect(safeFrom(input)).toBe(want);
  });

  it.each([
    ['https://evil.example/'],
    ['//evil.example/'],
    ['/\\evil.example'],
    ['javascript:alert(1)'],
    ['runs'],
    [''],
    ['/a\nb'],
    [42],
    [undefined],
    [null],
    ['/' + 'x'.repeat(3000)],
  ])('refuses %j', (input) => {
    expect(safeFrom(input)).toBeUndefined();
  });
});

describe('returnTarget', () => {
  it.each([
    ['/', 'Back to the experiment', true],
    ['/?run=r1', 'Back to the experiment', true],
    ['/runs', 'Back to all runs', false],
    ['/captures/c1', 'Back to Live capture', false],
    ['/fleet/rollouts/r1', 'Back to Fleet', false],
    ['/saved/a1?x=1', 'Back to Saved work', false],
    ['/elsewhere', 'Back', false],
  ])('names the way back to %s', (from, label, isDebugger) => {
    expect(returnTarget(from)).toEqual({ href: from, label, debugger: isDebugger });
  });

  it('has no return path when opened directly, from a runtimes page or from a foreign location', () => {
    expect(returnTarget(undefined)).toBeUndefined();
    expect(returnTarget('/runtimes')).toBeUndefined();
    expect(returnTarget('/runtimes/storage?x=1')).toBeUndefined();
    expect(returnTarget('https://example.com')).toBeUndefined();
  });
});

describe('fromOf', () => {
  it('reads the origin from a search object of any shape', () => {
    expect(fromOf({ from: '/' })).toBe('/');
    expect(fromOf({ from: 3 })).toBeUndefined();
    expect(fromOf({})).toBeUndefined();
    expect(fromOf(undefined)).toBeUndefined();
    expect(fromOf('from=/')).toBeUndefined();
  });
});

describe('setupTitle', () => {
  it('names the draft only for a debugger origin that has one', () => {
    const dbg = returnTarget('/')!;
    expect(setupTitle(dbg, 'checkout-logs.yaml')).toEqual({ before: 'Setting up a runtime for your configuration', draft: 'checkout-logs.yaml' });
    expect(setupTitle(dbg, ' ')).toEqual({ before: 'Setting up a runtime for your experiment' });
    expect(setupTitle(returnTarget('/saved')!, 'a.yaml')).toEqual({ before: 'Setting up a runtime' });
  });
});

describe('withFrom', () => {
  it('appends a safe origin and keeps an existing query', () => {
    expect(withFrom('/runtimes/builds/b1', '/?run=r1')).toBe('/runtimes/builds/b1?from=%2F%3Frun%3Dr1');
    expect(withFrom('/runtimes?rebuild=b1', '/')).toBe('/runtimes?rebuild=b1&from=%2F');
    expect(withFrom('/runtimes/builds/b1', 'https://evil.example/')).toBe('/runtimes/builds/b1');
  });
});

describe('rebuild links', () => {
  it('opens the builder on a build and keeps the way back', () => {
    expect(rebuildHref('b-1', '/?run=r1')).toBe('/runtimes?rebuild=b-1&from=%2F%3Frun%3Dr1');
    expect(rebuildHref('b-1', undefined)).toBe('/runtimes?rebuild=b-1');
  });
  it('reads a plain build id and nothing else', () => {
    expect(rebuildOf({ rebuild: 'b-1.x_2' })).toBe('b-1.x_2');
    for (const bad of ['', 'a/b', 'a b', '../x', 'x'.repeat(200), 5, undefined]) expect(rebuildOf({ rebuild: bad })).toBeUndefined();
    expect(rebuildOf(undefined)).toBeUndefined();
  });
});
