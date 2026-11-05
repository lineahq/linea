import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../api/client';
import type { LibraryItem } from '../api/types';
import { Icon } from '../ui/Icon';
import { Problem } from '../fleet/pages/FleetHead';
import { useDenial } from '../ui/session';
import { projectsQuery, useWorkProject } from '../ui/ProjectMenu';
import { ALL_PROJECTS, audienceText, LIBRARY, toggleShare, workProjects } from '../ui/project';
import { kindLabel, whenText } from './format';
import { SavedHead } from './SavedHead';
import { useSavedArtifacts } from './useSavedArtifacts';
import { refusalText } from '../access/accessView';

/**
 * LibraryPage lists the configurations, fixtures and scenarios an admin
 * published for projects to use: those shared with the current project, or
 * for a publisher the whole library. A shared item runs as a saved source in
 * the project, or is copied in to edit. A publisher publishes the current
 * project's work and chooses each item's audience; one audience is edited
 * at a time, so nothing typed is replaced.
 */
export function LibraryPage() {
  const client = useQueryClient();
  const project = useWorkProject();
  const publishDenied = useDenial('library.publish');
  const writeDenied = useDenial('artifact.write');
  const [everything, setEverything] = useState(false);
  const whole = everything && !publishDenied;
  const projects = useQuery(projectsQuery);
  const library = useInfiniteQuery({
    queryKey: ['library', whole ? LIBRARY : project],
    queryFn: ({ pageParam }) => api.library({ cursor: pageParam || undefined, project: whole ? LIBRARY : undefined }),
    initialPageParam: '',
    getNextPageParam: (last) => last.next_cursor || undefined,
    retry: false,
  });
  const saved = useSavedArtifacts(!publishDenied);
  const [sharing, setSharing] = useState<{ id: string; audience: string[] }>();
  const [publishing, setPublishing] = useState('');
  const [notice, setNotice] = useState<{ ok: boolean; text: string }>();
  const fail = (err: unknown) => setNotice({ ok: false, text: err instanceof ApiError ? refusalText(err.message) : String(err) });
  const revision = library.data?.pages[0]?.revision ?? 0;
  const list = projects.data?.projects ?? [];
  const audienceChoices = workProjects(list, '');

  const copy = useMutation({
    mutationFn: (item: LibraryItem) => api.copyArtifact(item.artifact.id),
    onSuccess: (s) => {
      setNotice({ ok: true, text: `Copied “${s.artifact.label}” into this project.` });
      void client.invalidateQueries({ queryKey: ['saved'] });
    },
    onError: fail,
  });
  const share = useMutation({
    mutationFn: (s: { id: string; audience: string[] }) => api.setLibraryAudience(s.id, s.audience, revision),
    onSuccess: () => {
      setSharing(undefined);
      setNotice(undefined);
      void client.invalidateQueries({ queryKey: ['library'] });
    },
    onError: (err) => {
      fail(err);
      if (err instanceof ApiError && err.status === 409) void client.invalidateQueries({ queryKey: ['library'] });
    },
  });
  const publish = useMutation({
    mutationFn: (id: string) => api.copyArtifactTo(id, LIBRARY),
    onSuccess: (s) => {
      setPublishing('');
      setNotice({ ok: true, text: `Published “${s.artifact.label}”. Share it with projects to make it usable there.` });
      setEverything(true);
      void client.invalidateQueries({ queryKey: ['library'] });
    },
    onError: fail,
  });

  const items = library.data?.pages.flatMap((p) => p.items ?? []) ?? [];
  const publishable = (saved.data?.items ?? []).filter((a) => a.kind === 'configuration' || a.kind === 'fixture' || a.kind === 'scenario');
  const canCopy = project !== LIBRARY && !writeDenied;
  return (
    <>
      <SavedHead />
      <div className="od-page">
        <div className="saved-stack" style={{ maxWidth: 1200 }}>
          {!publishDenied && (
            <div className="audit-filters">
              <div className="od-field">
                <label htmlFor="library-show">Show</label>
                <select id="library-show" className="od-select" value={everything ? 'all' : 'project'} onChange={(e) => setEverything(e.target.value === 'all')}>
                  <option value="project">Shared with this project</option>
                  <option value="all">The whole library</option>
                </select>
              </div>
              <div className="od-field">
                <label htmlFor="library-publish">Publish from this project</label>
                <select id="library-publish" className="od-select" value={publishing} onChange={(e) => setPublishing(e.target.value)}>
                  <option value="">Choose saved work…</option>
                  {publishable.map((a) => (
                    <option key={a.id} value={a.id}>
                      {kindLabel(a.kind)}: {a.label}
                    </option>
                  ))}
                </select>
              </div>
              <button type="button" className="od-btn" disabled={!publishing || publish.isPending} onClick={() => publish.mutate(publishing)}>
                {publish.isPending ? 'Publishing…' : 'Publish'}
              </button>
            </div>
          )}
          {notice && (
            <div className={`od-callout ${notice.ok ? '' : 'od-callout--warn'}`} role={notice.ok ? 'status' : 'alert'}>
              <Icon name={notice.ok ? 'circle-check' : 'triangle-alert'} size={16} />
              <div className="od-callout__main">
                <div className="od-callout__body">{notice.text}</div>
              </div>
            </div>
          )}
          {library.isPending && <div className="od-empty" role="status"><div className="od-empty__title">Loading the library…</div></div>}
          {library.error && <Problem title="The library could not be read" error={library.error} retry={() => void library.refetch()} />}
          {library.data && items.length === 0 && (
            <div className="od-empty">
              <div className="od-empty__title">{whole ? 'The library is empty' : 'Nothing is shared with this project yet'}</div>
              <div className="od-empty__body">An admin publishes captured or derived fixtures, scenarios and configurations here and shares them with projects.</div>
            </div>
          )}
          {items.length > 0 && (
            <div className="od-card" style={{ overflow: 'auto' }}>
              <table className="od-table">
                <thead>
                  <tr><th>Kind</th><th>Label</th><th>Shared with</th><th>Updated</th><th /></tr>
                </thead>
                <tbody>
                  {items.map((item) => {
                    const a = item.artifact;
                    const editing = sharing?.id === a.id;
                    return (
                      <tr key={a.id}>
                        <td>{kindLabel(a.kind)}</td>
                        <td>
                          <Link to="/saved/$artifactId" params={{ artifactId: a.id }}>{a.label}</Link>
                        </td>
                        <td>
                          {!editing && (
                            <span title={item.shared_by ? `Shared ${item.shared ? whenText(item.shared) : ''} by ${item.shared_by}` : undefined}>{audienceText(item.audience, list)}</span>
                          )}
                          {editing && (
                            <fieldset className="od-field" aria-label={`Share ${a.label}`} style={{ border: 0, padding: 0, margin: 0, display: 'flex', flexWrap: 'wrap', gap: 8 }}>
                              {[{ id: ALL_PROJECTS, name: 'Every project' }, ...audienceChoices].map((p) => (
                                <label key={p.id} style={{ display: 'inline-flex', gap: 4, alignItems: 'center' }}>
                                  <input type="checkbox" checked={sharing.audience.includes(p.id)} onChange={() => setSharing({ id: a.id, audience: toggleShare(sharing.audience, p.id) })} />
                                  {p.name}
                                </label>
                              ))}
                            </fieldset>
                          )}
                        </td>
                        <td className="od-subtle" title={a.updated}>{whenText(a.updated)}</td>
                        <td>
                          <span style={{ display: 'inline-flex', gap: 4 }}>
                            {canCopy && !editing && (
                              <button type="button" className="od-btn od-btn--sm od-btn--ghost" disabled={copy.isPending} title="Copy this item into the current project as saved work of its own" onClick={() => copy.mutate(item)}>
                                Copy into project
                              </button>
                            )}
                            {!publishDenied && !editing && (
                              <button type="button" className="od-btn od-btn--sm od-btn--ghost" disabled={!!sharing} title={sharing ? 'Finish the open audience first' : undefined} onClick={() => setSharing({ id: a.id, audience: [...(item.audience ?? [])] })}>
                                Share…
                              </button>
                            )}
                            {editing && (
                              <>
                                <button type="button" className="od-btn od-btn--sm od-btn--primary" disabled={share.isPending} onClick={() => share.mutate(sharing)}>
                                  {share.isPending ? 'Saving…' : 'Save audience'}
                                </button>
                                <button type="button" className="od-btn od-btn--sm" onClick={() => setSharing(undefined)}>Cancel</button>
                              </>
                            )}
                          </span>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
          {library.hasNextPage && (
            <button type="button" className="od-btn" disabled={library.isFetchingNextPage} onClick={() => void library.fetchNextPage()}>
              {library.isFetchingNextPage ? 'Loading…' : 'Load more'}
            </button>
          )}
        </div>
      </div>
    </>
  );
}
