import { describe, expect, it } from 'vitest';
import type { Project } from '../api/types';
import {
  audienceText,
  availabilityText,
  chooseProject,
  EMPTY_PROJECT_FORM,
  projectForm,
  projectFormProblem,
  projectRequest,
  retentionText,
  toggleShare,
  validProjectID,
  withProject,
  workProjects,
} from './project';

const p = (id: string, extra: Partial<Project> = {}): Project => ({ id, name: id[0].toUpperCase() + id.slice(1), built_in: false, archived: false, ...extra });
const list = [p('default', { built_in: true }), p('library', { built_in: true }), p('payments'), p('old', { archived: true })];

describe('withProject', () => {
  it('adds the project to paths with and without a query, before a fragment', () => {
    expect(withProject('/api/v1/runs', 'payments')).toBe('/api/v1/runs?project=payments');
    expect(withProject('/api/v1/runs?limit=5', 'payments')).toBe('/api/v1/runs?limit=5&project=payments');
    expect(withProject('/x#y', 'a b')).toBe('/x?project=a%20b#y');
    expect(withProject('/api/v1/runs', '')).toBe('/api/v1/runs');
    expect(withProject('/copies?project=library', 'payments')).toBe('/copies?project=library');
  });
});

describe('choosing a project', () => {
  it('lists unarchived projects but the library, keeping the current one', () => {
    expect(workProjects(list, '').map((x) => x.id)).toEqual(['default', 'payments']);
    expect(workProjects(list, 'old').map((x) => x.id)).toEqual(['default', 'payments', 'old']);
  });
  it('prefers the URL, then the stored one, then the default', () => {
    expect(chooseProject(list, 'payments', 'default')).toBe('payments');
    expect(chooseProject(list, 'gone', 'payments')).toBe('payments');
    expect(chooseProject(list, 'library', 'old')).toBe('default');
    expect(chooseProject([p('payments'), p('checkout')], undefined, undefined)).toBe('payments');
    expect(chooseProject([], 'payments', undefined)).toBe('');
  });
});

describe('sharing lists', () => {
  it('describes audiences by name', () => {
    expect(audienceText([], list)).toBe('No project');
    expect(audienceText(['*'], list)).toBe('Every project');
    expect(audienceText(['payments', 'ghost'], list)).toBe('Payments, ghost');
    expect(availabilityText(undefined, list)).toBe('Every project');
  });
  it('keeps "*" alone', () => {
    expect(toggleShare([], 'payments')).toEqual(['payments']);
    expect(toggleShare(['payments'], 'payments')).toEqual([]);
    expect(toggleShare(['payments'], '*')).toEqual(['*']);
    expect(toggleShare(['*'], '*')).toEqual([]);
    expect(toggleShare(['*'], 'payments')).toEqual(['payments']);
  });
});

describe('the project form', () => {
  it('checks IDs like the server', () => {
    expect(validProjectID('payments-2')).toBe(true);
    for (const bad of ['', '-x', 'Pay', 'a_b', 'x'.repeat(65)]) expect(validProjectID(bad)).toBe(false);
  });
  it('refuses missing names and fractional numbers', () => {
    expect(projectFormProblem({ ...EMPTY_PROJECT_FORM, id: 'pay' }, true)).toMatch(/Name/);
    expect(projectFormProblem({ ...EMPTY_PROJECT_FORM, id: 'pay', name: 'Pay', retentionRuns: '1.5' }, true)).toMatch(/runs/);
    expect(projectFormProblem({ ...EMPTY_PROJECT_FORM, name: 'Pay' }, false)).toBeUndefined();
  });
  it('round-trips a project through the form', () => {
    const proj = p('payments', { retention_runs: 50, retention_age_seconds: 7 * 86_400, storage_limit: 64 << 20, default_runtime: 'v162' });
    const f = projectForm(proj);
    expect(f).toMatchObject({ retentionRuns: '50', retentionDays: '7', storageLimitMiB: '64', defaultRuntime: 'v162' });
    expect(projectRequest(f, 3)).toEqual({ revision: 3, id: 'payments', name: 'Payments', default_runtime: 'v162', retention_runs: 50, retention_age_seconds: 7 * 86_400, storage_limit: 64 << 20 });
    expect(retentionText(proj)).toBe('50 runs · 7 days · 64 MiB');
    expect(retentionText(p('x'))).toBe('Server policy');
  });
});
