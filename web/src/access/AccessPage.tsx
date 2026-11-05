import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../api/client';
import { accessRoute } from '../router';
import type { RoleBindings } from '../api/types';
import { PageHead } from '../ui/PageHead';
import { Icon } from '../ui/Icon';
import { Problem } from '../fleet/pages/FleetHead';
import { InlineConfirm } from '../capture/pages/InlineConfirm';
import { whenText } from '../saved/format';
import { ACCESS_NAV } from './ProjectsPage';
import { projectsQuery } from '../ui/ProjectMenu';
import { bindingRequest, EMPTY_FORM, formProblem, grantableProjects, grantForm, refusalText, ROLE_BINDINGS_KEY, scopeText, sourceView, subjectText, type BindingForm } from './accessView';

const KEY = ROLE_BINDINGS_KEY;

/**
 * AccessPage lists the team server's role bindings and changes the managed
 * ones. Server config bindings are shown read-only. Every change names the
 * revision the page loaded; a 409 reloads the list and keeps the form, so
 * nothing typed is lost. It needs access.manage; the rail hides it without.
 * A link with ?grant=<project> opens the form scoped to that project.
 */
export function AccessPage() {
  const client = useQueryClient();
  const bindings = useQuery({ queryKey: KEY, queryFn: () => api.roleBindings(), retry: false });
  const { grant } = accessRoute.useSearch();
  const projects = useQuery(projectsQuery);
  const [form, setForm] = useState<BindingForm>(() => grantForm(grant));
  const [refusal, setRefusal] = useState<string>();
  const [removing, setRemoving] = useState<string>();
  const settled = (l: RoleBindings) => {
    client.setQueryData(KEY, l);
    setRefusal(undefined);
  };
  const refused = (err: unknown) => {
    setRefusal(err instanceof ApiError ? refusalText(err.message) : String(err));
    if (err instanceof ApiError && err.status === 409) void client.invalidateQueries({ queryKey: KEY });
  };
  const add = useMutation({
    mutationFn: (f: BindingForm) => api.addRoleBinding(bindingRequest(f, bindings.data?.revision ?? 0)),
    onSuccess: (l) => {
      settled(l);
      setForm(EMPTY_FORM);
    },
    onError: refused,
  });
  const remove = useMutation({
    mutationFn: (id: string) => api.removeRoleBinding(id, bindings.data?.revision ?? 0),
    onSuccess: (l) => {
      settled(l);
      setRemoving(undefined);
    },
    onError: (err) => {
      setRemoving(undefined);
      refused(err);
    },
  });
  const problem = formProblem(form);
  const roles = bindings.data?.roles ?? [];
  const rows = bindings.data?.bindings ?? [];
  return (
    <>
      <PageHead title="Access" nav={ACCESS_NAV} />
      <div className="od-page">
        <div className="saved-stack" style={{ maxWidth: 1200 }}>
          <form
            className="audit-filters"
            aria-label="Grant a role"
            onSubmit={(e) => {
              e.preventDefault();
              if (!problem) add.mutate(form);
            }}
          >
            <div className="od-field">
              <label htmlFor="access-subject">Grant to</label>
              <select id="access-subject" className="od-select" value={form.subject} onChange={(e) => setForm({ ...form, subject: e.target.value as BindingForm['subject'] })}>
                <option value="group">Group</option>
                <option value="principal">Principal</option>
              </select>
            </div>
            <div className="od-field">
              <label htmlFor="access-name">{form.subject === 'group' ? 'Group' : 'Principal'}</label>
              <input
                id="access-name"
                className="od-input"
                placeholder={form.subject === 'group' ? 'platform-team' : 'user:<issuer>/<subject>'}
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
              />
            </div>
            <div className="od-field">
              <label htmlFor="access-role">Role</label>
              <select id="access-role" className="od-select" value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value })}>
                {roles.map((r) => (
                  <option key={r.name} value={r.name} title={(r.permissions ?? []).join(', ')}>{r.name}</option>
                ))}
              </select>
            </div>
            <div className="od-field">
              <label htmlFor="access-scope">Scope</label>
              <select id="access-scope" className="od-select" value={form.scope} onChange={(e) => setForm({ ...form, scope: e.target.value as BindingForm['scope'] })}>
                <option value="all">Everywhere</option>
                <option value="project">One project</option>
              </select>
            </div>
            {form.scope === 'project' && (
              <div className="od-field">
                <label htmlFor="access-project">Project</label>
                <select id="access-project" className="od-select" value={form.project} onChange={(e) => setForm({ ...form, project: e.target.value })}>
                  <option value="">Choose a project…</option>
                  {grantableProjects(projects.data?.projects ?? [], form.project).map((p) => (
                    <option key={p.id} value={p.id}>{p.name === p.id ? p.id : `${p.name} (${p.id})`}</option>
                  ))}
                </select>
              </div>
            )}
            <button type="submit" className="od-btn od-btn--primary" disabled={!!problem || add.isPending || !bindings.data} title={problem}>
              {add.isPending ? 'Granting…' : 'Grant'}
            </button>
          </form>
          {refusal && (
            <div className="od-callout od-callout--warn" role="alert">
              <Icon name="triangle-alert" size={16} />
              <div className="od-callout__main">
                <div className="od-callout__title">The change was refused</div>
                <div className="od-callout__body">{refusal}</div>
              </div>
            </div>
          )}
          {bindings.isPending && <div className="od-empty" role="status"><div className="od-empty__title">Loading the role bindings…</div></div>}
          {bindings.error && <Problem title="The role bindings could not be read" error={bindings.error} retry={() => void bindings.refetch()} />}
          {rows.length > 0 && (
            <div className="od-card" style={{ overflow: 'auto' }}>
              <table className="od-table">
                <thead>
                  <tr><th>Subject</th><th>Role</th><th>Scope</th><th>Source</th><th>Added</th><th /></tr>
                </thead>
                <tbody>
                  {rows.map((b) => {
                    const source = sourceView(b);
                    const role = roles.find((r) => r.name === b.role);
                    return (
                      <tr key={b.id}>
                        <td className="mono">{subjectText(b)}</td>
                        <td title={role?.permissions?.join(', ')}>{b.role}</td>
                        <td>{scopeText(b.scope)}</td>
                        <td><span className="od-chip od-chip--plain" title={source.title}>{source.label}</span></td>
                        <td className="od-subtle" title={b.created}>{b.created ? `${whenText(b.created)} by ${b.created_by ?? 'unknown'}` : '—'}</td>
                        <td>
                          {source.editable && removing !== b.id && (
                            <button type="button" className="od-btn od-btn--sm od-btn--ghost" onClick={() => setRemoving(b.id)}>Remove</button>
                          )}
                          {removing === b.id && (
                            <InlineConfirm danger busy={remove.isPending} confirmLabel="Remove binding" cancelLabel="Keep binding" onConfirm={() => remove.mutate(b.id)} onCancel={() => setRemoving(undefined)}>
                              {subjectText(b)} loses {b.role} ({scopeText(b.scope)}) unless another binding grants it.
                            </InlineConfirm>
                          )}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </div>
    </>
  );
}
