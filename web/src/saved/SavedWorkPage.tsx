import { useEffect, useMemo, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Link, useNavigate } from '@tanstack/react-router';
import { savedRoute, type SavedSearch } from '../router';
import { api } from '../api/client';
import type { ArtifactMetadata } from '../api/types';
import { openBinaryFile } from '../files';
import { Icon } from '../ui/Icon';
import {
  FILTERS,
  filterCounts,
  filterItems,
  isRunnable,
  isTest,
  PROFILE_LIMIT,
  paginate,
  profileTargets,
  scenarioScope,
  selectedMembers,
  setSelected,
  toggleSelected,
  unscopedScenarios,
  type KindFilter,
  type Profiles,
} from './collection';
import { bytesToBase64, importSummary } from './ciArchive';
import { kindLabel, plural, whenText } from './format';
import { canOpenAsDraft } from './openDraft';
import { NoWorkspace } from './NoWorkspace';
import { SavedHead } from './SavedHead';
import { SuiteLauncher } from './SuiteLauncher';
import { noWorkspace, useProfiles, useSavedArtifacts } from './useSavedArtifacts';
import { useOpenAsDraft } from './useOpenAsDraft';
import { useDenial } from '../ui/session';

const NO_PROFILES: Profiles = new Map();

/**
 * SavedWorkPage is the searchable collection of everything saved on the
 * server. Tests are the scenarios with assertions; selecting
 * scenarios offers to run them as a suite or export them for CI, and Import
 * saves a CI archive's files back.
 */
export function SavedWorkPage() {
  const saved = useSavedArtifacts();
  const search = savedRoute.useSearch();
  const navigate = useNavigate({ from: '/saved' });
  const filter: KindFilter = FILTERS.find((f) => f.id === search.kind)?.id ?? 'all';
  const query = search.q ?? '';
  const setFilter = (kind: KindFilter) => navigate({ search: (s: SavedSearch) => ({ ...s, kind: kind === 'all' ? undefined : kind }), replace: true });
  const setQuery = (q: string) => navigate({ search: (s: SavedSearch) => ({ ...s, q: q || undefined }), replace: true });
  const [page, setPage] = useState(1);
  const [selected, setSelectedIds] = useState<ReadonlySet<string>>(new Set());
  const draft = useOpenAsDraft();
  const qc = useQueryClient();
  const writeDenied = useDenial('artifact.write');
  const imported = useMutation({
    mutationFn: async () => {
      const file = await openBinaryFile('.zip,application/zip');
      return file ? api.importArtifacts(bytesToBase64(file.bytes)) : undefined;
    },
    onSuccess: (imp) => {
      if (imp) void qc.invalidateQueries({ queryKey: ['saved'] });
    },
  });
  const importButton = (
    <button type="button" className="od-btn" disabled={imported.isPending || !!writeDenied} title={writeDenied ?? 'Save the configuration and scenarios of a CI archive exported from Saved work as new artifacts or revisions'} onClick={() => imported.mutate()}>
      <Icon name="file-plus" size={14} /><span>{imported.isPending ? 'Importing…' : 'Import…'}</span>
    </button>
  );

  const items = useMemo(() => saved.data?.items ?? [], [saved.data]);

  // Which head revisions to read: the scenarios (to know the tests), and the
  // rows on the page (for their origin). Without profiles the page shows
  // exactly the rows the kind filter and search choose.
  const provisional = useMemo(() => paginate(filterItems(items, filter === 'test' ? 'scenario' : filter, query, NO_PROFILES), page).rows, [items, filter, query, page]);
  const targets = useMemo(() => profileTargets(items, provisional), [items, provisional]);
  const profiles = useProfiles(targets);

  const counts = useMemo(() => filterCounts(items, profiles), [items, profiles]);
  const listed = useMemo(() => filterItems(items, filter, query, profiles), [items, filter, query, profiles]);
  const view = paginate(listed, page);
  const configs = useMemo(() => items.filter((m) => m.kind === 'configuration'), [items]);
  const members = useMemo(() => selectedMembers(items, selected), [items, selected]);

  const reading = scenarioScope(items).filter((m) => !profiles.has(m.id)).length;
  const unscoped = unscopedScenarios(items);

  if (saved.isPending) {
    return (
      <>
        <SavedHead />
        <div className="od-page"><div className="od-empty" role="status"><div className="od-empty__title">Loading saved work…</div></div></div>
      </>
    );
  }
  if (saved.error) {
    return (
      <>
        <SavedHead />
        <div className="od-page">
          {noWorkspace(saved.error) ? <NoWorkspace /> : (
            <div className="od-callout od-callout--fail" role="alert">
              <Icon name="circle-x" size={16} />
              <div className="od-callout__main">
                <div className="od-callout__title">Saved work could not be read</div>
                <div className="od-callout__body">{saved.error.message}</div>
                <div className="od-callout__actions">
                  <button type="button" className="od-btn od-btn--sm" onClick={() => void saved.refetch()}>Try again</button>
                </div>
              </div>
            </div>
          )}
        </div>
      </>
    );
  }

  const pageScenarios = view.rows.filter(isRunnable);
  const allOnPage = pageScenarios.length > 0 && pageScenarios.every((m) => selected.has(m.id));
  const someOnPage = pageScenarios.some((m) => selected.has(m.id));

  return (
    <>
      <SavedHead actions={importButton} />
      <div className="od-page">
        <div className="saved-stack">
          {imported.isError && (
            <div className="od-callout od-callout--fail" role="alert">
              <Icon name="circle-x" size={16} />
              <div className="od-callout__main">
                <div className="od-callout__title">The archive was not imported</div>
                <div className="od-callout__body">{imported.error instanceof Error ? imported.error.message : String(imported.error)}</div>
              </div>
            </div>
          )}
          {imported.data && (
            <div className="od-callout od-callout--info" role="status">
              <Icon name="circle-check" size={16} />
              <div className="od-callout__main"><div className="od-callout__body">{importSummary(imported.data)}</div></div>
            </div>
          )}
          <div className="saved-row">
            <div className="od-search saved-search">
              <Icon name="search" size={14} />
              <input
                aria-label="Search saved work"
                placeholder="Name or ID"
                value={query}
                onChange={(e) => {
                  setQuery(e.target.value);
                  setPage(1);
                }}
              />
            </div>
            <span className="od-muted" style={{ fontSize: 12.5 }}>Tests are scenarios with assertions.</span>
            <span className="saved-grow" />
            <span className="od-subtle" style={{ fontSize: 12 }}>Saved on this machine · revisions are immutable</span>
          </div>

          <div className="od-tabs" role="tablist" aria-label="Kind">
            {FILTERS.filter((f) => f.id !== 'setup' || (counts.setup ?? 0) > 0).map((f) => (
              <button
                key={f.id}
                type="button"
                role="tab"
                className="od-tab"
                aria-selected={filter === f.id}
                onClick={() => {
                  setFilter(f.id);
                  setPage(1);
                }}
              >
                {f.label}
                {counts[f.id] !== undefined && <span className="od-tab__count">{counts[f.id]}</span>}
              </button>
            ))}
          </div>

          {(reading > 0 || unscoped > 0) && (
            <div className="od-muted" style={{ fontSize: 12 }} role="status">
              {reading > 0 && `Reading ${plural(reading, 'scenario')} to find the tests… `}
              {unscoped > 0 && `Tests are found among the ${PROFILE_LIMIT} most recently updated scenarios; ${unscoped} older ${unscoped === 1 ? 'one is' : 'ones are'} not checked.`}
            </div>
          )}
          {saved.data?.truncated && (
            <div className="od-callout od-callout--warn" role="status">
              <Icon name="triangle-alert" size={16} />
              <div className="od-callout__main"><div className="od-callout__body">Only the first {items.length} saved items are listed.</div></div>
            </div>
          )}

          {members.length > 0 && (
            <SuiteLauncher
              members={members}
              configs={configs}
              heading={`${plural(members.length, filter === 'test' ? 'test' : 'scenario')} selected`}
              onClear={() => setSelectedIds(new Set())}
            />
          )}
          {draft.confirm}
          {draft.error && (
            <div className="od-callout od-callout--fail" role="alert">
              <Icon name="circle-x" size={16} />
              <div className="od-callout__main"><div className="od-callout__body">The revision could not be opened: {draft.error}</div></div>
            </div>
          )}

          {items.length === 0 ? (
            <div className="od-card">
              <div className="od-empty">
                <div className="od-empty__glyph"><Icon name="library" size={18} /></div>
                <div className="od-empty__title">Nothing saved yet</div>
                <div className="od-empty__body">Save a configuration or scenario from the debugger and it appears here with its revisions.</div>
                <div className="od-empty__actions"><Link to="/" className="od-btn">Open the debugger</Link></div>
              </div>
            </div>
          ) : (
            <div className="od-card" style={{ overflow: 'hidden' }} role="tabpanel" aria-label={FILTERS.find((f) => f.id === filter)?.label}>
              {view.total === 0 ? (
                <div className="od-empty">
                  <div className="od-empty__title">{query.trim() ? 'No saved work matches' : `No ${FILTERS.find((f) => f.id === filter)?.label.toLowerCase()}`}</div>
                  <div className="od-empty__body">{query.trim() ? 'Try a different name, or another kind.' : filter === 'test' ? 'A test is a scenario with at least one assertion.' : 'Nothing of this kind is saved.'}</div>
                </div>
              ) : (
                <table className="od-table">
                  <thead>
                    <tr>
                      <th className="cb">
                        <SelectAll checked={allOnPage} indeterminate={someOnPage && !allOnPage} disabled={pageScenarios.length === 0} onChange={(on) => setSelectedIds(setSelected(selected, pageScenarios.map((m) => m.id), on))} />
                      </th>
                      <th>Name</th>
                      <th>Kind</th>
                      <th className="num">Revision</th>
                      <th>Updated</th>
                      <th>Origin</th>
                      <th><span className="sr-only">Actions</span></th>
                    </tr>
                  </thead>
                  <tbody>
                    {view.rows.map((m) => (
                      <Row key={m.id} m={m} profiles={profiles} selected={selected.has(m.id)} busy={draft.busy} onToggle={() => setSelectedIds(toggleSelected(selected, m.id))} onOpen={() => void draft.open(m)} />
                    ))}
                  </tbody>
                </table>
              )}
              <div className="od-pager">
                <span>{view.total === 0 ? '0 items' : `${view.from}–${view.to} of ${plural(view.total, 'item')}`}</span>
                <span className="saved-pager-actions">
                  <span>25 per page</span>
                  <button type="button" className="od-btn od-btn--sm" disabled={view.page <= 1} onClick={() => setPage(view.page - 1)}>
                    <Icon name="chevron-left" size={14} /><span>Previous</span>
                  </button>
                  <button type="button" className="od-btn od-btn--sm" disabled={view.page >= view.pages} onClick={() => setPage(view.page + 1)}>
                    <span>Next</span><Icon name="chevron-right" size={14} />
                  </button>
                </span>
              </div>
            </div>
          )}
        </div>
      </div>
    </>
  );
}

function Row({ m, profiles, selected, busy, onToggle, onOpen }: { m: ArtifactMetadata; profiles: Profiles; selected: boolean; busy: boolean; onToggle(): void; onOpen(): void }) {
  const profile = profiles.get(m.id);
  const test = isTest(m, profiles);
  const assertions = profile?.assertions;
  return (
    <tr aria-selected={selected}>
      <td className="cb">
        {isRunnable(m) && <input type="checkbox" aria-label={`Select ${m.label || m.id}`} checked={selected} onChange={onToggle} />}
      </td>
      <td>
        <span className="saved-name">
          <Icon name={m.kind === 'scenario' ? 'flask-conical' : m.kind === 'configuration' ? 'file-code' : m.kind === 'manifest' ? 'package' : 'file-text'} size={16} className="od-subtle" />
          <Link to="/saved/$artifactId" params={{ artifactId: m.id }}><code>{m.label || m.id}</code></Link>
        </span>
      </td>
      <td className="od-subtle">
        {kindLabel(m.kind)}
        {test && ` · test${assertions && assertions > 1 ? ` (${assertions})` : ''}`}
      </td>
      <td className="num">rev {m.generation}</td>
      <td className="od-subtle" title={m.updated}>{whenText(m.updated)}</td>
      <td className="od-subtle">{profile?.origin ?? '…'}</td>
      <td style={{ textAlign: 'right' }}>
        {canOpenAsDraft(m.kind) && (
          <button type="button" className="od-btn od-btn--link od-btn--sm" disabled={busy} onClick={onOpen}>
            <span>Open</span>
          </button>
        )}
      </td>
    </tr>
  );
}

function SelectAll({ checked, indeterminate, disabled, onChange }: { checked: boolean; indeterminate: boolean; disabled: boolean; onChange(on: boolean): void }) {
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (ref.current) ref.current.indeterminate = indeterminate;
  }, [indeterminate]);
  return <input ref={ref} type="checkbox" aria-label="Select all scenarios on this page" checked={checked} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />;
}
