import type { Project } from '../api/types';

/** DEFAULT_PROJECT holds work that names no project. */
export const DEFAULT_PROJECT = 'default';
/** LIBRARY is the built-in project an admin publishes shared work in. */
export const LIBRARY = 'library';
/** ALL_PROJECTS shares with every project; it stands alone in a list. */
export const ALL_PROJECTS = '*';

/** withProject adds project=<id> to an API path; an empty project, or a path that names one, is left unchanged. */
export function withProject(path: string, project: string): string {
  if (!project) return path;
  const hash = path.indexOf('#');
  const [head, tail] = hash < 0 ? [path, ''] : [path.slice(0, hash), path.slice(hash)];
  const q = head.indexOf('?');
  if (q >= 0 && new URLSearchParams(head.slice(q)).has('project')) return path;
  return `${head}${head.includes('?') ? '&' : '?'}project=${encodeURIComponent(project)}${tail}`;
}

/** workProjects are the projects to work in: every unarchived one but the library, and the current one. */
export function workProjects(list: readonly Project[], current: string): Project[] {
  return list.filter((p) => p.id !== LIBRARY && (!p.archived || p.id === current));
}

/**
 * chooseProject is the project the client works in: the one the URL names,
 * else the one stored last, while the caller can work there; otherwise the
 * default project when listed, else the first. Empty while none is listed.
 */
export function chooseProject(list: readonly Project[], fromURL: string | undefined, stored: string | undefined): string {
  const work = workProjects(list, '');
  for (const id of [fromURL, stored]) if (id && work.some((p) => p.id === id)) return id;
  if (work.some((p) => p.id === DEFAULT_PROJECT)) return DEFAULT_PROJECT;
  return work[0]?.id ?? '';
}

/** validProjectID mirrors the server: 1 to 64 lowercase letters, digits and hyphens, not starting with a hyphen. */
export function validProjectID(id: string): boolean {
  return /^[a-z0-9][a-z0-9-]{0,63}$/.test(id);
}

/** audienceText says whom a sharing list reaches, by project name where known. */
export function audienceText(audience: readonly string[] | undefined, list: readonly Project[]): string {
  if (!audience?.length) return 'No project';
  if (audience.includes(ALL_PROJECTS)) return 'Every project';
  return audience.map((id) => list.find((p) => p.id === id)?.name || id).join(', ');
}

/** availabilityText says which projects a runtime runs work for; no list is every project. */
export function availabilityText(projects: readonly string[] | undefined, list: readonly Project[]): string {
  return projects?.length ? audienceText(projects, list) : 'Every project';
}

/** toggleShare adds or removes a project from a sharing list; "*" replaces the list, as any project replaces "*". */
export function toggleShare(audience: readonly string[], id: string): string[] {
  if (id === ALL_PROJECTS) return audience.includes(ALL_PROJECTS) ? [] : [ALL_PROJECTS];
  const rest = audience.filter((p) => p !== ALL_PROJECTS);
  return rest.includes(id) ? rest.filter((p) => p !== id) : [...rest, id];
}

/** ProjectForm is the project editor's fields as typed. */
export interface ProjectForm {
  id: string;
  name: string;
  defaultRuntime: string;
  retentionRuns: string;
  retentionDays: string;
  storageLimitMiB: string;
}

export const EMPTY_PROJECT_FORM: ProjectForm = { id: '', name: '', defaultRuntime: '', retentionRuns: '', retentionDays: '', storageLimitMiB: '' };

const MiB = 1 << 20;
const DAY = 86_400;

/** projectForm fills the editor from a project. */
export function projectForm(p: Project): ProjectForm {
  return {
    id: p.id,
    name: p.name,
    defaultRuntime: p.default_runtime ?? '',
    retentionRuns: p.retention_runs ? String(p.retention_runs) : '',
    retentionDays: p.retention_age_seconds ? String(p.retention_age_seconds / DAY) : '',
    storageLimitMiB: p.storage_limit ? String(p.storage_limit / MiB) : '',
  };
}

const wholeOrEmpty = (s: string) => s.trim() === '' || /^\d+$/.test(s.trim());

/** projectFormProblem is why the form cannot be sent, or undefined. `creating` also checks the ID. */
export function projectFormProblem(f: ProjectForm, creating: boolean): string | undefined {
  if (creating && !validProjectID(f.id)) return 'A project ID is 1 to 64 lowercase letters, digits and hyphens.';
  if (!f.name.trim()) return 'Name the project.';
  if (!wholeOrEmpty(f.retentionRuns)) return 'Keep a whole number of runs, or leave it empty.';
  if (!wholeOrEmpty(f.retentionDays)) return 'Keep runs a whole number of days, or leave it empty.';
  if (!wholeOrEmpty(f.storageLimitMiB)) return 'Limit storage to a whole number of MiB, or leave it empty.';
  return undefined;
}

/** projectRequest is the request a valid form sends at revision. */
export function projectRequest(f: ProjectForm, revision: number) {
  const n = (s: string, unit = 1) => (s.trim() ? Number(s.trim()) * unit : undefined);
  return {
    revision,
    id: f.id || undefined,
    name: f.name.trim(),
    default_runtime: f.defaultRuntime.trim() || undefined,
    retention_runs: n(f.retentionRuns),
    retention_age_seconds: n(f.retentionDays, DAY),
    storage_limit: n(f.storageLimitMiB, MiB),
  };
}

/** retentionText summarizes a project's own retention, or the server's when it sets none. */
export function retentionText(p: Project): string {
  const parts: string[] = [];
  if (p.retention_runs) parts.push(`${p.retention_runs} runs`);
  if (p.retention_age_seconds) parts.push(`${Math.round(p.retention_age_seconds / DAY)} days`);
  if (p.storage_limit) parts.push(`${Math.round(p.storage_limit / MiB)} MiB`);
  return parts.length ? parts.join(' · ') : 'Server policy';
}
