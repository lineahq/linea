import { describe, expect, it } from 'vitest';
import { allows, denial, principalLabel, shows, signInHref, type Permission } from './access';
import type { Destination } from './destinations';

// The built-in roles of internal/debugger/authz, as GET /api/v1/session lists them.
const viewer = ['run.read', 'artifact.read', 'capture.read', 'fleet.read', 'runtime.read', 'token.own'];
const engineer = [...viewer, 'run.submit', 'run.delete', 'artifact.write', 'capture.start', 'runtime.build'];
const operator = [...engineer, 'fleet.config.write', 'fleet.rollout'];
const admin = [...operator, 'capture.raw', 'runtime.manage', 'audit.read', 'token.manage', 'access.manage'];

const WRITES: Permission[] = ['run.submit', 'run.delete', 'artifact.write', 'capture.start', 'capture.raw', 'fleet.config.write', 'fleet.rollout', 'runtime.manage', 'runtime.build', 'token.manage'];
const RAIL: Destination[] = ['debugger', 'playground', 'capture', 'fleet', 'runtimes', 'saved', 'audit', 'access'];

describe('denial', () => {
  it('disables every write for a viewer and names the permission', () => {
    for (const perm of WRITES) expect(denial(viewer, perm)).toBe(`Needs the ${perm} permission, which your role does not have`);
    expect(denial(viewer, 'run.read')).toBeUndefined();
  });

  it('allows an engineer runs, saves and runtime builds but not fleet changes or runtime management', () => {
    expect(['run.submit', 'run.delete', 'artifact.write', 'capture.start', 'runtime.build'].map((p) => allows(engineer, p as Permission))).toEqual([true, true, true, true, true]);
    expect(['fleet.config.write', 'fleet.rollout', 'runtime.manage', 'capture.raw'].map((p) => allows(engineer, p as Permission))).toEqual([false, false, false, false]);
  });

  it('allows a fleet operator fleet changes', () => {
    expect(allows(operator, 'fleet.config.write') && allows(operator, 'fleet.rollout')).toBe(true);
    expect(allows(operator, 'runtime.manage')).toBe(false);
  });

  it('allows an admin every write', () => {
    for (const perm of WRITES) expect(denial(admin, perm)).toBeUndefined();
  });

  it('offers everything while the permissions are unknown', () => {
    for (const perm of WRITES) expect(allows(undefined, perm)).toBe(true);
  });
});

describe('shows', () => {
  const rail = (held: string[] | undefined) => RAIL.filter((d) => shows(held, d));

  it('hides what a role cannot use', () => {
    expect(rail(viewer)).toEqual(['debugger', 'capture', 'fleet', 'runtimes', 'saved']);
    expect(rail(engineer)).toEqual(['debugger', 'playground', 'capture', 'fleet', 'runtimes', 'saved']);
    expect(rail(admin)).toEqual(RAIL);
    expect(rail([])).toEqual([]);
    expect(rail(undefined)).toEqual(RAIL);
  });
});

describe('signInHref', () => {
  it('returns to in-app paths only', () => {
    expect(signInHref('/runs?run=r-1')).toBe('/auth/login?return_to=%2Fruns%3Frun%3Dr-1');
    expect(signInHref('//evil.example')).toBe('/auth/login?return_to=%2F');
    expect(signInHref('https://evil.example')).toBe('/auth/login?return_to=%2F');
  });
});

describe('principalLabel', () => {
  it('names users by name and the local owner plainly', () => {
    expect(principalLabel({ id: 'user:alice', kind: 'user', name: 'Alice' })).toBe('Alice');
    expect(principalLabel({ id: 'token:ci', kind: 'token' })).toBe('token:ci');
    expect(principalLabel({ id: 'local-owner', kind: 'local', name: 'local-owner' })).toBe('Local owner');
  });
});
