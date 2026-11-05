import { useEffect, useMemo, useRef, useState } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import type { ComposedManifest, ManifestRequest, RetargetedManifest } from '../api/types';
import { useDenial } from '../ui/session';
import { getWorkspace } from '../workspace/store';
import { browserStorage, holdFolders, rememberBuild, type FormSeed } from './buildMemory';
import { inspectFolder, localModules, prepareBuild, sourceProblems, sourceStatus, type PickedFolder } from './buildSources';
import { CatalogPicker } from './CatalogPicker';
import { catalogKeysOf, manifestRequest, STARTS, supportedKeys, type Start } from './catalogPicks';
import { filesOfInput, folderOfDrop, type PickedFiles } from './readFolder';
import { supportQuery } from './RuntimeSelect';
import { defaultName, nameProblem, problemOf, UNVERIFIED } from './runtimes';
import { MAX_BUILD_BYTES, megabytes } from './tar';
import { retargetNotes } from './upgrade';

export const TRUST_NOTE = 'Custom component code is compiled on a builder and runs in the runtime on the pool workers with whatever they allow; build only sources you trust.';

/** REBUILD_NOTE says what building under an existing name does. */
export const REBUILD_NOTE = 'Building under the same name replaces the runtime once the build succeeds. Runs already queued keep the binary they were queued with.';

/** SOURCE_HINT shows how a manifest names a module the build takes from a folder. */
export const SOURCE_HINT = 'processors:\n  - gomod: example.com/myprocessor v0.0.0\n    path: ./myprocessor';

/**
 * RuntimeForm writes a manifest from a release's catalog, or imports one, then
 * queues a build of it on the team server's builders. A start (the standard
 * set, the current configuration, all components or none) preselects its
 * components in the picker, where they are added to or removed. The manifest
 * stays editable text. A module the manifest builds from a local directory
 * (`path:` or a replace) gets a folder slot: the folder is packed in the
 * browser and uploaded with the build, since the server reads nothing from
 * the user's machine. A seed (Rebuild) fills it with what an earlier build
 * was started from. `onDirty` reports whether it holds a manifest or folders
 * being written, so a caller can ask before a seed replaces them.
 */
export function RuntimeForm({ onBuilt, seed, onDirty }: { onBuilt(id: string): void; seed?: FormSeed; onDirty?(dirty: boolean): void }) {
  const support = useQuery(supportQuery);
  const newest = support.data?.verified?.at(-1) ?? support.data?.built_in ?? '';
  const [version, setVersion] = useState('');
  const release = version || newest;
  const verified = (support.data?.verified ?? []).includes(release);
  const [start, setStart] = useState<Start>('built-in');
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [manifest, setManifest] = useState('');
  const [composed, setComposed] = useState<ComposedManifest>();
  const [name, setName] = useState('');
  const [folders, setFolders] = useState<Record<string, PickedFolder | undefined>>({});
  const [problems, setProblems] = useState<string[]>([]);
  const section = useRef<HTMLElement>(null);
  const buildDenied = useDenial('runtime.build');
  const builtIn = support.data?.built_in ?? '';
  const picking = start !== 'import' && verified;
  const catalog = useQuery({
    queryKey: ['collector-catalog', release],
    queryFn: () => api.collectorCatalog(release),
    enabled: picking,
    staleTime: Infinity,
  });
  // The standard set is the built-in runtime's manifest; another release
  // selects the same modules from its own catalog.
  const standard = useQuery({
    queryKey: ['manifest-start', 'built-in', builtIn],
    queryFn: () => api.composeManifest({ version: builtIn, preset: 'built-in' }),
    enabled: picking && start === 'built-in' && !!builtIn,
    staleTime: Infinity,
  });
  const [configYAML, setConfigYAML] = useState('');
  const used = useQuery({
    queryKey: ['manifest-start', 'config', release, configYAML],
    queryFn: () => api.composeManifest({ version: release, config_yaml: configYAML }),
    enabled: picking && start === 'config' && configYAML.trim() !== '',
    staleTime: Infinity,
  });
  const template = useMemo((): { keys: string[]; missing: string[] } | undefined => {
    if (!catalog.data || start === 'import') return undefined;
    if (start === 'pick') return { keys: [], missing: [] };
    if (start === 'all') return { keys: supportedKeys(catalog.data), missing: [] };
    const from = start === 'built-in' ? standard.data : used.data;
    if (!from) return undefined;
    const m = catalogKeysOf(from.manifest, catalog.data);
    return { keys: m.keys, missing: [...m.missing, ...(from.unknown ?? [])] };
  }, [catalog.data, start, standard.data, used.data]);
  // A start, or another release, selects its components once they are known;
  // picks of another release's catalog do not carry over.
  const applied = useRef('');
  useEffect(() => {
    const id = `${start}|${release}|${configYAML}`;
    if (!template || applied.current === id) return;
    applied.current = id;
    setPicked(new Set(template.keys));
  }, [template, start, release, configYAML]);
  const choose = (s: Start) => {
    setStart(s);
    setPicked(new Set());
    applied.current = '';
    if (s === 'config') setConfigYAML(getWorkspace().configYAML);
  };
  const startLabel = STARTS.find((s) => s.id === start)!.label;
  const startError = (start === 'built-in' && standard.error) || (start === 'config' && used.error) || catalog.error;
  const noConfig = start === 'config' && !configYAML.trim();
  const loading = !picking || noConfig || startError ? undefined
    : catalog.isPending ? 'Reading the catalog…'
      : !template ? `Selecting the ${startLabel.toLowerCase()}…` : undefined;

  const compose = useMutation({
    mutationFn: (req: ManifestRequest) => api.composeManifest(req),
    onSuccess: (m) => { setComposed(m); setManifest(m.manifest); },
  });
  const [moved, setMoved] = useState<RetargetedManifest>();
  const retarget = useMutation({
    mutationFn: () => api.retargetManifest({ version: release, manifest_yaml: manifest }),
    onSuccess: (m) => { setComposed(undefined); setMoved(m); setManifest(m.manifest); },
  });
  const modules = useMemo(() => localModules(manifest), [manifest]);
  const build = useMutation({
    mutationFn: async (o: { name: string }) => {
      const prepared = prepareBuild({ name: o.name, manifest, modules, folders });
      const b = modules.length ? await api.uploadRuntimeBuild(prepared.request, prepared.archives) : await api.createRuntimeBuild(prepared.request);
      rememberBuild(browserStorage(), b.id, { name: o.name, manifest, version: release });
      holdFolders(b.id, Object.fromEntries(modules.flatMap((m) => (folders[m] ? [[m, folders[m]!]] : []))));
      return b;
    },
    onSuccess: (b) => onBuilt(b.id),
  });
  const { reset: resetBuild } = build;
  useEffect(() => {
    if (!seed) return;
    setVersion(seed.version ?? '');
    setStart('import');
    applied.current = '';
    setManifest(seed.manifest);
    setComposed(undefined);
    setMoved(undefined);
    setName(seed.name);
    setFolders(seed.folders ?? {});
    setProblems([]);
    resetBuild();
    section.current?.scrollIntoView?.({ block: 'start' });
  }, [seed, resetBuild]);
  const rebuilding = !!seed && name === seed.name;
  const dirty = manifest.trim() !== '' || Object.values(folders).some(Boolean);
  useEffect(() => onDirty?.(dirty), [dirty, onDirty]);

  const write = () => {
    setComposed(undefined);
    compose.mutate(manifestRequest({ start, release, builtIn, picked, template: template?.keys }));
  };
  const submit = () => {
    const runtimeName = name || defaultName(release);
    const nameIssue = nameProblem(runtimeName);
    const all = [...(nameIssue ? [nameIssue] : []), ...(manifest.trim() ? [] : ['Write or import a manifest first.']), ...sourceProblems(modules, folders)];
    setProblems(all);
    if (all.length) return;
    build.mutate({ name: runtimeName });
  };
  const failure = build.error ? problemOf(build.error, true) : undefined;
  const composeFailure = compose.error ? problemOf(compose.error) : undefined;

  return (
    <section className="runtime-form" aria-label="New runtime" ref={section}>
      <h4>{rebuilding ? <>Rebuild <code>{seed.name}</code></> : 'New runtime'}</h4>
      {rebuilding && <p className="note">{REBUILD_NOTE}</p>}
      {rebuilding && !seed.remembered && (seed.manifest
        ? <p className="note">This browser did not start that build. The manifest is the one it was sent with; pick any local folders again.</p>
        : <p className="note lim-warning">This browser did not start that build, so its manifest is not known here. Write or import the manifest again.</p>
      )}
      {buildDenied && <p className="note lim-warning" role="status">{buildDenied}. You can prepare a manifest here, but not build it.</p>}
      <div className="row">
        <label>
          Collector release{' '}
          <input list="collector-releases" value={version} placeholder={newest} size={10}
            onChange={(e) => setVersion(e.target.value.trim())} aria-label="Collector release" />
          <datalist id="collector-releases">
            {(support.data?.verified ?? []).map((v) => <option key={v} value={v} />)}
          </datalist>
        </label>
        {release && !verified && (
          <span className="badge unverified" title={UNVERIFIED}>unverified</span>
        )}
        {support.data && <span className="muted small">From {support.data.oldest} on; verified: {(support.data.verified ?? []).join(', ')}.</span>}
      </div>
      {release && !verified && <p className="note lim-warning">{UNVERIFIED} Only an imported manifest builds it; catalogs exist for verified releases.</p>}

      <fieldset className="runtime-starts">
        <legend>Start from</legend>
        <div className="runtime-starts__grid" role="radiogroup" aria-label="Start from">
          {STARTS.map((s) => (
            <label key={s.id} className={`od-radio-card${start === s.id ? ' od-radio-card--on' : ''}`}>
              <input type="radio" name="runtime-start" checked={start === s.id} onChange={() => choose(s.id)} aria-label={s.label} />
              <span>
                <strong>{s.label}</strong>
                <br />
                <span className="od-subtle">{s.note}</span>
              </span>
            </label>
          ))}
        </div>
      </fieldset>

      {picking && (
        <>
          {noConfig && <p className="note lim-warning">No configuration is open: open one, or start from another set.</p>}
          {startError && <p className="error">{problemOf(startError).message}</p>}
          {template && template.missing.length > 0 && (
            <p className="note lim-warning">
              {start === 'config' ? 'The configuration uses components' : `The ${startLabel.toLowerCase()} has components`} the {release} catalog does not have (add their modules to the manifest with a folder):{' '}
              {template.missing.map((m) => <code key={m}>{m} </code>)}
            </p>
          )}
          <CatalogPicker catalog={catalog.data} picked={picked} onPicked={setPicked} loading={loading}
            template={start !== 'pick' && template ? { label: startLabel, keys: template.keys } : undefined} />
        </>
      )}

      {start === 'import' ? (
        <label className="file-button">
          Open manifest file…
          <input type="file" accept=".yaml,.yml" hidden onChange={async (e) => {
            const f = e.target.files?.[0];
            if (f) { setComposed(undefined); setManifest(await f.text()); }
          }} />
        </label>
      ) : (
        <button type="button" disabled={!verified || compose.isPending || !picked.size} onClick={write}>
          Write manifest
        </button>
      )}
      {composeFailure && <p className="error">{composeFailure.message}</p>}
      {composed?.unknown?.length ? (
        <p className="note lim-warning">Not in the catalog (add their modules yourself): {composed.unknown.map((u) => <code key={u}>{u} </code>)}</p>
      ) : null}
      {(composed?.warnings ?? []).map((w, i) => <p key={i} className="note lim-warning">{w}</p>)}

      <label className="stacked">
        Manifest (OCB)
        <textarea aria-label="Manifest" value={manifest} rows={10} spellCheck={false}
          placeholder={start === 'import' ? 'Paste an OCB manifest, or open a file' : 'Write a manifest from the catalog, or paste one'}
          onChange={(e) => { setMoved(undefined); setManifest(e.target.value); }} />
      </label>
      <div className="row">
        <button type="button" disabled={!verified || !manifest.trim() || retarget.isPending} onClick={() => retarget.mutate()}
          title="Change the manifest's core and contrib module versions to the chosen release; the rest of the text stays as written">
          Move to {release || 'release'}
        </button>
      </div>
      {retarget.error && (
        <div className="error">
          <p>{problemOf(retarget.error).message}</p>
          {problemOf(retarget.error).details.map((d, i) => <p key={i}>{d}</p>)}
        </div>
      )}
      {moved && retargetNotes(moved).map((n, i) => <p key={i} className={n.warn ? 'note lim-warning' : 'note'}>{n.text}</p>)}

      <fieldset className="source-slots">
        <legend>Your own components</legend>
        {modules.length === 0 ? (
          <>
            <p className="muted small">
              A module the manifest builds from a local directory (a <code>path:</code> entry, or a replace with a directory) gets a folder to choose here;
              the folder is packed in your browser and uploaded with the build. Add your component to the manifest like this, then choose its folder. {TRUST_NOTE}
            </p>
            <pre className="od-code rt-cli" aria-label="A manifest entry for a local module"><code>{SOURCE_HINT}</code></pre>
          </>
        ) : (
          <>
            <p className="muted small">
              The manifest builds {modules.length === 1 ? 'this module' : 'these modules'} from a local directory. Choose the folder that holds each one&apos;s <code>go.mod</code>;
              at most {megabytes(MAX_BUILD_BYTES)} in all, and <code>.git</code> and <code>node_modules</code> are left out. {TRUST_NOTE}
            </p>
            {modules.map((m) => (
              <SourceSlot key={m} module={m} folder={folders[m]} onChange={(f) => setFolders((cur) => ({ ...cur, [m]: f }))} />
            ))}
          </>
        )}
      </fieldset>

      <div className="row">
        <label>Name <input value={name} placeholder={defaultName(release)} onChange={(e) => setName(e.target.value.trim())} aria-label="Runtime name" /></label>
        <button type="button" className="primary" disabled={build.isPending || !!buildDenied} title={buildDenied} onClick={submit}>
          {build.isPending ? (modules.length ? 'Uploading…' : 'Queuing…') : 'Build'}
        </button>
      </div>
      {problems.map((p, i) => <p key={i} className="error">{p}</p>)}
      {failure && (
        <div className="error" role="alert">
          <p>{failure.message}</p>
          {failure.details.length > 0 && <ul>{failure.details.map((d, i) => <li key={i}>{d}</li>)}</ul>}
        </div>
      )}
    </section>
  );
}

/**
 * SourceSlot takes the folder of one module, from the directory picker or a
 * drop. It checks the folder as it arrives (safe paths, bounds, a go.mod that
 * declares the module) and says what it found.
 */
function SourceSlot({ module, folder, onChange }: { module: string; folder: PickedFolder | undefined; onChange(f: PickedFolder | undefined): void }) {
  const [reading, setReading] = useState(false);
  const [error, setError] = useState<string>();
  const [over, setOver] = useState(false);
  const input = useRef<HTMLInputElement>(null);
  const take = async (read: () => Promise<PickedFiles | undefined>) => {
    setReading(true);
    setError(undefined);
    try {
      const got = await read();
      if (!got) throw new Error('This browser did not say where the files are; choose the folder again or drop it.');
      onChange(await inspectFolder(got.name, module, got.files));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setReading(false);
    }
  };
  return (
    <div
      className={`source-slot${over ? ' source-slot--over' : ''}`}
      role="group"
      aria-label={`Folder of ${module}`}
      onDragOver={(e) => { e.preventDefault(); setOver(true); }}
      onDragLeave={() => setOver(false)}
      onDrop={(e) => {
        e.preventDefault();
        setOver(false);
        void take(() => folderOfDrop(Array.from(e.dataTransfer.items) as never));
      }}
    >
      <div className="source-slot__head">
        <code>{module}</code>
        <button type="button" onClick={() => input.current?.click()} disabled={reading}>{folder ? 'Choose another…' : 'Choose folder…'}</button>
        {folder && <button type="button" onClick={() => { onChange(undefined); setError(undefined); }} aria-label={`Clear the folder of ${module}`}>×</button>}
        <input ref={input} type="file" hidden aria-label={`Folder picker for ${module}`}
          {...({ webkitdirectory: '', directory: '' } as Record<string, string>)}
          onChange={(e) => {
            const files = e.target.files;
            if (files?.length) void take(async () => filesOfInput(Array.from(files)));
            e.target.value = '';
          }} />
      </div>
      <div className="small muted" role="status">{reading ? 'Reading the folder…' : sourceStatus(folder)}{!folder && !reading && ' · or drop it here'}</div>
      {folder?.problems.map((p, i) => <p key={i} className="error small">{p}</p>)}
      {error && <p className="error small">{error}</p>}
    </div>
  );
}
