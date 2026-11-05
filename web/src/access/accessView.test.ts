import { describe, expect, it } from 'vitest';
import { bindingRequest, EMPTY_FORM, formDirty, formProblem, grantableProjects, grantForm, memberText, parseAccessSearch, projectMembers, refusalText, scopeText, sourceView, subjectText } from './accessView';

describe('formProblem', () => {
  it('asks for what is missing', () => {
    expect(formProblem(EMPTY_FORM)).toBe('Name the group.');
    expect(formProblem({ ...EMPTY_FORM, subject: 'principal', name: 'alice' })).toBe('A principal starts with user: or service:.');
    expect(formProblem({ ...EMPTY_FORM, name: 'sre', scope: 'project', project: ' ' })).toBe('Name the project.');
    expect(formProblem({ ...EMPTY_FORM, name: 'sre' })).toBeUndefined();
    expect(formProblem({ ...EMPTY_FORM, subject: 'principal', name: 'service:ci' })).toBeUndefined();
  });
});

describe('bindingRequest', () => {
  it('names one subject and the scope', () => {
    expect(bindingRequest({ ...EMPTY_FORM, name: ' sre ', role: 'engineer' }, 3)).toEqual({ revision: 3, group: 'sre', role: 'engineer', scope: '*' });
    expect(bindingRequest({ subject: 'principal', name: 'service:ci', role: 'viewer', scope: 'project', project: 'payments' }, 0)).toEqual({
      revision: 0,
      principal: 'service:ci',
      role: 'viewer',
      scope: 'project:payments',
    });
  });
});

describe('formDirty', () => {
  it('is dirty once something is typed', () => {
    expect(formDirty(EMPTY_FORM)).toBe(false);
    expect(formDirty({ ...EMPTY_FORM, name: 'sre' })).toBe(true);
  });
});

describe('words', () => {
  it('names subjects, scopes and sources', () => {
    expect(subjectText({ group: 'sre' })).toBe('Group sre');
    expect(subjectText({ principal: 'service:ci' })).toBe('service:ci');
    expect(scopeText('*')).toBe('Everywhere');
    expect(scopeText('project:payments')).toBe('Project payments');
    expect(sourceView({ source: 'config' }).editable).toBe(false);
    expect(sourceView({ source: 'managed' }).editable).toBe(true);
  });

  it('drops the code prefix from a refusal', () => {
    expect(refusalText('conflict: the role bindings changed or already grant this; reload')).toBe('The role bindings changed or already grant this; reload');
  });
});

describe('project access', () => {
  const b = (id: string, scope: string, extra: object = {}) => ({ id, source: 'managed' as const, group: id, role: 'engineer', scope, ...extra });

  it('reads only a valid project to grant in from the URL', () => {
    expect(parseAccessSearch({ grant: 'payments' })).toEqual({ grant: 'payments' });
    expect(parseAccessSearch({ grant: 'Bad ID' })).toEqual({});
    expect(parseAccessSearch({ grant: 3, other: 'x' })).toEqual({});
  });

  it('scopes a new grant to the named project', () => {
    expect(grantForm('payments')).toEqual({ ...EMPTY_FORM, scope: 'project', project: 'payments' });
    expect(grantForm(undefined)).toBe(EMPTY_FORM);
  });

  it('offers projects that are not archived and keeps the chosen one', () => {
    const list = [
      { id: 'default', name: 'Default', built_in: true, archived: false },
      { id: 'old', name: 'Old', built_in: false, archived: true },
      { id: 'payments', name: 'Payments', built_in: false, archived: false },
    ];
    expect(grantableProjects(list, '').map((p) => p.id)).toEqual(['default', 'payments']);
    expect(grantableProjects(list, 'gone').map((p) => p.id)).toEqual(['default', 'payments', 'gone']);
    expect(grantableProjects(list, 'payments').map((p) => p.id)).toEqual(['default', 'payments']);
  });

  it('splits the members of a project into its own and those bound everywhere', () => {
    const all = [b('sre', '*'), b('pay', 'project:payments'), b('shop', 'project:checkout'), b('ci', 'project:payments', { group: undefined, principal: 'service:ci', role: 'viewer' })];
    const m = projectMembers(all, 'payments');
    expect(m.own.map((x) => x.id)).toEqual(['pay', 'ci']);
    expect(m.everywhere.map((x) => x.id)).toEqual(['sre']);
    expect(m.own.map(memberText)).toEqual(['Group pay · engineer', 'service:ci · viewer']);
  });
});
