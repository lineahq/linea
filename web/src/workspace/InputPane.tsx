import { useMemo, useRef, useState, type KeyboardEvent } from 'react';
import { useQuery } from '@tanstack/react-query';
import { ConfigStart, InsertBar, InsertMenu, NewConfigMenu, openConfigFile, useReplaceConfig, useReplaceDraft } from '../config/ConfigTools';
import type { ComponentSnippet } from '../config/templates';
import type { Graph, Inspection } from '../api/types';
import { YamlEditor, type EditorHandle } from '../editor/YamlEditor';
import { DiagnosticsList } from '../editor/DiagnosticsList';
import type { Marker } from '../editor/markers';
import { baseName, openTextFile } from '../files';
import { InputsPanel } from '../input/InputsPanel';
import { runBlocker, summarizePayload } from '../input/quickInput';
import { MocksPanel } from '../mocks/MocksPanel';
import { defaultRuntimeQuery, runtimesQuery } from '../runtimes/RuntimeSelect';
import { BUILT_IN, effectiveRuntime, runtimeProviders } from '../runtimes/runtimes';
import { AssertionBuilder } from '../scenario/AssertionBuilder';
import { editScenario, readScenario, setField } from '../scenario/scenarioDoc';
import { useStartRun } from '../runs/useStartRun';
import { EDITOR_TABS, editorTab, type EditorTab } from './editorTabs';
import { getWorkspace, updateWorkspace, type WorkspaceState } from './store';
import { SavedSources } from './SavedSources';
import { configStatus } from '../debugger/configStatus';
import { SamplesMenu } from '../debugger/SamplesMenu';
import { Icon } from '../ui/Icon';

interface Props {
  ws: WorkspaceState;
  requested?: string;
  onTab(tab: EditorTab): void;
  inspection?: Inspection;
  /** The topology of the configuration, or of the shown run while it is invalid. */
  topology?: Graph;
  markers: Marker[];
  /** A finished run that assertions can be checked against. */
  runId?: string;
  /** Shows a run started from the pane. */
  onRun(id: string): void;
  /** A sample replaced the drafts. */
  onSampleLoaded(): void;
}

/**
 * InputPane edits what a run uses: the configuration and one document
 * holding inputs, mocks and assertions. A configuration and one input make
 * a quick run; more inputs or assertions make a scenario, and any input can
 * run on its own.
 */
export function InputPane({ ws, requested, onTab, inspection, topology, markers, runId, onRun, onSampleLoaded }: Props) {
  const configEditor = useRef<EditorHandle>(undefined);
  const replaceConfig = useReplaceConfig();
  const [writing, setWriting] = useState(false);
  const tab = editorTab(requested);
  const read = useMemo(() => readScenario(ws.scenarioYAML), [ws.scenarioYAML]);
  const model = 'error' in read ? undefined : read.model;
  const start = useStartRun(ws, onRun);
  const runtimes = useQuery(runtimesQuery);
  const team = useQuery(defaultRuntimeQuery);
  const configValid = !!inspection?.valid;
  const diagnostics = inspection?.diagnostics ?? [];
  // An ignored scenario shows where its problem is: in an assertion, or with the mocks it disables.
  const assertionDiagnostics = diagnostics.filter((d) => d.code === 'scenario_ignored' && /assertions\[\d+\]/.test(d.message));
  const mockDiagnostics = diagnostics.filter((d) => d.code === 'mock_fixture_missing' || (d.code === 'scenario_ignored' && !assertionDiagnostics.includes(d)));
  const badInputs = (model?.inputs ?? []).filter((i) => i.data !== undefined && 'error' in summarizePayload(i.data)).length;
  const attention: Partial<Record<EditorTab, { n: number; cls: string }>> = {
    config: {
      n: markers.filter((m) => m.severity === 'error').length,
      cls: 'bad',
    },
    inputs: { n: badInputs, cls: 'warn' },
    mocks: { n: mockDiagnostics.length, cls: 'warn' },
    assertions: { n: assertionDiagnostics.length, cls: 'warn' },
    yaml: { n: 'error' in read ? 1 : 0, cls: 'bad' },
  };
  const counts: Partial<Record<EditorTab, number>> = {
    inputs: model?.inputs.length,
    assertions: model?.assertions.length,
    mocks: model ? model.files.length + model.kubeObjects.length + model.kubeFiles.length + model.environment.length : undefined,
  };
  const scenarioIgnored = diagnostics.filter((d) => d.code === 'scenario_ignored');

  const payloads = useMemo(
    () =>
      (model?.inputs ?? []).flatMap((i) => {
        try {
          return [JSON.parse(i.data ?? '') as unknown];
        } catch {
          return [];
        }
      }),
    [model],
  );

  const keyNav = (e: KeyboardEvent<HTMLDivElement>) => {
    if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(e.key)) return;
    const ids = EDITOR_TABS.map((t) => t.id);
    const i = ids.indexOf(tab);
    const next = e.key === 'Home' ? 0 : e.key === 'End' ? ids.length - 1 : (i + (e.key === 'ArrowLeft' ? -1 : 1) + ids.length) % ids.length;
    e.preventDefault();
    onTab(ids[next]);
    e.currentTarget.querySelectorAll<HTMLButtonElement>('[role=tab]')[next]?.focus();
  };

  const status = tab === 'config' ? configStatus(markers, inspection, ws.configYAML) : undefined;
  return (
    <section className="od-pane" aria-label="Run inputs">
      <div className="od-tabs editor-tabs" role="tablist" aria-label="Run inputs" onKeyDown={keyNav}>
        {EDITOR_TABS.map((t) => {
          const a = attention[t.id];
          const n = counts[t.id];
          return (
            <button
              key={t.id}
              type="button"
              role="tab"
              className="od-tab"
              aria-selected={tab === t.id}
              tabIndex={tab === t.id ? 0 : -1}
              title={t.purpose}
              onClick={() => onTab(t.id)}
            >
              {t.label}
              {a && a.n > 0 ? (
                <span className={`od-tab__count od-tab__count--${a.cls === 'bad' ? 'fail' : 'warn'}`} aria-label={`${a.n} to review`}>
                  {a.n}
                </span>
              ) : (
                n !== undefined && <span className="od-tab__count">{n}</span>
              )}
            </button>
          );
        })}
      </div>

      {tab === 'config' ? (
        <ConfigBar ws={ws} replace={replaceConfig.replace} onInserted={(line) => setTimeout(() => configEditor.current?.reveal(line), 0)} onSampleLoaded={onSampleLoaded} />
      ) : (
        <DocumentBar ws={ws} />
      )}
      {tab === 'config' && replaceConfig.confirm}
      <div
        className={`editor-host${['inputs', 'assertions', 'mocks'].includes(tab) || (tab === 'config' && !ws.configYAML.trim() && !writing) ? ' scroll' : ''}`}
      >
        {tab === 'config' && !ws.configYAML.trim() && !writing ? (
          <ConfigStart replace={replaceConfig.replace} onInsertFirst={() => setWriting(true)} />
        ) : tab === 'config' ? (
          <YamlEditor
            path="config.yaml"
            value={ws.configYAML}
            onChange={(configYAML) => updateWorkspace({ configYAML })}
            markers={markers}
            handle={(h) => (configEditor.current = h)}
          />
        ) : tab === 'inputs' ? (
          <InputsPanel
            ws={ws}
            topology={topology}
            diagnostics={scenarioIgnored}
            runInput={(input) => start.mutate({ input })}
            blocker={(i) => runBlocker(ws, configValid, i)}
            running={start.isPending}
            onMocks={() => onTab('mocks')}
          />
        ) : tab === 'assertions' ? (
          <AssertionBuilder ws={ws} topology={topology} runId={runId} payloads={payloads} diagnostics={assertionDiagnostics} />
        ) : tab === 'mocks' ? (
          <MocksPanel
            yaml={ws.scenarioYAML}
            onChange={(scenarioYAML) => updateWorkspace({ scenarioYAML })}
            configYAML={ws.configYAML}
            topology={topology}
            effective={inspection?.effective}
            payloads={payloads}
            diagnostics={mockDiagnostics}
            providers={runtimeProviders(runtimes.data, ws.runtime ?? BUILT_IN, team.data?.name)}
            runtime={effectiveRuntime(ws.runtime ?? BUILT_IN, team.data?.name) ?? BUILT_IN}
            setClientAddress={(address) => {
              const r = readScenario(getWorkspace().scenarioYAML);
              if ('error' in r) return;
              const edited = editScenario(getWorkspace().scenarioYAML, (doc) =>
                r.model.inputs.forEach((input, i) => !input.clientAddress && setField(['inputs', i, 'client_address'], address)(doc)),
              );
              if (!('error' in edited)) updateWorkspace({ scenarioYAML: edited.yaml });
            }}
          />
        ) : (
          <YamlEditor path="scenario.yaml" value={ws.scenarioYAML} onChange={(scenarioYAML) => updateWorkspace({ scenarioYAML })} />
        )}
      </div>
      {start.error && <p className="error pane-error">{start.error.message}</p>}
      {tab === 'config' && <DiagnosticsList markers={markers} onSelect={(m) => configEditor.current?.reveal(m.location.startLine, m.location.startColumn)} />}
      {status && (
        <div className="config-status" style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '0 12px', height: 28, borderTop: '1px solid var(--border-subtle)', fontSize: 12, flex: 'none' }} role="status">
          {status.items.map((s) => (
            <span key={s.text} className={`od-ready od-ready--${s.tone}`}>
              <Icon name={s.tone === 'ok' ? 'circle-check' : s.tone === 'warn' ? 'triangle-alert' : 'circle-x'} size={14} />
              {s.text}
            </span>
          ))}
          <span className="od-subtle">{status.detail}</span>
        </div>
      )}
    </section>
  );
}

/** ConfigBar names the configuration and offers templates, components and files. */
function ConfigBar({ ws, replace, onInserted, onSampleLoaded }: { ws: WorkspaceState; replace(text: string, label: string): void; onInserted(line: number): void; onSampleLoaded(): void }) {
  const [picked, setPicked] = useState<ComponentSnippet>();
  return (
    <>
      <div className="file-bar config-bar">
        <InsertMenu onPick={setPicked} />
        <SamplesMenu onLoaded={onSampleLoaded} />
        <span className="file-name" title={ws.configPath || 'Not saved to a file'}>
          {ws.configPath ? baseName(ws.configPath) : 'Unsaved configuration'}
        </span>
        <NewConfigMenu replace={replace} />
        <button type="button" onClick={() => openConfigFile(replace)}>
          Open…
        </button>
        <SavedSources ws={ws} kind="configuration" hideSave />
      </div>
      {picked && <InsertBar key={`${picked.kind}/${picked.type}`} ws={ws} snippet={picked} onDone={() => setPicked(undefined)} onInserted={onInserted} />}
    </>
  );
}

/** describeRun says how the document runs, in a few words. */
function describeRun(yaml: string): string {
  const r = readScenario(yaml);
  if ('error' in r) return 'scenario';
  const { inputs, assertions } = r.model;
  const parts = [`${inputs.length} input${inputs.length === 1 ? '' : 's'}`];
  if (assertions.length) parts.push(`${assertions.length} assertion${assertions.length === 1 ? '' : 's'}`);
  return parts.join(' · ');
}

/** DocumentBar opens, downloads and saves the document as a scenario file. */
function DocumentBar({ ws }: { ws: WorkspaceState }) {
  const replace = useReplaceDraft('scenario');
  return (
    <>
      <div className="file-bar">
        <span className="file-name" title={ws.scenarioPath || 'Not saved to a file'}>
          {ws.scenarioPath ? baseName(ws.scenarioPath) : 'scenario.yaml'}
        </span>
        <span className="muted small run-kind" title="A scenario with assertions is a test">
          {describeRun(ws.scenarioYAML)}
        </span>
        <span className="grow" />
        <button
          type="button"
          title="Open a scenario file"
          onClick={async () => {
            const f = await openTextFile('.yaml,.yml');
            // A browser does not reveal the file's directory: relative input
            // files need the path typed or the data inlined.
            if (f) replace.replace(f.text, f.name);
          }}
        >
          Open…
        </button>
        <SavedSources ws={ws} kind="scenario" hideSave />
      </div>
      {replace.confirm}
    </>
  );
}
