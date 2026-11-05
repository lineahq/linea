import type { Project, RoleBinding, RoleBindingRequest } from '../api/types';
import { validProjectID } from '../ui/project';

/** BindingForm is the add form as it holds its fields. */
export interface BindingForm {
  subject: 'group' | 'principal';
  name: string;
  role: string;
  scope: 'all' | 'project';
  project: string;
}

export const EMPTY_FORM: BindingForm = { subject: 'group', name: '', role: 'viewer', scope: 'all', project: '' };

/** ROLE_BINDINGS_KEY is the query key of the role binding list. */
export const ROLE_BINDINGS_KEY = ['role-bindings'];

/** AccessSearch is the Access page's URL: grant names a project to grant access in. */
export interface AccessSearch {
  grant?: string;
}

/** parseAccessSearch keeps a valid project ID in grant and drops anything else. */
export function parseAccessSearch(s: Record<string, unknown>): AccessSearch {
  return typeof s.grant === 'string' && validProjectID(s.grant) ? { grant: s.grant } : {};
}

/** grantForm is the empty form, scoped to project when one is named. */
export function grantForm(project: string | undefined): BindingForm {
  return project ? { ...EMPTY_FORM, scope: 'project', project } : EMPTY_FORM;
}

/**
 * grantableProjects are the projects a binding can be scoped to: those not
 * archived, plus chosen when the list no longer has it, so the form keeps
 * showing what it holds.
 */
export function grantableProjects(list: readonly Project[], chosen: string): Pick<Project, 'id' | 'name'>[] {
  const out: Pick<Project, 'id' | 'name'>[] = list.filter((p) => !p.archived);
  if (chosen && !out.some((p) => p.id === chosen)) out.push({ id: chosen, name: chosen });
  return out;
}

/**
 * projectMembers splits the bindings that reach project: its own, and those
 * that apply everywhere.
 */
export function projectMembers(bindings: readonly RoleBinding[], project: string): { own: RoleBinding[]; everywhere: RoleBinding[] } {
  return {
    own: bindings.filter((b) => b.scope === `project:${project}`),
    everywhere: bindings.filter((b) => b.scope === '*'),
  };
}

/** memberText names a binding's subject and role in a project's member list. */
export function memberText(b: Pick<RoleBinding, 'group' | 'principal' | 'role'>): string {
  return `${subjectText(b)} · ${b.role}`;
}

/** formDirty tells whether the form holds something the user typed. */
export function formDirty(f: BindingForm): boolean {
  return f.name.trim() !== '' || f.project.trim() !== '';
}

/**
 * formProblem is why the form cannot be sent yet, or undefined. The server
 * checks the same and more; this only spares a round trip.
 */
export function formProblem(f: BindingForm): string | undefined {
  const name = f.name.trim();
  if (!name) return f.subject === 'group' ? 'Name the group.' : 'Name the principal.';
  if (f.subject === 'principal' && !name.startsWith('user:') && !name.startsWith('service:')) return 'A principal starts with user: or service:.';
  if (!f.role) return 'Choose a role.';
  if (f.scope === 'project' && !f.project.trim()) return 'Name the project.';
  return undefined;
}

/** bindingRequest is the request that grants the form's binding at revision. */
export function bindingRequest(f: BindingForm, revision: number): RoleBindingRequest {
  const name = f.name.trim();
  return {
    revision,
    role: f.role,
    scope: f.scope === 'all' ? '*' : `project:${f.project.trim()}`,
    ...(f.subject === 'group' ? { group: name } : { principal: name }),
  };
}

/** subjectText names whom a binding grants to. */
export function subjectText(b: Pick<RoleBinding, 'group' | 'principal'>): string {
  return b.group ? `Group ${b.group}` : (b.principal ?? '');
}

/** scopeText words where a binding applies. */
export function scopeText(scope: string): string {
  if (scope === '*') return 'Everywhere';
  if (scope.startsWith('project:')) return `Project ${scope.slice('project:'.length)}`;
  return scope;
}

/** sourceView labels where a binding comes from; server config bindings are read-only here. */
export function sourceView(b: Pick<RoleBinding, 'source'>): { label: string; title: string; editable: boolean } {
  if (b.source === 'config') return { label: 'Server config', title: 'Set in the server config; change it there and restart the server.', editable: false };
  return { label: 'Managed', title: 'Added here or with linea access; every server instance applies it within a second.', editable: true };
}

/** refusalText is a refused change's reason, without the code the server prefixes. */
export function refusalText(message: string): string {
  const m = message.replace(/^[a-z_]+: /, '');
  return m.charAt(0).toUpperCase() + m.slice(1);
}
