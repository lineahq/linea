import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../api/client';
import type { Project, Projects, RoleBinding } from '../api/types';
import { PageHead } from '../ui/PageHead';
import { Icon } from '../ui/Icon';
import { Problem } from '../fleet/pages/FleetHead';
import { InlineConfirm } from '../capture/pages/InlineConfirm';
import { whenText } from '../saved/format';
import { useDenial } from '../ui/session';
import { projectsQuery } from '../ui/ProjectMenu';
import { EMPTY_PROJECT_FORM, LIBRARY, projectForm, projectFormProblem, projectRequest, retentionText, type ProjectForm } from '../ui/project';
import { memberText, projectMembers, refusalText, ROLE_BINDINGS_KEY } from './accessView';

/** ACCESS_NAV is the Access destination's sections. */
export const ACCESS_NAV = [
  { to: '/access', label: 'Role bindings', exact: true },
  { to: '/access/projects', label: 'Projects' },
];

/**
 * ProjectsPage lists the server's projects and lets an admin add, edit,
 * archive and restore them. Every change names the revision the page
 * loaded; a 409 reloads the list and keeps the form. Starting to edit
 * another project while the form holds unsaved changes asks first. For an
 * access manager each project lists who is bound in it, and Grant access
 * opens the Access form scoped to it.
 */
export function ProjectsPage() {
  const client = useQueryClient();
  const projects = useQuery(projectsQuery);
  const denied = useDenial('project.manage');
  const accessDenied = useDenial('access.manage');
  const bindings = useQuery({ queryKey: ROLE_BINDINGS_KEY, queryFn: () => api.roleBindings(), enabled: !accessDenied, retry: false });
  const [editing, setEditing] = useState<string>();
  const [base, setBase] = useState<ProjectForm>(EMPTY_PROJECT_FORM);
  const [form, setForm] = useState<ProjectForm>(EMPTY_PROJECT_FORM);
  const [replacing, setReplacing] = useState<Project | 'new'>();
  const [archiving, setArchiving] = useState<string>();
  const [refusal, setRefusal] = useState<string>();
  const revision = projects.data?.revision ?? 0;
  const dirty = JSON.stringify(form) !== JSON.stringify(base);

  const settled = (l: Projects) => {
    client.setQueryData(projectsQuery.queryKey, l);
    setRefusal(undefined);
  };
  const refused = (err: unknown) => {
    setRefusal(err instanceof ApiError ? refusalText(err.message) : String(err));
    if (err instanceof ApiError && err.status === 409) void client.invalidateQueries({ queryKey: projectsQuery.queryKey });
  };
  const start = (p: Project | 'new') => {
    const next = p === 'new' ? EMPTY_PROJECT_FORM : projectForm(p);
    setEditing(p === 'new' ? undefined : p.id);
    setBase(next);
    setForm(next);
    setReplacing(undefined);
  };
  const ask = (p: Project | 'new') => (dirty ? setReplacing(p) : start(p));
  const save = useMutation({
    mutationFn: (f: ProjectForm) => (editing ? api.updateProject(editing, projectRequest({ ...f, id: '' }, revision)) : api.createProject(projectRequest(f, revision))),
    onSuccess: (l) => {
      settled(l);
      start('new');
    },
    onError: refused,
  });
  const archive = useMutation({
    mutationFn: ({ id, archived }: { id: string; archived: boolean }) => api.setProjectArchived(id, archived, revision),
    onSuccess: (l) => {
      settled(l);
      setArchiving(undefined);
    },
    onError: (err) => {
      setArchiving(undefined);
      refused(err);
    },
  });

  const problem = projectFormProblem(form, !editing);
  const rows = projects.data?.projects ?? [];
  const field = (key: keyof ProjectForm, label: string, placeholder: string, disabled = false) => (
    <div className="od-field">
      <label htmlFor={`project-${key}`}>{label}</label>
      <input id={`project-${key}`} className="od-input" placeholder={placeholder} disabled={disabled} value={form[key]} onChange={(e) => setForm({ ...form, [key]: e.target.value })} />
    </div>
  );
  return (
    <>
      <PageHead title="Access" nav={ACCESS_NAV} />
      <div className="od-page">
        <div className="saved-stack" style={{ maxWidth: 1200 }}>
          {!denied && (
            <form
              className="audit-filters"
              aria-label={editing ? `Edit project ${editing}` : 'Add a project'}
              onSubmit={(e) => {
                e.preventDefault();
                if (!problem) save.mutate(form);
              }}
            >
              {field('id', 'ID', 'payments', !!editing)}
              {field('name', 'Name', 'Payments')}
              {field('defaultRuntime', 'Default runtime', 'server default')}
              {field('retentionRuns', 'Keep runs', 'server policy')}
              {field('retentionDays', 'Keep days', 'server policy')}
              {field('storageLimitMiB', 'Storage limit (MiB)', 'none')}
              <button type="submit" className="od-btn od-btn--primary" disabled={!!problem || save.isPending || !projects.data} title={problem}>
                {save.isPending ? 'Saving…' : editing ? 'Save changes' : 'Add project'}
              </button>
              {editing && (
                <button type="button" className="od-btn" onClick={() => ask('new')}>
                  Cancel
                </button>
              )}
            </form>
          )}
          {replacing && (
            <InlineConfirm confirmLabel="Discard changes" cancelLabel="Keep editing" onConfirm={() => start(replacing)} onCancel={() => setReplacing(undefined)}>
              The form has unsaved changes{editing ? ` to ${editing}` : ''}. Discard them?
            </InlineConfirm>
          )}
          {refusal && (
            <div className="od-callout od-callout--warn" role="alert">
              <Icon name="triangle-alert" size={16} />
              <div className="od-callout__main">
                <div className="od-callout__title">The change was refused</div>
                <div className="od-callout__body">{refusal}</div>
              </div>
            </div>
          )}
          {projects.isPending && <div className="od-empty" role="status"><div className="od-empty__title">Loading the projects…</div></div>}
          {projects.error && <Problem title="The projects could not be read" error={projects.error} retry={() => void projects.refetch()} />}
          {rows.length > 0 && (
            <div className="od-card" style={{ overflow: 'auto' }}>
              <table className="od-table">
                <thead>
                  <tr><th>Project</th><th>Default runtime</th><th>Retention</th>{!accessDenied && <th>Who has access</th>}<th>Your permissions</th><th>Changed</th><th /></tr>
                </thead>
                <tbody>
                  {rows.map((p) => (
                    <tr key={p.id}>
                      <td>
                        {p.name} <span className="mono od-subtle">{p.id}</span>
                        {p.built_in && <span className="od-chip od-chip--plain" style={{ marginLeft: 6 }}>{p.id === LIBRARY ? 'library' : 'built-in'}</span>}
                        {p.archived && <span className="od-chip od-chip--plain" style={{ marginLeft: 6 }}>archived</span>}
                      </td>
                      <td className="mono">{p.default_runtime || '—'}</td>
                      <td>{retentionText(p)}</td>
                      {!accessDenied && (
                        <td>
                          <Members project={p} bindings={bindings.data?.bindings} failed={!!bindings.error} />
                        </td>
                      )}
                      <td className="od-subtle" title={(p.permissions ?? []).join(', ')}>{(p.permissions ?? []).length || 'none'}</td>
                      <td className="od-subtle" title={p.updated ?? p.created}>{p.updated || p.created ? `${whenText((p.updated ?? p.created)!)} by ${p.updated_by ?? p.created_by ?? 'unknown'}` : '—'}</td>
                      <td>
                        {!denied && archiving !== p.id && (
                          <span style={{ display: 'inline-flex', gap: 4 }}>
                            <button type="button" className="od-btn od-btn--sm od-btn--ghost" onClick={() => ask(p)}>Edit</button>
                            {!p.built_in && (
                              <button type="button" className="od-btn od-btn--sm od-btn--ghost" onClick={() => (p.archived ? archive.mutate({ id: p.id, archived: false }) : setArchiving(p.id))}>
                                {p.archived ? 'Restore' : 'Archive'}
                              </button>
                            )}
                          </span>
                        )}
                        {archiving === p.id && (
                          <InlineConfirm danger busy={archive.isPending} confirmLabel="Archive project" cancelLabel="Keep project" onConfirm={() => archive.mutate({ id: p.id, archived: true })} onCancel={() => setArchiving(undefined)}>
                            {p.name} takes no new runs or saved work; its history stays readable until retention removes it.
                          </InlineConfirm>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </div>
    </>
  );
}

/**
 * Members lists the bindings scoped to a project, says how many apply
 * everywhere, and links to granting access in the project.
 */
function Members({ project, bindings, failed }: { project: Project; bindings?: RoleBinding[]; failed: boolean }) {
  if (failed) return <span className="od-subtle">Could not be read</span>;
  if (!bindings) return <span className="od-subtle">…</span>;
  const { own, everywhere } = projectMembers(bindings, project.id);
  return (
    <div style={{ display: 'grid', gap: 2 }}>
      {own.length === 0 && <span className="od-subtle">No project bindings</span>}
      {own.map((b) => (
        <span key={b.id} className="mono">{memberText(b)}</span>
      ))}
      {everywhere.length > 0 && (
        <span className="od-subtle" title={everywhere.map(memberText).join(', ')}>
          and {everywhere.length} binding{everywhere.length === 1 ? '' : 's'} everywhere
        </span>
      )}
      {!project.archived && (
        <Link to="/access" search={{ grant: project.id }} className="od-btn od-btn--sm od-btn--ghost" style={{ justifySelf: 'start' }}>
          Grant access
        </Link>
      )}
    </div>
  );
}
