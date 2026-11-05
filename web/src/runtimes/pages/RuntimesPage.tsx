import { useCallback, useEffect, useRef, useState } from 'react';
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Link, useRouter, useSearch } from '@tanstack/react-router';
import { api } from '../../api/client';
import type { RuntimeDefault } from '../../api/types';
import { Icon } from '../../ui/Icon';
import { getWorkspace } from '../../workspace/store';
import { browserStorage, rebuildSeed, type FormSeed } from '../buildMemory';
import { RuntimeForm } from '../RuntimeForm';
import { ImportForm, PushForm } from '../RuntimeForms';
import { defaultRuntimeQuery, runtimesQuery, supportQuery } from '../RuntimeSelect';
import { DEFAULT_REMOVE_REASON, DELETE_CATALOG_WARNING, DELETE_RUNTIME_WARNING, finished, makeDefaultQuestion, problemOf } from '../runtimes';
import { buildTime } from './buildView';
import { fromOf, rebuildOf, returnTarget, setupTitle, withFrom } from './returnPath';
import { buildChip, canBeDefault, runtimeDetail, runtimeRows, selectedRow, type RuntimeRow } from './runtimeList';
import { BuiltInComponents, RuntimeComponents } from './RuntimeComponents';
import { RuntimesHead } from './RuntimesHead';
import { StatePill } from './StatePill';
import { useDenial } from '../../ui/session';
import { RuntimeAvailability, RuntimeShareForm } from './RuntimeShare';

type Panel = 'build' | 'import';

/** CLI_STEPS builds from a terminal: the same job, queued on this server's builders and waited for. */
export const CLI_STEPS = 'linea runtime build --server URL --name NAME --manifest ocb.yaml [--source MODULE=DIR]';

/** REPLACE_FORM asks before a rebuild seed replaces the runtime being written. */
export const REPLACE_FORM = 'Replace the runtime you are writing with this one? What you wrote in the build form is discarded.';

/** BUILDS_PAGE is how many builds the list shows at a time. */
const BUILDS_PAGE = 10;

/**
 * RuntimesPage is the runtime list with the selected runtime beside it. It
 * builds runtimes on the server's builders, lists the builds and imports
 * images; it never chooses the runtime of the workspace, that stays with the
 * selector beside Run in the debugger. Its one choice is the team's default
 * runtime, which requests that name no runtime run on. When opened from the
 * debugger (`from`) it offers the way back to the experiment. `rebuild` opens
 * the builder from an earlier build.
 */
export function RuntimesPage() {
  const router = useRouter();
  const search = useSearch({ strict: false });
  const from = fromOf(search);
  const rebuild = rebuildOf(search);
  const target = returnTarget(from);
  const runtimes = useQuery(runtimesQuery);
  const team = useQuery(defaultRuntimeQuery);
  const support = useQuery(supportQuery);
  const builtIn = support.data?.built_in;
  const manageDenied = useDenial('runtime.manage');
  const buildDenied = useDenial('runtime.build');
  const rows = runtimeRows(runtimes.data, builtIn, team.data?.name);
  const [selection, setSelection] = useState<string>();
  const row = selectedRow(rows, selection);
  const [panel, setPanel] = useState<Panel>();
  // Panels stay mounted once opened, so closing one never discards a draft.
  const [opened, setOpened] = useState<Set<Panel>>(new Set());
  const [seed, setSeed] = useState<FormSeed>();
  const [dirty, setDirty] = useState(false);
  const [replacing, setReplacing] = useState<FormSeed>();
  const open = (p?: Panel) => {
    setPanel(p);
    if (p) setOpened((s) => new Set(s).add(p));
  };
  const toggle = (p: Panel) => open(panel === p ? undefined : p);
  const failure = runtimes.error ? problemOf(runtimes.error) : undefined;
  const onBuilt = useCallback((id: string) => router.history.push(withFrom(`/runtimes/builds/${encodeURIComponent(id)}`, from)), [router, from]);
  // A rebuild link opens the builder seeded from that build, once per link. If
  // the form already holds something being written, the seed waits behind the
  // inline confirmation instead of replacing it.
  const seeded = useRef<string | undefined>(undefined);
  const earlier = useQuery({ queryKey: ['runtime-build', rebuild], queryFn: () => api.runtimeBuild(rebuild!), enabled: !!rebuild, retry: false });
  useEffect(() => {
    if (!rebuild || !earlier.data || seeded.current === rebuild) return;
    seeded.current = rebuild;
    const next = rebuildSeed(earlier.data, browserStorage(), (seed?.key ?? 0) + 1);
    setOpened((s) => new Set(s).add('build'));
    setPanel('build');
    if (dirty) setReplacing(next);
    else setSeed(next);
  }, [rebuild, earlier.data, dirty, seed?.key]);
  const draftPath = getWorkspace().configPath;
  const title = target ? setupTitle(target, draftPath) : undefined;

  return (
    <>
      <RuntimesHead
        actions={(
          <>
            <button type="button" className="od-btn od-btn--sm" aria-pressed={panel === 'import'} disabled={!!manageDenied} title={manageDenied} onClick={() => toggle('import')}>
              <Icon name="upload" size={14} /><span>Import image</span>
            </button>
            <button type="button" className="od-btn od-btn--primary od-btn--sm" aria-pressed={panel === 'build'} disabled={!!buildDenied} title={buildDenied} onClick={() => toggle('build')}>
              <Icon name="hammer" size={14} /><span>Build a runtime</span>
            </button>
          </>
        )}
      />
      {target && title && (
        <div className="od-callout od-callout--info od-callout--bar" role="status">
          <Icon name="info" size={16} />
          <div className="od-callout__main">
            <span className="od-callout__title">{title.before}{title.draft && <> <code>{title.draft}</code></>}</span>
            <div className="od-callout__body">When you are done, return and choose the runtime beside Run.</div>
          </div>
          <div className="od-callout__actions">
            <button type="button" className="od-btn od-btn--primary od-btn--sm" onClick={() => router.history.push(target.href)}>
              <Icon name="arrow-left" size={14} /><span>{target.label}</span>
            </button>
          </div>
        </div>
      )}
      <div className="rt-split">
        <div className="od-page rt-split__main">
          {opened.has('build') && (
            <section className="od-card rt-panel" hidden={panel !== 'build'} aria-label="Build a runtime">
              <div className="od-card__head">
                <h2 className="od-card__title">Build a runtime</h2>
                <span className="od-subtle rt-note">compiled on this server&apos;s builders; builds never delay runs</span>
                <span className="rt-grow" />
                <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={() => open(undefined)}>Close</button>
              </div>
              <div className="od-card__body">
                {replacing && (
                  <div className="od-confirm" role="alertdialog" aria-label="Replace the runtime being written">
                    <Icon name="triangle-alert" size={16} />
                    <span className="od-confirm__text">{REPLACE_FORM}</span>
                    <button type="button" className="od-btn od-btn--sm" onClick={() => setReplacing(undefined)}>Keep writing</button>
                    <button type="button" className="od-btn od-btn--primary od-btn--sm" onClick={() => { setSeed(replacing); setReplacing(undefined); }}>Replace</button>
                  </div>
                )}
                <RuntimeForm onBuilt={onBuilt} seed={seed} onDirty={setDirty} />
                <p className="od-subtle rt-note">
                  A finished build registers the runtime under its name; it runs on the pool workers with no further step.
                  From a terminal: <code>{CLI_STEPS}</code> (replace <code>URL</code> with this server&apos;s address).
                </p>
              </div>
            </section>
          )}
          {opened.has('import') && (
            <section className="od-card rt-panel" hidden={panel !== 'import'} aria-label="Import an image">
              <div className="od-card__head">
                <h2 className="od-card__title">Import an image</h2>
                <span className="rt-grow" />
                <button type="button" className="od-btn od-btn--ghost od-btn--sm" onClick={() => open(undefined)}>Close</button>
              </div>
              <div className="od-card__body"><ImportForm /></div>
            </section>
          )}

          {failure && (
            <div className="od-callout od-callout--warn" role="alert">
              <Icon name="triangle-alert" size={16} />
              <div className="od-callout__main">
                <span className="od-callout__title">Built runtimes are not available</span>
                <div className="od-callout__body">{failure.message}</div>
              </div>
            </div>
          )}
          {runtimes.isPending && <p className="od-muted" role="status">Loading runtimes…</p>}

          <div className="od-card rt-card">
            <table className="od-table">
              <thead>
                <tr><th>Runtime</th><th>Type</th><th>Version</th><th>State</th></tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr key={r.key} aria-selected={r.key === row.key} onClick={() => setSelection(r.key)}>
                    <td className="mono">
                      <button type="button" className="rt-rowbtn" aria-label={`Show ${r.name} ${r.version}`} onClick={() => setSelection(r.key)}>{r.name}</button>
                      {r.isDefault && <span className="od-chip od-chip--accent rt-default">Default</span>}
                    </td>
                    <td>{r.type}</td>
                    <td className="mono">{r.version}</td>
                    <td><StatePill chip={r.state} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          <Builds from={from} />

          <div className="od-muted rt-hint">
            <Icon name="info" size={14} />
            Runtimes are chosen beside Run in the debugger. Here you build one, import an image, or make one the team&apos;s default.
          </div>
        </div>

        <aside className="od-pane rt-detail" aria-label="Runtime">
          <div className="od-pane__head"><span className="od-pane__title">Runtime</span></div>
          <div className="od-pane__body rt-detail__body">
            <Detail row={row} builtIn={builtIn} team={team.data} from={from} />
          </div>
        </aside>
      </div>
    </>
  );
}

/**
 * Builds lists the runtime builds, newest first, ten at a time. The caller sees
 * their own; runtime.manage sees all. While one has not finished the list is
 * read every 3 s, so its chip follows it.
 */
function Builds({ from }: { from: string | undefined }) {
  const builds = useInfiniteQuery({
    queryKey: ['runtime-builds', 'page'],
    queryFn: ({ pageParam }) => api.runtimeBuilds({ limit: BUILDS_PAGE, cursor: pageParam || undefined }),
    initialPageParam: '',
    getNextPageParam: (last) => last.next_cursor || undefined,
    retry: false,
    refetchInterval: (q) => (q.state.data?.pages.some((p) => p.builds?.some((b) => !finished(b.state))) ? 3000 : false),
    refetchIntervalInBackground: false,
  });
  const list = builds.data?.pages.flatMap((p) => p.builds ?? []) ?? [];
  if (builds.error) {
    return (
      <section className="od-card rt-card" aria-label="Builds">
        <div className="od-card__head"><h2 className="od-card__title">Builds</h2></div>
        <div className="od-card__body od-muted rt-note" role="alert">Builds could not be listed: {problemOf(builds.error, true).message}</div>
      </section>
    );
  }
  if (list.length === 0) return null;
  return (
    <section className="od-card rt-card" aria-label="Builds">
      <div className="od-card__head"><h2 className="od-card__title">Builds</h2></div>
      <table className="od-table od-table--dense">
        <tbody>
          {list.map((b) => (
            <tr key={b.id}>
              <td className="mono">
                <Link to="/runtimes/builds/$buildId" params={{ buildId: b.id }} search={from ? ({ from } as never) : undefined}>{b.name} {b.collector_version}</Link>
              </td>
              <td><StatePill chip={buildChip(b)} />{b.reused && <span className="od-subtle rt-note"> reused</span>}</td>
              <td className="od-subtle">{buildTime(b)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {builds.hasNextPage && (
        <div className="od-pager">
          <button type="button" className="od-btn od-btn--ghost od-btn--sm" disabled={builds.isFetchingNextPage} onClick={() => void builds.fetchNextPage()}>
            {builds.isFetchingNextPage ? 'Loading…' : 'Show older builds'}
          </button>
        </div>
      )}
    </section>
  );
}

function Detail({ row, builtIn, team, from }: { row: RuntimeRow; builtIn: string | undefined; team: RuntimeDefault | undefined; from: string | undefined }) {
  const d = runtimeDetail(row, builtIn, team);
  const build = row.runtime?.catalog?.build;
  return (
    <>
      <div>
        <div className="t-code-strong">{d.title}</div>
        <div className="od-muted rt-note">{d.subtitle}</div>
      </div>
      <dl className="od-kv">
        {d.facts.map(([k, v]) => <FactRow key={k} k={k} v={v} />)}
      </dl>
      {row.kind === 'built-in' && builtIn && <BuiltInComponents version={builtIn} />}
      {row.runtime?.manifest && (
        <RuntimeComponents key={row.key} manifest={row.runtime.manifest} version={row.runtime.collector_version} verified={row.runtime.verified} />
      )}
      {d.providers.length > 0 && (
        <>
          <div className="od-section-label">Providers</div>
          <div className="rt-chips">{d.providers.map((p) => <span key={p} className="od-chip od-chip--plain">{p}</span>)}</div>
        </>
      )}
      {d.custom.length > 0 && (
        <>
          <div className="od-section-label">Custom modules</div>
          <div className="rt-chips">{d.custom.map((c) => <span key={c} className="od-chip od-chip--accent">{c}</span>)}</div>
        </>
      )}
      {build && (
        <div>
          <Link className="od-btn od-btn--sm" to="/runtimes/builds/$buildId" params={{ buildId: build }} search={from ? ({ from } as never) : undefined}>
            <Icon name="arrow-right" size={14} /><span>Open its build</span>
          </Link>
        </div>
      )}
      {(row.kind === 'built-in' || row.runtime) && <RuntimeActions key={row.key} row={row} />}
    </>
  );
}

function FactRow({ k, v }: { k: string; v: string }) {
  return (
    <>
      <dt>{k}</dt>
      <dd>{v}</dd>
    </>
  );
}

/**
 * RuntimeActions pushes and removes an imported runtime, removes a catalog
 * runtime (built on the server, or registered with linea runtime register,
 * which has no manifest or image here) and makes the built-in or a catalog
 * runtime the team's default; removing and making default ask for an inline
 * confirmation. The default runtime cannot be removed. Making a default is
 * the page's one choice of a runtime: it changes what requests that name no
 * runtime run on, never the selection of this workspace.
 */
function RuntimeActions({ row }: { row: RuntimeRow }) {
  const qc = useQueryClient();
  const r = row.runtime;
  const [confirming, setConfirming] = useState(false);
  const [pushing, setPushing] = useState(false);
  const [making, setMaking] = useState(false);
  const [sharing, setSharing] = useState(false);
  const catalog = row.kind === 'catalog';
  const manageDenied = useDenial('runtime.manage');
  const remove = useMutation({
    mutationFn: () => api.deleteRuntime(r!.name),
    onSuccess: () => { setConfirming(false); void qc.invalidateQueries({ queryKey: ['runtimes'] }); },
  });
  const makeDefault = useMutation({
    mutationFn: () => api.setDefaultRuntime(row.name),
    onSuccess: () => {
      setMaking(false);
      void qc.invalidateQueries({ queryKey: ['runtime-default'] });
      void qc.invalidateQueries({ queryKey: ['runtimes'] });
      // Validations and previews that named no runtime ran on the old default.
      void qc.invalidateQueries({ queryKey: ['validate'] });
      void qc.invalidateQueries({ queryKey: ['ottl-playground'] });
    },
  });
  if (making) {
    return (
      <>
        <div className="od-confirm" role="alertdialog" aria-label="Make default runtime">
          <Icon name="triangle-alert" size={16} />
          <span className="od-confirm__text">{makeDefaultQuestion(row.name)}</span>
          <button type="button" className="od-btn od-btn--sm" onClick={() => { setMaking(false); makeDefault.reset(); }}>Cancel</button>
          <button type="button" className="od-btn od-btn--primary od-btn--sm" disabled={makeDefault.isPending} onClick={() => makeDefault.mutate()}>Confirm</button>
        </div>
        {makeDefault.error && <div className="od-field__error" role="alert">{problemOf(makeDefault.error).message}</div>}
      </>
    );
  }
  if (confirming) {
    return (
      <div className="od-confirm" role="alertdialog" aria-label="Remove runtime">
        <Icon name="triangle-alert" size={16} />
        <span className="od-confirm__text">{catalog ? DELETE_CATALOG_WARNING : DELETE_RUNTIME_WARNING}</span>
        <button type="button" className="od-btn od-btn--sm" onClick={() => { setConfirming(false); remove.reset(); }}>Keep</button>
        <button type="button" className="od-btn od-btn--danger od-btn--sm" disabled={remove.isPending} onClick={() => remove.mutate()}>
          Remove
        </button>
      </div>
    );
  }
  if (pushing && r) return <PushForm name={r.name} onDone={() => setPushing(false)} />;
  if (sharing) return <RuntimeShareForm name={row.name} projects={r?.projects} onDone={() => setSharing(false)} />;
  return (
    <>
      <div className="od-subtle">Available to <RuntimeAvailability projects={r?.projects} /></div>
      <div className="rt-actions">
        {(r || row.kind === 'built-in') && (
          <button type="button" className="od-btn od-btn--sm" disabled={!!manageDenied} title={manageDenied ?? 'Choose the projects whose runs may use this runtime'} onClick={() => setSharing(true)}>
            <Icon name="folder-open" size={14} /><span>Share…</span>
          </button>
        )}
        {canBeDefault(row) && (
          <button type="button" className="od-btn od-btn--sm" disabled={!!manageDenied} title={manageDenied ?? 'Run requests that name no runtime on this one, for the whole team'} onClick={() => { makeDefault.reset(); setMaking(true); }}>
            <Icon name="check" size={14} /><span>Make default</span>
          </button>
        )}
        {r && !catalog && (
          <button type="button" className="od-btn od-btn--sm" disabled={!!manageDenied} title={manageDenied ?? "Push this runtime's image to a registry, for CI or other machines"} onClick={() => setPushing(true)}>
            <Icon name="upload" size={14} /><span>Push…</span>
          </button>
        )}
        {r && (
          <button type="button" className="od-btn od-btn--sm" disabled={!!manageDenied || row.isDefault} title={manageDenied ?? (row.isDefault ? DEFAULT_REMOVE_REASON : undefined)} onClick={() => { remove.reset(); setConfirming(true); }}>
            <Icon name="trash-2" size={14} /><span>Remove…</span>
          </button>
        )}
      </div>
      {remove.error && <div className="od-field__error" role="alert">{problemOf(remove.error).message}</div>}
    </>
  );
}
