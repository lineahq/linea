import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../../api/client';
import { projectsQuery } from '../../ui/ProjectMenu';
import { ALL_PROJECTS, availabilityText, toggleShare, workProjects } from '../../ui/project';
import { refusalText } from '../../access/accessView';

/** RuntimeAvailability says which projects a runtime runs work for. */
export function RuntimeAvailability({ projects }: { projects?: string[] }) {
  const list = useQuery(projectsQuery).data?.projects ?? [];
  return <span title="Projects whose runs and builds may use this runtime">{availabilityText(projects, list)}</span>;
}

/**
 * RuntimeShareForm chooses the projects a runtime runs work for: "Every
 * project", or some. It names the project set's revision it loaded; a 409
 * reloads the projects and keeps the choice.
 */
export function RuntimeShareForm({ name, projects, onDone }: { name: string; projects?: string[]; onDone: () => void }) {
  const qc = useQueryClient();
  const set = useQuery(projectsQuery);
  const [audience, setAudience] = useState<string[]>(projects?.length ? [...projects] : [ALL_PROJECTS]);
  const save = useMutation({
    mutationFn: () => api.setRuntimeAvailability(name, audience.includes(ALL_PROJECTS) ? [] : audience, set.data?.revision ?? 0),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['runtimes'] });
      void qc.invalidateQueries({ queryKey: projectsQuery.queryKey });
      onDone();
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 409) void qc.invalidateQueries({ queryKey: projectsQuery.queryKey });
    },
  });
  const choices = [{ id: ALL_PROJECTS, name: 'Every project' }, ...workProjects(set.data?.projects ?? [], '')];
  return (
    <form
      className="rt-actions"
      aria-label={`Share runtime ${name}`}
      onSubmit={(e) => {
        e.preventDefault();
        save.mutate();
      }}
    >
      <fieldset style={{ border: 0, padding: 0, margin: 0, display: 'flex', flexWrap: 'wrap', gap: 8 }}>
        {choices.map((p) => (
          <label key={p.id} style={{ display: 'inline-flex', gap: 4, alignItems: 'center' }}>
            <input type="checkbox" checked={audience.includes(p.id)} onChange={() => setAudience(toggleShare(audience, p.id))} />
            {p.name}
          </label>
        ))}
      </fieldset>
      <button type="button" className="od-btn od-btn--sm" onClick={onDone}>Cancel</button>
      <button type="submit" className="od-btn od-btn--primary od-btn--sm" disabled={save.isPending || !set.data || audience.length === 0} title={audience.length === 0 ? 'Choose at least one project' : undefined}>
        {save.isPending ? 'Saving…' : 'Save'}
      </button>
      {save.error && <div className="od-field__error" role="alert">{save.error instanceof ApiError ? refusalText(save.error.message) : String(save.error)}</div>}
    </form>
  );
}
