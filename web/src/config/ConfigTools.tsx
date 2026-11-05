import { useMemo, useState } from 'react';
import { Menu, type MenuItem } from '../scenario/fields';
import { openTextFile } from '../files';
import { kindDraft, openedPatch, type DraftKind } from '../workspace/drafts';
import { getWorkspace, updateWorkspace, type WorkspaceState } from '../workspace/store';
import { useReplaceDrafts } from '../workspace/useReplaceDrafts';
import {
  COMPONENT_SNIPPETS,
  CONFIG_TEMPLATES,
  defaultPipelines,
  insertComponent,
  KIND_LABELS,
  pipelinesOf,
  type ComponentSnippet,
  type ConfigTemplate,
  type Kind,
} from './templates';

/**
 * useReplaceDraft replaces the configuration or scenario draft, asking first
 * when it holds unsaved edits. The confirmation renders where the caller
 * puts it.
 */
export function useReplaceDraft(kind: DraftKind) {
  const { request, confirm } = useReplaceDrafts();
  const replace = (text: string, label: string) =>
    request([kindDraft(kind)], label ? `“${label}”` : `The new ${kind}`, () => updateWorkspace(openedPatch(getWorkspace(), kind, text, label)));
  return { replace, confirm };
}

export const useReplaceConfig = () => useReplaceDraft('configuration');

export const openConfigFile = async (replace: (text: string, label: string) => void) => {
  const f = await openTextFile('.yaml,.yml');
  if (f) replace(f.text, f.name);
};

/** NewConfigMenu starts the configuration from a template. */
export function NewConfigMenu({ replace }: { replace(text: string, label: string): void }) {
  const items: MenuItem[] = CONFIG_TEMPLATES.map((t, i) => ({
    group: i === 0 ? 'Start from' : undefined,
    label: t.label,
    detail: t.description,
    onSelect: () => replace(t.yaml, ''),
  }));
  items.push({
    group: 'Or',
    label: 'Empty configuration',
    detail: 'Build it with Insert component',
    onSelect: () => replace('', ''),
  });
  return <Menu label="New ▾" items={items} title="Start a new configuration from a template" />;
}

/**
 * InsertMenu lists the components of the debugger's distribution, each with
 * a working configuration; InsertBar then wires the picked one in.
 */
export function InsertMenu({ onPick }: { onPick(s: ComponentSnippet): void }) {
  const items = useMemo(
    (): MenuItem[] =>
      (Object.keys(KIND_LABELS) as Kind[]).flatMap((kind) =>
        COMPONENT_SNIPPETS.filter((s) => s.kind === kind).map((s, i) => ({
          group: i === 0 ? KIND_LABELS[kind] : undefined,
          label: `${s.label} · ${s.type}`,
          detail: s.description,
          onSelect: () => onPick(s),
        })),
      ),
    [onPick],
  );
  return (
    <Menu
      label="+ Insert component"
      className="primary-outline"
      items={items}
      title="Add a receiver, processor, exporter or connector with a working configuration"
    />
  );
}

/** InsertBar asks which pipelines the component joins. */
export function InsertBar({
  ws,
  snippet: s,
  onDone,
  onInserted,
}: {
  ws: WorkspaceState;
  snippet: ComponentSnippet;
  onDone(): void;
  onInserted(line: number): void;
}) {
  const pipelines = pipelinesOf(ws.configYAML);
  const fits = defaultPipelines(s, pipelines);
  const [chosen, setChosen] = useState<string[]>(s.kind === 'connector' ? fits.slice(0, 1) : fits);
  const [error, setError] = useState<string>();
  const insert = () => {
    const r = insertComponent(getWorkspace().configYAML, s, chosen);
    if ('error' in r) {
      setError(r.error);
      return;
    }
    updateWorkspace({ configYAML: r.yaml });
    onDone();
    onInserted(r.line);
  };
  const role = s.kind === 'connector' ? 'Count or route from' : s.kind === 'receiver' ? 'Feeds' : s.kind === 'exporter' ? 'Exports from' : 'Runs in';
  return (
    <div className="insert-bar" role="dialog" aria-label={`Insert ${s.type}`}>
      <strong>
        {s.label} <code>{s.type}</code>
      </strong>
      {pipelines.length ? (
        <span className="insert-pipelines">
          <span className="muted">{role}</span>
          {pipelines.map((p) => {
            const ok = !!p.signal && s.signals.includes(p.signal);
            return (
              <label
                key={p.name}
                className={`inline${ok ? '' : ' muted'}`}
                title={ok ? undefined : `${s.type} does not accept ${p.signal ?? 'this pipeline'}'s signal`}
              >
                <input
                  type="checkbox"
                  disabled={!ok}
                  checked={chosen.includes(p.name)}
                  onChange={(e) => setChosen(e.target.checked ? [...chosen, p.name] : chosen.filter((n) => n !== p.name))}
                />
                {p.name}
              </label>
            );
          })}
        </span>
      ) : (
        <span className="muted">No pipelines yet: it is added to its section only.</span>
      )}
      {s.kind === 'connector' && (
        <span className="muted small">
          Creates {(s.outputs ?? []).length === 1 ? 'a pipeline' : 'pipelines'} for its output
          {s.replaces ? ', which take over the exporters' : ' with the same exporters'}.
        </span>
      )}
      {s.mocked && <span className="muted small">Runs against the scenario's mocks: add them in the Mocks tab.</span>}
      {error && <span className="error">{error}</span>}
      <span className="grow" />
      <button
        type="button"
        className="primary"
        onClick={insert}
        disabled={s.kind === 'connector' && pipelines.length > 0 && chosen.length === 0}
        title={s.kind === 'connector' && pipelines.length > 0 && chosen.length === 0 ? 'Choose the pipeline it reads from' : undefined}
      >
        Insert
      </button>
      <button type="button" onClick={onDone}>
        Cancel
      </button>
    </div>
  );
}

/** ConfigStart is the empty configuration: templates, a file or samples. */
export function ConfigStart({ replace, onInsertFirst }: { replace(text: string, label: string): void; onInsertFirst(): void }) {
  return (
    <div className="start-here config-start">
      <h3>Start a configuration</h3>
      <p className="muted">Pick a starting pipeline and add components to it, open your Collector configuration, or try a sample from the menu above.</p>
      <div className="template-grid">
        {CONFIG_TEMPLATES.map((t: ConfigTemplate) => (
          <button key={t.id} type="button" className="template-card" onClick={() => replace(t.yaml, '')}>
            <strong>{t.label}</strong>
            <span className="muted small">{t.description}</span>
          </button>
        ))}
      </div>
      <div className="form-actions">
        <button type="button" onClick={() => openConfigFile(replace)}>
          Open a configuration file…
        </button>
        <button type="button" onClick={onInsertFirst}>
          Write it myself
        </button>
      </div>
    </div>
  );
}
