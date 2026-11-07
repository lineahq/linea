import { useEffect, useRef, useState, useSyncExternalStore } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api, setWorkProject } from '../api/client';
import { Icon } from './Icon';
import { chooseProject, workProjects } from './project';

const KEY = 'linea.project';

/** projectsQuery reads the projects the caller holds a permission in. */
export const projectsQuery = { queryKey: ['projects'], queryFn: () => api.projects(), staleTime: 60_000, retry: false } as const;

function stored(): string | undefined {
  try {
    return window.localStorage.getItem(KEY) ?? undefined;
  } catch {
    return undefined;
  }
}

function remember(project: string) {
  try {
    window.localStorage.setItem(KEY, project);
  } catch {
    // The choice then lasts for this page only.
  }
}

function fromURL(): string | undefined {
  return new URLSearchParams(window.location.search).get('project') ?? undefined;
}

// Requests sent before the project list arrives work in the project the URL
// or the last visit named; the list then confirms or replaces it.
let current = fromURL() ?? stored() ?? '';
setWorkProject(current);
const listeners = new Set<() => void>();

/** useWorkProject is the project requests work in; empty until one is known. */
export function useWorkProject(): string {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => current,
  );
}

/**
 * ProjectMenu is the rail's project switcher: requests work in the chosen
 * project, so lists show its work and new runs and saved work go there.
 * Switching drops every cached read but the session and the project list,
 * and pages read again; local drafts stay as they are. It shows while the
 * caller can work in more than one project.
 */
export function ProjectMenu() {
  const client = useQueryClient();
  const projects = useQuery(projectsQuery);
  const [open, setOpen] = useState(false);
  const project = useWorkProject();
  const ref = useRef<HTMLDivElement>(null);
  const list = projects.data?.projects ?? [];

  const choose = (id: string) => {
    setOpen(false);
    if (id === current) return;
    // Reread even when the first known project is the default one: a list
    // that named no project holds every project the caller may read.
    current = id;
    setWorkProject(id);
    remember(id);
    listeners.forEach((l) => l());
    void client.resetQueries({ predicate: (q) => q.queryKey[0] !== 'session' && q.queryKey[0] !== 'projects' });
  };

  useEffect(() => {
    if (!projects.data) return;
    const chosen = chooseProject(projects.data.projects ?? [], fromURL(), current);
    if (chosen && chosen !== current) choose(chosen);
    // Choose once per project list; choose is rebuilt every render.
  }, [projects.data]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent | KeyboardEvent) => {
      if (e instanceof KeyboardEvent ? e.key === 'Escape' : !ref.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', close);
    document.addEventListener('keydown', close);
    return () => {
      document.removeEventListener('mousedown', close);
      document.removeEventListener('keydown', close);
    };
  }, [open]);

  const work = workProjects(list, project);
  if (work.length < 2) return null;
  const name = work.find((p) => p.id === project)?.name ?? project;
  return (
    <div ref={ref} className="od-rail__settings">
      <button type="button" className="od-rail__item" aria-expanded={open} aria-haspopup="menu" title={`Working in ${name}`} onClick={() => setOpen(!open)}>
        <Icon name="folder-open" size={20} />
        <span className="od-rail__project">{name}</span>
      </button>
      {open && (
        <div className="od-menu od-rail__menu" role="menu" aria-label="Project">
          <div className="od-menu__label">Work in project</div>
          {work.map((p) => (
            <button key={p.id} type="button" role="menuitemradio" aria-checked={p.id === project} className="od-menu__item" title={p.id} onClick={() => choose(p.id)}>
              {p.id === project ? <Icon name="check" size={14} /> : <span style={{ width: 14 }} />}
              {p.name}
              {p.archived && <span className="od-subtle"> (archived)</span>}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
