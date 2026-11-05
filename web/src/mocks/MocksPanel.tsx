import { useMemo, useState } from 'react';
import type { Diagnostic, Graph, ReportProvider, TopologyComponent } from '../api/types';
import { openTextFile } from '../files';
import {
  addFile,
  addKubeObjects,
  editScenario,
  readScenario,
  removeMock,
  removeProvider,
  setEnvironment,
  setField,
  setFileText,
  setProviderSettings,
  addProviderSettings,
  type Edit,
  type FileModel,
} from '../scenario/scenarioDoc';
import { CommitInput, Menu, TextBlock, type MenuItem } from '../scenario/fields';
import {
  duplicateKubeObject,
  extractedKeys,
  FIELD_LABELS,
  fieldsOf,
  FILE_TEMPLATES,
  fileTemplate,
  kubeObjectYAML,
  kubeSummary,
  OBJECT_TEMPLATES,
  objectTemplate,
  readKubeForms,
  replaceKubeObject,
  setKubeField,
  setKubeMap,
  type Entries,
  type FileTemplate,
  type KubeForm,
  type ObjectTemplate,
} from './objects';
import { proposalText, proposals, providerCards, providerGaps, providersToAdd, type ProviderCard } from './providers';
import { componentConfig, envReferences, mockedComponents, providerOf, suggestFile, suggestKube, type EnvRef } from './suggest';

interface Props {
  /** Scenario or quick-run setup text holding mocks and environment. */
  yaml: string;
  onChange(yaml: string): void;
  configYAML: string;
  topology?: Graph;
  /** The effective configuration, for suggestions. */
  effective?: unknown;
  /** The inputs' OTLP JSON payloads, for suggestions. */
  payloads: unknown[];
  /** Missing-fixture and unreadable-setup diagnostics. */
  diagnostics: Diagnostic[];
  /** Sets the sender address of inputs without one, for association by connection. */
  setClientAddress(address: string): void;
  /** The selected runtime's module providers; undefined while unknown. */
  providers?: ReportProvider[];
  /** The selected runtime's name; empty for the built-in one. */
  runtime: string;
}

const MODE_TEXT: Record<string, string> = {
  simulated: 'Your input is injected here; nothing listens on a port.',
  captured: 'Output is recorded here; nothing is sent.',
  real: 'Runs as configured.',
  unsupported: 'Not supported by the debugger.',
};

type Obj = Record<string, unknown>;

/**
 * MocksPanel edits what components that read their environment get
 * instead: files for filelog, objects for k8sattributes and ${env:NAME}
 * values. The real component still runs. One "+ Mock" menu adds any kind,
 * with suggestions for this configuration first; each mock is a card, and
 * Kubernetes objects open to a form (or their YAML). Groups appear once
 * they have entries, and say whether anything in the configuration reads
 * them.
 */
export function MocksPanel(p: Props) {
  const [problem, setProblem] = useState<string>();
  const [pasting, setPasting] = useState(false);
  const [addingEnv, setAddingEnv] = useState(false);
  const [addingProvider, setAddingProvider] = useState(false);
  const [openObject, setOpenObject] = useState<number>();
  const [notes, setNotes] = useState<string[]>();
  const read = readScenario(p.yaml);
  const forms = useMemo(() => readKubeForms(p.yaml), [p.yaml]);
  const env = useMemo(() => envReferences(p.configYAML), [p.configYAML]);
  const apply = (e: Edit) => {
    const r = editScenario(p.yaml, e);
    if ('error' in r) {
      setProblem(r.error);
      return false;
    }
    setProblem(undefined);
    p.onChange(r.yaml);
    return true;
  };
  const mocked = mockedComponents(p.topology);
  const fileReaders = mocked.filter((m) => m.provider === 'files').map((m) => m.component);
  const kubeReaders = mocked.filter((m) => m.provider === 'kubernetes').map((m) => m.component);
  const gaps = (cs: TopologyComponent[]) => p.diagnostics.filter((d) => d.code === 'mock_fixture_missing' && cs.some((c) => c.ref === d.component));
  const unreadable = p.diagnostics.filter((d) => !d.component);
  // Simulated receivers, captured exporters, and mocked components that need
  // no fixtures (the spanmetrics connector's simulated clock).
  const automatic = (p.topology?.components ?? []).filter(
    (c) => c.mode === 'simulated' || c.mode === 'captured' || (c.mode === 'dependency_mocked' && !providerOf(c) && !c.provider),
  );

  if ('error' in read) {
    return (
      <div className="scenario-form mocks-panel">
        <p className="error">The scenario YAML cannot be read: {read.error}. Fix it in the YAML tab.</p>
      </div>
    );
  }
  const m = read.model;
  const set = new Map(m.environment);
  const missingEnv = env.filter((r) => !set.has(r.name) && !r.hasDefault);
  const kubeCfg = kubeReaders[0] ? componentConfig(p.effective, kubeReaders[0]) : {};
  const total = m.files.length + m.kubeObjects.length + m.kubeFiles.length + m.environment.length + m.providers.length;
  const cards = providerCards(m.providers, p.providers, p.runtime);
  const toAdd = providersToAdd(p.providers, m.providers);
  const providerFindings = providerGaps(
    p.diagnostics,
    [...fileReaders, ...kubeReaders].map((c) => c.ref),
  );
  const proposed = proposals(providerFindings);
  const addSettings = (name: string) => apply(addProviderSettings(name, proposed.get(name) ?? {}));

  const addFileFrom = (id: FileTemplate, cfg: Obj) => {
    const f = fileTemplate(
      id,
      cfg,
      m.files.map((x) => x.path),
    );
    apply(addFile(f.path, f.content));
  };
  const addExampleFile = (c: TopologyComponent) => {
    const s = suggestFile(
      componentConfig(p.effective, c),
      m.files.map((x) => x.path),
    );
    apply(addFile(s.path, s.content));
  };
  const addObject = (kind: ObjectTemplate) => {
    if (apply(addKubeObjects(objectTemplate(kind, forms)))) setOpenObject(forms.length);
  };
  const suggestPods = (c?: TopologyComponent) => {
    const s = suggestKube(c ? componentConfig(p.effective, c) : {}, p.payloads, m.inputs.find((i) => i.clientAddress)?.clientAddress ?? '');
    if (!apply(addKubeObjects(s.manifests))) return;
    if (s.clientAddress) p.setClientAddress(s.clientAddress);
    setNotes(s.notes);
  };
  const addMissingEnv = () => apply(setEnvironment([...m.environment, ...missingEnv.map((r): [string, string] => [r.name, ''])]));
  const openManifests = async () => {
    const f = await openTextFile('.yaml,.yml,.json');
    if (f) apply(addKubeObjects(f.text));
  };

  // Suggestions built from this configuration come first.
  const forConfig: MenuItem[] = [
    ...fileReaders.map((c) => ({
      label: (
        <>
          Example file for <code>{c.id}</code>
        </>
      ),
      detail: `At a path its include patterns match, in the format it parses`,
      onSelect: () => addExampleFile(c),
    })),
    ...kubeReaders.map((c) => ({
      label: (
        <>
          Pods for the input · <code>{c.id}</code>
        </>
      ),
      detail: "Matched by the input's k8s.* attributes or sender address, with the labels it extracts",
      onSelect: () => suggestPods(c),
    })),
    ...(missingEnv.length
      ? [
          {
            label: `Missing variables: ${missingEnv.map((r) => r.name).join(', ')}`,
            detail: 'The configuration reads them and has no default',
            onSelect: addMissingEnv,
          },
        ]
      : []),
  ];
  const menu: MenuItem[] = [
    ...forConfig.map((it, i) => ({ ...it, group: i === 0 ? 'For this configuration' : undefined })),
    ...FILE_TEMPLATES.map((t, i) => ({
      group: i === 0 ? 'Files (filelog)' : undefined,
      label: t.label,
      detail: t.detail,
      onSelect: () => addFileFrom(t.id, fileReaders[0] ? componentConfig(p.effective, fileReaders[0]) : {}),
    })),
    ...OBJECT_TEMPLATES.map((t, i) => ({
      group: i === 0 ? 'Kubernetes (k8sattributes)' : undefined,
      label: t.kind,
      detail: t.detail,
      onSelect: () => addObject(t.kind),
    })),
    { label: 'Paste manifests…', detail: 'kubectl get -o yaml output, one object or many', onSelect: () => setPasting(true) },
    { label: 'Open manifests file…', detail: 'Read a YAML file as inline objects', onSelect: openManifests },
    { group: 'Environment', label: 'Variable', detail: 'A value for ${env:NAME}', onSelect: () => setAddingEnv(true) },
    ...toAdd.map((pr, i) => ({
      group: i === 0 ? 'Module providers' : undefined,
      label: (
        <>
          Settings for <code>{pr.name}</code>
        </>
      ),
      detail: proposed.has(pr.name) ? `mocks.providers.${pr.name}, starting from what ${pr.name} proposes` : `mocks.providers.${pr.name}, read by ${pr.package}`,
      onSelect: () => addSettings(pr.name),
    })),
    {
      group: toAdd.length ? undefined : 'Module providers',
      label: 'Provider by name…',
      detail: 'Settings for a provider the selected runtime does not list',
      onSelect: () => setAddingProvider(true),
    },
  ];
  const readers = [...fileReaders, ...kubeReaders];
  const showFiles = m.files.length > 0 || gaps(fileReaders).length > 0;
  const showKube = m.kubeObjects.length + m.kubeFiles.length > 0 || gaps(kubeReaders).length > 0 || pasting;
  const showEnv = m.environment.length > 0 || env.length > 0 || addingEnv;
  const driven = (p.topology?.components ?? []).filter((c) => c.provider);
  const showProviders = cards.length > 0 || providerFindings.length > 0 || driven.length > 0 || addingProvider;

  return (
    <div className="scenario-form mocks-panel">
      <div className="builder-bar">
        <h3>Mocks</h3>
        {total > 0 && <span className="tab-count plain">{total}</span>}
        <span className="grow" />
        <Menu label="+ Mock ▾" className="primary-outline" items={menu} align="right" />
      </div>
      <p className="lede">
        Components that read their environment run for real against these stand-ins, never your filesystem, cluster or shell. Mocks apply to every run of this
        document and are saved with it.
      </p>
      {problem && <p className="error">{problem}</p>}
      {unreadable.map((d, i) => (
        <p key={i} className="error">
          {d.message}
        </p>
      ))}

      {total === 0 && !pasting && !addingEnv && !showProviders && (
        <MocksStart
          readers={readers}
          forConfig={forConfig}
          missingEnv={missingEnv}
          onFile={(id) => addFileFrom(id, fileReaders[0] ? componentConfig(p.effective, fileReaders[0]) : {})}
          onObject={addObject}
        />
      )}

      {showFiles && (
        <Group title="Files" readers={fileReaders} unread="No filelog receiver in this configuration reads these files." gaps={gaps(fileReaders)}>
          <p className="hint">
            Read from an isolated directory at their deployment paths
            {fileReaders.map((c) => {
              const include = componentConfig(p.effective, c).include;
              return Array.isArray(include) && include.length ? (
                <span key={c.ref}>
                  {' '}
                  · <code>{c.id}</code> includes <code>{include.join(', ')}</code>
                </span>
              ) : null;
            })}
            .
          </p>
          {m.files.map((f, i) => (
            <FileCard key={`${i}|${f.path}`} index={i} file={f} apply={apply} />
          ))}
        </Group>
      )}

      {showKube && (
        <Group
          title="Kubernetes objects"
          readers={kubeReaders}
          unread="No k8sattributes processor in this configuration queries these objects."
          gaps={gaps(kubeReaders)}
        >
          <p className="hint">Served by an in-process Kubernetes API, never a cluster.</p>
          {notes && (
            <ul className="notes">
              {notes.map((n, i) => (
                <li key={i}>{n}</li>
              ))}
            </ul>
          )}
          {m.kubeFiles.map((f, i) => (
            <div key={`f|${i}`} className="mock-card">
              <div className="mock-card-head">
                <span className="badge">file</span>
                <span className="grow">
                  manifests from <code>{f}</code>{' '}
                  <span className="muted">(the server reads no host file: inline the manifests)</span>
                </span>
                <button type="button" className="icon" aria-label={`Remove manifest file ${f}`} onClick={() => apply(removeMock('kubernetes.files', i))}>
                  ✕
                </button>
              </div>
            </div>
          ))}
          {forms.map((f, i) => (
            <ObjectCard
              key={`${i}|${f.kind}|${f.name}`}
              index={i}
              form={f}
              forms={forms}
              yaml={p.yaml}
              cfg={kubeCfg}
              open={openObject === i}
              onToggle={() => setOpenObject(openObject === i ? undefined : i)}
              apply={apply}
            />
          ))}
          {pasting && <PasteManifests apply={apply} onDone={() => setPasting(false)} />}
        </Group>
      )}

      {showEnv && (
        <EnvironmentGroup refs={env} entries={m.environment} apply={apply} adding={addingEnv} setAdding={setAddingEnv} onAddMissing={addMissingEnv} />
      )}

      {showProviders && (
        <ProvidersGroup
          cards={cards}
          driven={driven}
          findings={providerFindings}
          toAdd={toAdd}
          addSettings={addSettings}
          apply={apply}
          adding={addingProvider}
          setAdding={setAddingProvider}
        />
      )}

      {automatic.length > 0 && (
        <details className="stand-ins">
          <summary>Handled automatically ({automatic.length})</summary>
          <ul>
            {automatic.map((c) => (
              <li key={c.ref}>
                <code>{c.ref}</code> <span className={`mode mode-${c.mode}`}>{c.mode}</span> <span className="muted">{MODE_TEXT[c.mode] ?? c.note ?? ''}</span>
              </li>
            ))}
          </ul>
        </details>
      )}
      {total > 0 && <p className="muted small">Changes here edit the YAML tab, which can also express everything by hand.</p>}
    </div>
  );
}

/**
 * MocksStart is the empty panel: what this configuration reads, with one
 * click to mock it, then every kind of mock.
 */
function MocksStart({
  readers,
  forConfig,
  missingEnv,
  onFile,
  onObject,
}: {
  readers: TopologyComponent[];
  forConfig: MenuItem[];
  missingEnv: EnvRef[];
  onFile(id: FileTemplate): void;
  onObject(kind: ObjectTemplate): void;
}) {
  const needs = readers.length > 0 || missingEnv.length > 0;
  return (
    <div className="start-here">
      {needs ? (
        <>
          <p className="muted">
            This configuration reads its environment:{' '}
            {[...readers.map((c) => c.id), ...(missingEnv.length ? ['${env:…} variables'] : [])].map((s, i) => (
              <span key={s}>
                {i > 0 && ', '}
                <code>{s}</code>
              </span>
            ))}
            . Give it stand-ins, or run and see what is missing.
          </p>
          <div className="template-grid">
            {forConfig.map((it, i) => (
              <button key={i} type="button" className="template-card" onClick={it.onSelect}>
                <strong>{it.label}</strong>
                <span className="muted small">{it.detail}</span>
              </button>
            ))}
          </div>
        </>
      ) : (
        <p className="muted">
          Nothing in this configuration reads files, a Kubernetes API or environment variables, so it runs without mocks. They are for <code>filelog</code>{' '}
          receivers, <code>k8sattributes</code> processors and <code>{'${env:NAME}'}</code> references; you can add them before the components.
        </p>
      )}
      <p className="muted small">{needs ? 'Or add any mock:' : 'Add a mock:'}</p>
      <div className="template-grid compact">
        {FILE_TEMPLATES.map((t) => (
          <button key={t.id} type="button" className="template-card" title={t.detail} onClick={() => onFile(t.id)}>
            <span className="badge">file</span> {t.label}
          </button>
        ))}
        {OBJECT_TEMPLATES.map((t) => (
          <button key={t.kind} type="button" className="template-card" title={t.detail} onClick={() => onObject(t.kind)}>
            <span className="badge">k8s</span> {t.kind}
          </button>
        ))}
      </div>
    </div>
  );
}

/**
 * ProvidersGroup edits the settings each module provider gets: the
 * provider's own YAML, handed to it as JSON when a run starts. What a
 * provider's check found missing comes first, with one click to start its
 * settings.
 */
function ProvidersGroup({
  cards,
  driven,
  findings,
  toAdd,
  addSettings,
  apply,
  adding,
  setAdding,
}: {
  cards: ProviderCard[];
  driven: TopologyComponent[];
  findings: Diagnostic[];
  toAdd: ReportProvider[];
  addSettings(name: string): boolean;
  apply(e: Edit): boolean;
  adding: boolean;
  setAdding(v: boolean): void;
}) {
  return (
    <section className="mock-group">
      <header>
        <h4>Module providers</h4>
        {driven.length ? (
          <span className="muted small">
            driving{' '}
            {driven.map((c, i) => (
              <span key={c.ref}>
                {i > 0 && ', '}
                <code>{c.id}</code> <span className="muted">({c.provider})</span>
              </span>
            ))}
          </span>
        ) : (
          <span className="muted small">settings under mocks.providers, read by the providers compiled into the runtime</span>
        )}
      </header>
      <p className="hint">
        A provider drives its custom components during a run, for example by serving the backend they talk to. What its settings mean is the provider's own; its
        documentation says what it reads.
      </p>
      {findings.map((g, i) => (
        <p key={i} className="warning">
          <code>{g.component}</code>: {g.message}
          {g.hint && <span className="muted"> {g.hint}</span>}
          {g.provider_settings && (
            <>
              {' '}
              <button type="button" className="link" title={proposalText(g.provider_settings.settings)} onClick={() => addSettings(g.provider_settings!.provider)}>
                Add what {g.provider_settings.provider} proposes
              </button>
            </>
          )}
        </p>
      ))}
      {cards.length === 0 && toAdd.length > 0 && (
        <div className="template-grid compact">
          {toAdd.map((pr) => (
            <button key={pr.name} type="button" className="template-card" title={pr.package} onClick={() => addSettings(pr.name)}>
              <span className="badge">provider</span> Settings for {pr.name}
            </button>
          ))}
        </div>
      )}
      {cards.map((c) => (
        <div key={c.name} className="mock-card">
          <div className="mock-card-head">
            <span className="badge">provider</span>
            <span className="grow">
              <code>{c.name}</code> {c.package && <span className="muted">from {c.package}</span>}
            </span>
            <button type="button" className="icon" aria-label={`Remove the settings of ${c.name}`} onClick={() => apply(removeProvider(c.name))}>
              ✕
            </button>
          </div>
          {c.problem && <p className="warning">{c.problem}</p>}
          <TextBlock
            key={c.settings}
            label={`Settings of ${c.name} as YAML`}
            text={c.settings}
            onCommit={(v) => apply(setProviderSettings(c.name, v))}
          />
        </div>
      ))}
      {adding && (
        <div className="mock-card">
          <CommitInput
            value=""
            label="Provider name"
            placeholder="Provider name, as the runtime lists it"
            onCommit={(v) => {
              if (v.trim() && apply(setProviderSettings(v, ''))) setAdding(false);
            }}
          />
          <button type="button" onClick={() => setAdding(false)}>
            Cancel
          </button>
        </div>
      )}
    </section>
  );
}

/** Group is one kind of mock, with whether the configuration reads it. */
function Group({
  title,
  readers,
  unread,
  gaps,
  children,
}: {
  title: string;
  readers: TopologyComponent[];
  unread: string;
  gaps: Diagnostic[];
  children: React.ReactNode;
}) {
  return (
    <section className="mock-group">
      <header>
        <h4>{title}</h4>
        {readers.length ? (
          <span className="muted small">
            read by{' '}
            {readers.map((c, i) => (
              <span key={c.ref}>
                {i > 0 && ', '}
                <code>{c.id}</code>
              </span>
            ))}
          </span>
        ) : (
          <span className="chip chip-warn" title="Kept in the document, but this configuration never reads them">
            not read
          </span>
        )}
      </header>
      {!readers.length && <p className="warn-text small">{unread}</p>}
      {gaps.map((g, i) => (
        <p key={i} className="warning">
          {g.message}
          {g.hint && <span className="muted"> {g.hint}</span>}
        </p>
      ))}
      {children}
    </section>
  );
}

function FileCard({ index, file, apply }: { index: number; file: FileModel; apply(e: Edit): boolean }) {
  const [showExisting, setShowExisting] = useState(file.existing !== '');
  const menu: MenuItem[] = [
    showExisting
      ? {
          label: 'Remove existing content',
          onSelect: () => {
            if (apply(setFileText(index, 'existing', ''))) setShowExisting(false);
          },
        }
      : {
          label: 'Add existing content',
          detail: 'Present before the Collector starts; start_at decides whether it is read',
          onSelect: () => setShowExisting(true),
        },
    { label: 'Remove file', onSelect: () => apply(removeMock('files', index)) },
  ];
  return (
    <div className="mock-card">
      <div className="mock-card-head">
        <span className="badge">file</span>
        <CommitInput
          className="grow mono"
          value={file.path}
          label={`Path of mocked file ${index}`}
          placeholder="/var/log/pods/ns_pod_uid/container/0.log"
          onCommit={(v) => v.trim() && apply(setField(['mocks', 'files', index, 'path'], v.trim()))}
        />
        <Menu label="⋯" className="icon" title="More actions" items={menu} align="right" />
      </div>
      {showExisting && <span className="muted small">Before the Collector starts (start_at decides whether it is read)</span>}
      {showExisting && (
        <TextBlock
          key={`e|${file.existing}`}
          label="Existing content (before the Collector starts)"
          text={file.existing}
          onCommit={(v) => apply(setFileText(index, 'existing', v))}
        />
      )}
      {file.file !== undefined ? (
        <p className="muted">
          Content is read from <code>{file.file}</code>, relative to the scenario.{' '}
          <button type="button" className="text-link" onClick={() => apply(setFileText(index, 'content', '\n'))}>
            Type content instead
          </button>
        </p>
      ) : (
        <span className="muted small">Written while the Collector runs, so it is always read</span>
      )}
      {file.file === undefined && (
        <TextBlock
          key={`c|${file.content}`}
          label="Content (written while the Collector runs, so it is always read)"
          text={file.content}
          onCommit={(v) => apply(setFileText(index, 'content', v))}
        />
      )}
    </div>
  );
}

/**
 * ObjectCard is one Kubernetes object: a summary line that opens to its
 * form, or to its YAML for anything the form does not cover.
 */
function ObjectCard({
  index: i,
  form: f,
  forms,
  yaml,
  cfg,
  open,
  onToggle,
  apply,
}: {
  index: number;
  form: KubeForm;
  forms: KubeForm[];
  yaml: string;
  cfg: Obj;
  open: boolean;
  onToggle(): void;
  apply(e: Edit): boolean;
}) {
  const [asYAML, setAsYAML] = useState(false);
  const yamlMode = asYAML || !!f.yamlOnly;
  const [head, ...rest] = kubeSummary(f).split(' · ');
  const menu: MenuItem[] = [
    {
      label: yamlMode ? 'Edit in the form' : 'Edit as YAML',
      disabled: !!f.yamlOnly,
      onSelect: () => {
        setAsYAML(!asYAML);
        if (!open) onToggle();
      },
    },
    { label: 'Duplicate', detail: 'With a name, UID and IP of its own', disabled: !!f.yamlOnly, onSelect: () => apply(duplicateKubeObject(i, forms)) },
    { label: `Remove ${f.kind || 'object'}`, onSelect: () => apply(removeMock('kubernetes.objects', i)) },
  ];
  return (
    <div className={`mock-card${open ? ' open' : ''}`}>
      <div className="mock-card-head">
        <button type="button" className="mock-card-toggle" aria-expanded={open} onClick={onToggle}>
          <span className="caret">{open ? '▾' : '▸'}</span>
          <span className="badge">{f.kind || '?'}</span>
          <strong>{f.name ? head : '(no name)'}</strong>
          <span className="muted small">{f.yamlOnly ?? rest.join(' · ')}</span>
        </button>
        <Menu label="⋯" className="icon" title="More actions" items={menu} align="right" />
      </div>
      {open &&
        (yamlMode ? (
          <ObjectYAML key={yaml} text={kubeObjectYAML(yaml, i)} onApply={(t) => apply(replaceKubeObject(i, t))} note={f.yamlOnly} />
        ) : (
          <ObjectForm index={i} form={f} cfg={cfg} apply={apply} />
        ))}
    </div>
  );
}

function ObjectForm({ index: i, form: f, cfg, apply }: { index: number; form: KubeForm; cfg: Obj; apply(e: Edit): boolean }) {
  return (
    <div className="object-form">
      <div className="object-fields">
        {fieldsOf(f.kind).map((field) => (
          <label key={field}>
            <span className="muted small">{FIELD_LABELS[field]}</span>
            <CommitInput
              key={f[field]}
              className="mono"
              value={f[field]}
              label={`${f.kind} ${FIELD_LABELS[field]}`}
              placeholder={field === 'ip' ? '10.42.0.17' : field === 'namespace' ? 'default' : ''}
              title={field === 'ip' ? 'status.podIP: association by k8s.pod.ip or by the sender address reads it' : undefined}
              onCommit={(v) => apply(setKubeField(i, field, v))}
            />
          </label>
        ))}
      </div>
      <KeyValues title="Labels" entries={f.labels} offered={extractedKeys(cfg, f.kind, 'labels')} onCommit={(e) => apply(setKubeMap(i, 'labels', e))} />
      <KeyValues
        title="Annotations"
        entries={f.annotations}
        offered={extractedKeys(cfg, f.kind, 'annotations')}
        onCommit={(e) => apply(setKubeMap(i, 'annotations', e))}
      />
    </div>
  );
}

/**
 * KeyValues edits labels or annotations. Keys the processor extracts but the
 * object lacks are offered with one click.
 */
function KeyValues({ title, entries, offered, onCommit }: { title: string; entries: Entries; offered: string[]; onCommit(e: Entries): boolean }) {
  const [adding, setAdding] = useState(false);
  const missing = offered.filter((k) => !entries.some(([e]) => e === k));
  const replace = (j: number, entry?: [string, string]) => onCommit(entry ? entries.map((r, x) => (x === j ? entry : r)) : entries.filter((_, x) => x !== j));
  const singular = title.toLowerCase().replace(/s$/, '');
  return (
    <div className="kv-rows">
      <div className="kv-head">
        <span className="muted small">{title}</span>
        {missing.map((k) => (
          <button
            key={k}
            type="button"
            className="chip-button"
            title={`The processor extracts the ${singular} ${k}`}
            onClick={() => onCommit([...entries, [k, 'example']])}
          >
            + {k}
          </button>
        ))}
        <button type="button" className="text-link" disabled={adding} onClick={() => setAdding(true)}>
          + {singular}
        </button>
      </div>
      {entries.map(([k, v], j) => (
        <div key={`${j}|${k}|${v}`} className="form-line">
          <CommitInput
            className="kv-key mono"
            value={k}
            label={`${title} key`}
            placeholder="key"
            onCommit={(nk) => replace(j, nk.trim() ? [nk, v] : undefined)}
          />
          <span className="muted">=</span>
          <CommitInput className="grow mono" value={v} label={`${title} value of ${k}`} placeholder="value" onCommit={(nv) => replace(j, [k, nv])} />
          <button type="button" className="icon" aria-label={`Remove ${singular} ${k}`} onClick={() => replace(j)}>
            ✕
          </button>
        </div>
      ))}
      {adding && (
        <NewEntry
          placeholder="key"
          onCommit={(k) => {
            setAdding(false);
            if (k.trim()) onCommit([...entries, [k.trim(), '']]);
          }}
        />
      )}
    </div>
  );
}

function ObjectYAML({ text, onApply, note }: { text: string; onApply(t: string): boolean; note?: string }) {
  const [draft, setDraft] = useState(text);
  return (
    <div className="form-row">
      {note && <p className="muted small">{note} Edit it as YAML.</p>}
      <textarea
        className="mono conditions"
        rows={Math.min(18, Math.max(4, draft.split('\n').length))}
        value={draft}
        aria-label="Object YAML"
        spellCheck={false}
        onChange={(e) => setDraft(e.target.value)}
      />
      <div className="form-actions">
        <button type="button" className="primary" disabled={draft === text} onClick={() => onApply(draft)}>
          Apply
        </button>
        <button type="button" disabled={draft === text} onClick={() => setDraft(text)}>
          Revert
        </button>
      </div>
    </div>
  );
}

function PasteManifests({ apply, onDone }: { apply(e: Edit): boolean; onDone(): void }) {
  const [text, setText] = useState('');
  return (
    <div className="mock-card">
      <textarea
        className="mono conditions"
        rows={8}
        value={text}
        autoFocus
        aria-label="Kubernetes manifests"
        placeholder="Output of: kubectl get pod checkout-1 -n shop -o yaml"
        spellCheck={false}
        onChange={(e) => setText(e.target.value)}
      />
      <div className="form-actions">
        <button
          type="button"
          className="primary"
          disabled={!text.trim()}
          onClick={() => {
            if (apply(addKubeObjects(text))) onDone();
          }}
        >
          Add objects
        </button>
        <button type="button" onClick={onDone}>
          Cancel
        </button>
      </div>
    </div>
  );
}

/**
 * EnvironmentGroup edits ${env:NAME} values. The configuration's
 * references are listed first, so a missing value is visible before a run
 * fails. Values may be secrets, so they are masked until revealed.
 */
function EnvironmentGroup({
  refs,
  entries,
  apply,
  adding,
  setAdding,
  onAddMissing,
}: {
  refs: EnvRef[];
  entries: [string, string][];
  apply(e: Edit): boolean;
  adding: boolean;
  setAdding(v: boolean): void;
  onAddMissing(): void;
}) {
  const [reveal, setReveal] = useState(false);
  const set = new Map(entries);
  const missing = refs.filter((r) => !set.has(r.name) && !r.hasDefault);
  const replace = (i: number, entry?: [string, string]) =>
    apply(setEnvironment(entry ? entries.map((r, j) => (j === i ? entry : r)) : entries.filter((_, j) => j !== i)));
  const unused = (name: string) => !refs.some((r) => r.name === name);
  return (
    <section className="mock-group">
      <header>
        <h4>Environment variables</h4>
        {missing.length ? (
          <span className="chip chip-warn">{missing.length} missing</span>
        ) : refs.length ? (
          <span className="chip chip-good">all set</span>
        ) : (
          <span className="chip chip-warn" title="Kept in the document, but this configuration never reads them">
            not read
          </span>
        )}
        <span className="grow" />
        {missing.length > 0 && (
          <button type="button" className="primary" onClick={onAddMissing}>
            + Add missing
          </button>
        )}
        {entries.length > 0 && (
          <label className="inline small">
            <input type="checkbox" checked={reveal} onChange={(e) => setReveal(e.target.checked)} /> show values
          </label>
        )}
      </header>
      <p className="hint">
        {refs.length ? (
          <>
            The configuration reads{' '}
            {refs.map((r, i) => (
              <span key={r.name}>
                {i > 0 && ', '}
                <code className={!set.has(r.name) && !r.hasDefault ? 'missing' : undefined}>{r.name}</code>
                {r.hasDefault && <span className="muted"> (has a default)</span>}
              </span>
            ))}
            . Your shell's environment is never used.
          </>
        ) : (
          'Values for ${env:NAME} references. Your shell’s environment is never used.'
        )}
      </p>
      {entries.map(([k, v], i) => (
        <div key={k} className="form-line">
          <CommitInput
            className="env-name mono"
            value={k}
            label="Variable name"
            placeholder="NAME"
            onCommit={(name) => replace(i, name.trim() ? [name.trim(), v] : undefined)}
          />
          <span className="muted">=</span>
          <input
            className="grow"
            type={reveal ? 'text' : 'password'}
            value={v}
            placeholder="value"
            aria-label={`Value of ${k}`}
            autoComplete="off"
            onChange={(e) => replace(i, [k, e.target.value])}
          />
          {refs.length > 0 && unused(k) && (
            <span className="muted" title="The configuration does not reference this variable">
              unused
            </span>
          )}
          <button type="button" className="icon" aria-label={`Remove ${k}`} onClick={() => replace(i)}>
            ✕
          </button>
        </div>
      ))}
      {adding && (
        <NewEntry
          className="env-name mono"
          placeholder="NAME"
          onCommit={(name) => {
            setAdding(false);
            if (name.trim()) apply(setEnvironment([...entries, [name.trim(), '']]));
          }}
        />
      )}
      {!adding && (
        <button type="button" className="add-check text-link" onClick={() => setAdding(true)}>
          + Variable
        </button>
      )}
    </section>
  );
}

function NewEntry({ onCommit, placeholder, className = 'kv-key mono' }: { onCommit(v: string): void; placeholder: string; className?: string }) {
  const [draft, setDraft] = useState('');
  return (
    <div className="form-line">
      <input
        className={className}
        value={draft}
        placeholder={placeholder}
        aria-label={`New ${placeholder}`}
        autoFocus
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') e.currentTarget.blur();
          if (e.key === 'Escape') {
            setDraft('');
            onCommit('');
          }
        }}
        onBlur={() => onCommit(draft)}
      />
    </div>
  );
}
