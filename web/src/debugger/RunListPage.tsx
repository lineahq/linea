import { useEffect, useMemo, useRef, useState } from 'react';
import { Link, useNavigate, useSearch } from '@tanstack/react-router';
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';
import { Icon } from '../ui/Icon';
import { Crumbs } from '../ui/PageHead';
import { comparePair, finished } from '../runs/compare';
import { canDelete, deleteWarning } from '../runs/deletion';
import { runListQuery, runPagesQuery } from '../runs/runList';
import { dedupeRuns, firstPageRuns, flattenRunPages, runsCountText } from '../runs/runPages';
import { FactChip, FactPlaceholder } from './FactChip';
import { filterRuns, modesOf, NO_FILTER, RESULTS, runFacts, runLabel, runtimesOf, scenariosOf, serverFilter, toggleSelection, type RunFilter } from './runFacts';
import { filterFromSearch, searchFromFilter, type RunListSearch } from './runListSearch';
import { useDenial } from '../ui/session';

/**
 * RunListPage (`/runs`) lists every retained run, newest first, with the
 * platform's summary of each finished run. Two runs can be selected to
 * compare, and finished runs deleted after an inline confirmation. The server
 * returns the list a page at a time: the next page loads when the end of the
 * list scrolls into view, and "Load more" does the same by hand. The loaded
 * pages refresh every 5 s while the page is visible. The server filters by
 * scenario, runtime and result; the search and the mode apply in this browser
 * to the pages loaded so far. All five filters live in the URL.
 */
export function RunListPage() {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const search = useSearch({ strict: false }) as RunListSearch;
  const { scenario, runtime, result, mode, q } = search;
  const filter = useMemo(() => filterFromSearch({ scenario, runtime, result, mode, q }), [scenario, runtime, result, mode, q]);
  const server = serverFilter(filter);
  const filtered = Object.keys(server).length > 0;
  // The newest page of the unfiltered list names the scenarios and runtimes the filters offer.
  const unfiltered = useQuery(runListQuery);
  // A new filter is a new query key, so paging restarts from the first page.
  const runs = useInfiniteQuery(runPagesQuery(server));
  const [selected, setSelected] = useState<string[]>([]);
  const [confirming, setConfirming] = useState(false);
  const [failed, setFailed] = useState<string>();

  const known = useMemo(() => firstPageRuns(unfiltered.data), [unfiltered.data]);
  const all = useMemo(() => flattenRunPages(runs.data?.pages), [runs.data]);
  const matching = useMemo(() => filterRuns(all, filter), [all, filter]);
  const rows = useMemo(() => matching.map((r) => runFacts(r)), [matching]);
  const more = !!runs.hasNextPage;

  // Selection survives filtering and loading more, but never keeps a deleted run.
  const pool = useMemo(() => dedupeRuns([...all, ...known]), [all, known]);
  const chosen = selected.filter((id) => pool.some((r) => r.id === id));
  const chosenRuns = chosen.map((id) => pool.find((r) => r.id === id)!);
  const pair = chosen.length === 2 && chosenRuns.every(finished) ? comparePair(pool, chosen[0], chosen[1]) : undefined;
  const deletable = chosenRuns.length > 0 && chosenRuns.every((r) => canDelete(r));

  const deleteDenied = useDenial('run.delete');
  const remove = useMutation({
    mutationFn: async (ids: string[]) => {
      const results = await Promise.allSettled(ids.map((id) => api.deleteRun(id).then(() => id)));
      const gone = results.flatMap((r) => (r.status === 'fulfilled' ? [r.value] : []));
      const errors = results.flatMap((r) => (r.status === 'rejected' ? [r.reason instanceof Error ? r.reason.message : String(r.reason)] : []));
      return { gone, errors };
    },
    onSuccess: ({ gone, errors }) => {
      setConfirming(false);
      for (const id of gone) qc.removeQueries({ queryKey: ['run', id] });
      setSelected((s) => s.filter((id) => !gone.includes(id)));
      setFailed(errors.length ? `${errors.length} run${errors.length === 1 ? '' : 's'} could not be deleted: ${errors[0]}` : undefined);
      void qc.invalidateQueries({ queryKey: ['runs'] });
    },
  });

  // Filters replace the history entry, so Back leaves the list instead of undoing keystrokes.
  const setFilter = (next: RunFilter) => void navigate({ search: searchFromFilter(next) as never, replace: true });
  const change = (patch: Partial<RunFilter>) => setFilter({ ...filter, ...patch });
  const toggle = (id: string) => { setSelected(toggleSelection(chosen, id)); setConfirming(false); setFailed(undefined); };
  const firstTwo = matching.slice(0, 2);
  const allSelected = firstTwo.length > 0 && firstTwo.every((r) => chosen.includes(r.id));
  const selectFirstTwo = () => {
    // At most two runs can be selected, so "select all" clears a full selection and otherwise selects the first two.
    setSelected(allSelected ? [] : firstTwo.map((r) => r.id));
    setConfirming(false);
  };

  // The next page loads when the end of the list scrolls into view. Observing anew after each page
  // makes a list that is still short enough to show its end load the following page too.
  const end = useRef<HTMLDivElement>(null);
  const { fetchNextPage, isFetchingNextPage, isFetchNextPageError } = runs;
  useEffect(() => {
    const node = end.current;
    if (!node || !more || isFetchingNextPage || isFetchNextPageError || typeof IntersectionObserver === 'undefined') return;
    const watch = new IntersectionObserver((seen) => { if (seen.some((e) => e.isIntersecting)) void fetchNextPage(); }, { rootMargin: '200px' });
    watch.observe(node);
    return () => watch.disconnect();
  }, [more, isFetchingNextPage, isFetchNextPageError, fetchNextPage, all.length, matching.length]);

  return (
    <>
      <header className="od-wbhead">
        <Crumbs items={[{ label: 'Debugger', to: '/' }, { label: 'All runs' }]} />
        <span style={{ flex: 1 }} />
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <Link to="/runs/storage" className="od-btn od-btn--ghost" title="What the run history holds, the retention policy and pruning">
            <Icon name="layers" size={16} />
            <span>Storage</span>
          </Link>
          <Link to="/" className="od-btn od-btn--ghost">
            <Icon name="arrow-left" size={16} />
            <span>Back to the debugger</span>
          </Link>
        </div>
      </header>
      <div className="od-page">
        <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
            <div className="od-search" style={{ width: 320 }}>
              <Icon name="search" size={14} />
              <input aria-label="Search runs" placeholder="Run id, scenario, runtime, result" value={filter.query} onChange={(e) => change({ query: e.target.value })} />
            </div>
            <FilterSelect label="Scenario" value={filter.scenario} options={withCurrent(scenariosOf(known), filter.scenario).map((s) => [s, s])} onChange={(scenario) => change({ scenario })} />
            <FilterSelect label="Runtime" value={filter.runtime} options={withCurrent(runtimesOf(known), filter.runtime).map((r) => [r, r])} onChange={(runtime) => change({ runtime })} />
            <FilterSelect label="Result" value={filter.result} options={RESULTS} onChange={(result) => change({ result })} />
            <FilterSelect label="Mode" value={filter.mode} options={withCurrent(modesOf(known), filter.mode).map((m) => [m, m])} onChange={(mode) => change({ mode })} />
            <span style={{ flex: 1 }} />
            <span className="od-subtle" style={{ fontSize: 12 }}>
              {runsCountText(matching.length, more, filtered || matching.length !== all.length)}
            </span>
          </div>

          {chosen.length > 0 && (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 8, padding: '8px 12px', border: '1px solid var(--accent-border)', background: 'var(--accent-subtle)', borderRadius: 'var(--radius-lg)' }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
                <span className="t-body-strong">{chosen.length} run{chosen.length === 1 ? '' : 's'} selected</span>
                <span className="od-muted" style={{ fontSize: 12.5 }}>
                  {pair ? `${runLabel(pair.before)} is the baseline (older), ${runLabel(pair.after)} the candidate`
                    : chosen.length === 1 ? 'Select one more run to compare'
                    : 'A run that has not finished cannot be compared'}
                </span>
                <span style={{ flex: 1 }} />
                <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={() => { setSelected([]); setConfirming(false); }}>Clear</button>
                <button type="button" className="od-btn od-btn--danger od-btn--sm" disabled={!deletable || remove.isPending || !!deleteDenied} aria-expanded={confirming}
                  title={deleteDenied ?? (deletable ? 'Delete the selected runs and their recorded evidence' : 'Only finished runs can be deleted; cancel a running one first')}
                  onClick={() => { remove.reset(); setConfirming(true); }}>
                  <Icon name="trash-2" size={14} />
                  <span>Delete</span>
                </button>
                <button type="button" className="od-btn od-btn--primary od-btn--sm" disabled={!pair}
                  onClick={() => pair && void navigate({ to: '/', search: { run: pair.after, compare: pair.before } })}>
                  <Icon name="git-compare" size={14} />
                  <span>Compare {chosen.length} runs</span>
                </button>
              </div>
              {confirming && (
                <div className="confirm-inline" role="alertdialog" aria-label="Delete runs" style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
                  <span style={{ fontSize: 12.5 }}>{deleteWarning(chosen.length)}</span>
                  <button type="button" className="od-btn od-btn--danger od-btn--sm" disabled={remove.isPending} onClick={() => remove.mutate(chosen)}>
                    <span>{chosen.length === 1 ? 'Delete run' : `Delete ${chosen.length} runs`}</span>
                  </button>
                  <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={() => { setConfirming(false); remove.reset(); }}><span>Keep</span></button>
                </div>
              )}
            </div>
          )}
          {(failed || remove.error) && (
            <div className="od-callout od-callout--fail" role="alert">
              <Icon name="octagon-alert" size={16} />
              <div className="od-callout__main"><div className="od-callout__title">{failed ?? `Could not delete: ${remove.error!.message}`}</div></div>
            </div>
          )}

          {runs.error && !runs.data ? (
            <div className="od-callout od-callout--fail" role="alert">
              <Icon name="octagon-alert" size={16} />
              <div className="od-callout__main">
                <div className="od-callout__title">Could not load the runs</div>
                <div className="od-callout__body">{runs.error.message}</div>
              </div>
            </div>
          ) : runs.isPending ? (
            <div className="od-empty" role="status"><div className="od-empty__body">Loading runs…</div></div>
          ) : !all.length && !filtered ? (
            <div className="od-empty">
              <div className="od-empty__title">No runs yet</div>
              <div className="od-empty__body">Run an input or a scenario in the debugger; every run is kept here until you delete it.</div>
              <Link to="/" className="od-btn od-btn--primary od-btn--sm"><span>Open the debugger</span></Link>
            </div>
          ) : !matching.length ? (
            <div className="od-empty" role="status">
              <div className="od-empty__title">{more ? 'No loaded run matches' : 'No run matches'}</div>
              <div style={{ display: 'flex', gap: 8 }}>
                {more && <LoadMore loading={isFetchingNextPage} onClick={() => void fetchNextPage()} />}
                <button type="button" className="od-btn od-btn--sm" onClick={() => setFilter(NO_FILTER)}><span>Clear filters</span></button>
              </div>
            </div>
          ) : (
            <div className="od-card" style={{ overflow: 'hidden' }}>
              <table className="od-table">
                <thead>
                  <tr>
                    <th className="cb"><input type="checkbox" aria-label="Select the first two runs" checked={allSelected} onChange={selectFirstTwo} /></th>
                    <th>Run</th><th>When</th><th>Scenario</th><th>Runtime</th><th>Execution</th><th>Evidence</th><th>Assertions</th><th aria-label="Open run" />
                  </tr>
                </thead>
                <tbody>
                  {rows.map((f) => {
                    const on = chosen.includes(f.id);
                    return (
                      <tr key={f.id} aria-selected={on}>
                        <td className="cb"><input type="checkbox" aria-label={`Select run ${f.label}`} checked={on} onChange={() => toggle(f.id)} /></td>
                        <td className="mono" title={f.id}>{f.label}{f.mode && <span className="od-subtle"> · {f.mode}</span>}</td>
                        <td className="od-subtle">{f.when}</td>
                        <td className="mono">{f.pending ? <FactPlaceholder /> : f.scenario ?? <span className="od-subtle">—</span>}</td>
                        <td className="mono">{f.pending ? <FactPlaceholder /> : f.runtime ?? <span className="od-subtle">—</span>}</td>
                        <td><FactChip fact={f.execution} label="Execution" /></td>
                        <td>{f.evidence ? <FactChip fact={f.evidence} label="Evidence" /> : <FactPlaceholder />}</td>
                        <td>{f.assertions ? <FactChip fact={f.assertions} label="Assertions" /> : <FactPlaceholder />}</td>
                        <td style={{ textAlign: 'right' }}>
                          <Link to="/" search={{ run: f.id }} className="od-btn od-btn--link od-btn--sm" aria-label={`Investigate run ${f.label}`}>
                            <span>Investigate</span>
                            <Icon name="arrow-right" size={14} />
                          </Link>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
              <div className="od-pager">
                <span>{matching.length === all.length ? `${all.length} loaded` : `${matching.length} of ${all.length} loaded match`}{more ? ', older runs follow' : ', no older runs'}</span>
                {more && <LoadMore loading={isFetchingNextPage} onClick={() => void fetchNextPage()} />}
              </div>
              <div ref={end} aria-hidden="true" />
              {runs.isFetchNextPageError && (
                <div className="od-callout od-callout--fail" role="alert">
                  <Icon name="octagon-alert" size={16} />
                  <div className="od-callout__main"><div className="od-callout__title">Could not load more runs: {runs.error?.message}</div></div>
                </div>
              )}
            </div>
          )}
        </div>
      </div>
    </>
  );
}

/** withCurrent keeps a chosen value among the options after the runs that had it are gone. */
function withCurrent(options: string[], current: string): string[] {
  return current && !options.includes(current) ? [...options, current] : options;
}

function FilterSelect({ label, value, options, onChange }: { label: string; value: string; options: readonly (readonly [string, string])[]; onChange(value: string): void }) {
  return (
    <label className="od-subtle" style={{ fontSize: 12 }}>
      {label}{' '}
      <select className="od-select" value={value} onChange={(e) => onChange(e.target.value)} disabled={!options.length}>
        <option value="">any</option>
        {options.map(([v, text]) => <option key={v} value={v}>{text}</option>)}
      </select>
    </label>
  );
}

/** LoadMore is the keyboard and pointer way to load the next page, beside the scroll trigger. */
function LoadMore({ loading, onClick }: { loading: boolean; onClick(): void }) {
  return (
    <button type="button" className="od-btn od-btn--sm" disabled={loading} onClick={onClick}>
      <span>{loading ? 'Loading…' : 'Load more'}</span>
    </button>
  );
}
