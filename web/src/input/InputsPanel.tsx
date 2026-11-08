import { useEffect, useMemo, useRef, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';
import type { Diagnostic, Graph } from '../api/types';
import { YamlEditor } from '../editor/YamlEditor';
import type { Marker } from '../editor/markers';
import { downloadText, openTextFile } from '../files';
import { JSON_LANGUAGE } from '../monaco';
import { SignalExplorer } from '../inspect/SignalExplorer';
import { mockedComponents } from '../mocks/suggest';
import { CommitInput, Menu, type MenuItem } from '../scenario/fields';
import {
  addInput,
  duplicateItem,
  moveItem,
  receiverSignals,
  removeItem,
  setField,
  setInputData,
  setInputSource,
  setHeaders,
  type Edit,
  type Header,
  type InputModel,
} from '../scenario/scenarioDoc';
import { useDocEdit } from '../scenario/useDocEdit';
import { decodeText, encodeText } from '../workspace/drafts';
import type { WorkspaceState } from '../workspace/store';
import { jsonProblem, summarizePayload } from './quickInput';
import { CaptureInputPicker } from '../capture/CaptureInputPicker';
import { useDenial } from '../ui/session';
import { templatesFor, type PayloadTemplate } from './templates';
import { HeadersEditor } from './HeadersEditor';
import { headersChip } from './headers';

interface Props {
  ws: WorkspaceState;
  topology?: Graph;
  /** Diagnostics about inputs the run ignores. */
  diagnostics: Diagnostic[];
  /** Runs one input on its own, with the document's mocks. */
  runInput(i: number): void;
  /** Why an input cannot run on its own, if it cannot. */
  blocker(i: number): string | undefined;
  running: boolean;
  /** Opens the Mocks tab. */
  onMocks(): void;
}

const ITEM_NAMES: Record<string, [string, string]> = {
  traces: ['span', 'spans'],
  logs: ['log record', 'log records'],
  metrics: ['data point', 'data points'],
};

/** describeData says what an inline payload holds, in one line. */
function describeData(data: string): { text: string; error?: boolean } {
  if (!data.trim()) return { text: 'empty', error: true };
  const s = summarizePayload(data);
  if ('error' in s) return { text: s.error, error: true };
  const [one, many] = ITEM_NAMES[s.signal];
  return { text: `${s.signal} · ${s.items} ${s.items === 1 ? one : many}` };
}

/** receiverFor picks the receiver a payload of signal enters by default. */
function receiverFor(signal: string, bySignal: Map<string, string[]>): string {
  for (const [r, signals] of bySignal) if (signals.includes(signal)) return r;
  return '';
}

/**
 * InputsPanel edits the payloads a run sends, as cards. One input with a
 * configuration is all a quick run needs; more inputs, waits between them
 * and assertions make a scenario. An input starts from a template, a file or
 * pasted JSON, and any input can run on its own.
 */
export function InputsPanel({ ws, topology, diagnostics, runInput, blocker, running, onMocks }: Props) {
  const { read, apply, problem } = useDocEdit(ws.scenarioYAML);
  const [open, setOpen] = useState(0);
  const [captureOpen, setCaptureOpen] = useState(false);
  // Deriving inputs from a capture inspects this configuration on the runtime.
  const captureDenied = useDenial('run.submit');
  const bySignal = useMemo(() => receiverSignals(topology), [topology]);
  const receivers = [...bySignal.keys()].sort();
  const kube = mockedComponents(topology).some((m) => m.provider === 'kubernetes');
  const fileReceivers = (topology?.components ?? []).filter((c) => c.kind === 'receiver' && c.mode === 'dependency_mocked');
  if ('error' in read) {
    return (
      <div className="inputs-panel">
        <p className="error">The scenario YAML cannot be read: {read.error}. Fix it in the YAML tab.</p>
      </div>
    );
  }
  const m = read.model;
  const signals = [...new Set([...bySignal.values()].flat())];

  const add = (src: { template: PayloadTemplate } | { data: string } | { file: string }) => {
    const data = 'template' in src ? src.template.json() : 'data' in src ? src.data : undefined;
    const signal = 'template' in src ? src.template.signal : data ? summarizeSignal(data) : '';
    const receiver = (signal && receiverFor(signal, bySignal)) || (receivers.length === 1 ? receivers[0] : '');
    if (apply(addInput({ receiver, ...(data !== undefined ? { data, raw: true } : { file: 'file' in src ? src.file : '' }) }))) {
      setOpen(m.inputs.length);
    }
  };
  const addItems = (): MenuItem[] => [
    ...templatesFor(signals).map((t, i) => ({
      group: i === 0 ? 'From a template' : undefined,
      label: t.label,
      detail: `${t.signal} · ${t.description}`,
      onSelect: () => add({ template: t }),
    })),
    { group: 'From live telemetry', label: 'From a capture…', detail: captureDenied ?? 'Choose an existing capture or start a new one', disabled: !!captureDenied, onSelect: () => setCaptureOpen(true) },
    {
      group: 'From your data',
      label: 'Empty JSON',
      detail: 'Paste or type an OTLP JSON payload',
      onSelect: () => add({ data: '' }),
    },
    {
      label: 'Open a JSON file…',
      detail: 'Read a local OTLP JSON file into the scenario',
      onSelect: async () => {
        const f = await openTextFile('.json');
        if (f) add({ data: f.text });
      },
    },
    {
      label: 'A file path',
      detail: 'Read by the server when the run starts, relative to the scenario',
      onSelect: () => add({ file: '' }),
    },
  ];

  return (
    <div className="inputs-panel">
      {problem && <p className="error">{problem}</p>}
      {diagnostics.map((d, i) => (
        <p key={i} className="error">
          {d.message}
        </p>
      ))}
      {m.inputs.length === 0 ? (
        <StartHere onCapture={() => setCaptureOpen(true)} captureDenied={captureDenied} signals={signals} onAdd={add} fileReceivers={fileReceivers.map((c) => c.id)} onMocks={onMocks} hasFiles={m.files.length > 0} />
      ) : (
        <div className="input-cards">
          {m.inputs.map((input, i) => (
            <InputCard
              key={i}
              index={i}
              count={m.inputs.length}
              input={input}
              open={open === i}
              onToggle={() => setOpen(open === i ? -1 : i)}
              receivers={receivers}
              bySignal={bySignal}
              apply={apply}
              clientAddress={kube || input.clientAddress !== ''}
              defaultHeaders={m.headers}
              run={() => runInput(i)}
              blocker={blocker(i)}
              running={running}
              onRemoved={() => setOpen(Math.max(0, Math.min(open, m.inputs.length - 2)))}
              onMoved={(to) => setOpen(to)}
            />
          ))}
          <div className="add-row">
            <Menu label="+ Add input" className="primary-outline" items={addItems()} />
            <span className="muted small">
              {m.inputs.length === 1 && !m.assertions.length
                ? 'Add assertions to make this scenario a test.'
                : 'Inputs are sent in order; each can wait before it is sent.'}
            </span>
          </div>
        </div>
      )}
      {captureOpen && <CaptureInputPicker onClose={() => setCaptureOpen(false)} onAdded={() => { setOpen(m.inputs.length); setCaptureOpen(false); }} />}
      <RunSettings model={m} apply={apply} />
    </div>
  );
}

function summarizeSignal(data: string): string {
  const s = summarizePayload(data);
  return 'error' in s ? '' : s.signal;
}

/** StartHere is the empty state: the ways to give the run its first input. */
function StartHere({
  onCapture,
  captureDenied,
  signals,
  onAdd,
  fileReceivers,
  onMocks,
  hasFiles,
}: {
  onCapture(): void;
  captureDenied?: string;
  signals: string[];
  onAdd(src: { template: PayloadTemplate } | { data: string } | { file: string }): void;
  fileReceivers: string[];
  onMocks(): void;
  hasFiles: boolean;
}) {
  const templates = templatesFor(signals);
  return (
    <div className="start-here">
      <h3>What should go through the pipeline?</h3>
      <p className="muted">
        A run needs only the configuration and one OTLP JSON payload. Start from an example and edit it, or bring your own.
      </p>
      {fileReceivers.length > 0 && (
        <p className="note">
          <code>{fileReceivers.join(', ')}</code> {fileReceivers.length === 1 ? 'reads' : 'read'} files, not payloads.{' '}
          {hasFiles ? 'The mocked files are its input: you can run now, or add a payload too.' : 'Give it files to read under'}{' '}
          {!hasFiles && (
            <button type="button" className="text-link" onClick={onMocks}>
              Mocks
            </button>
          )}
        </p>
      )}
      <button type="button" className="primary-outline capture-input-entry" disabled={!!captureDenied} title={captureDenied} onClick={onCapture}>From a capture… <span>Use an existing capture or start a new one</span></button>
      <div className="template-grid">
        {templates.map((t) => (
          <button
            key={t.id}
            type="button"
            className={`template-card${signals.length && !signals.includes(t.signal) ? ' dim' : ''}`}
            onClick={() => onAdd({ template: t })}
          >
            <span className={`signal-tag signal-${t.signal}`}>{t.signal}</span>
            <strong>{t.label}</strong>
            <span className="muted small">{t.description}</span>
          </button>
        ))}
      </div>
      <div className="form-actions">
        <button type="button" onClick={() => onAdd({ data: '' })}>
          Paste JSON
        </button>
        <button
          type="button"
          onClick={async () => {
            const f = await openTextFile('.json');
            if (f) onAdd({ data: f.text });
          }}
        >
          Open a JSON file…
        </button>
        <button type="button" onClick={() => onAdd({ file: '' })} title="Read by the server when the run starts">
          Use a file path
        </button>
        <SavedFixtures onPick={(text) => onAdd({ data: text })} />
      </div>
    </div>
  );
}

function ReceiverSelect({ value, receivers, bySignal, onChange }: { value: string; receivers: string[]; bySignal: Map<string, string[]>; onChange(v: string): void }) {
  const unknown = value && !receivers.includes(value);
  return (
    <select value={value} onChange={(e) => onChange(e.target.value)} aria-label="Receiver" title="Where the payload enters the pipelines" onClick={(e) => e.stopPropagation()}>
      <option value="">{receivers.length === 1 ? `${receivers[0]}` : 'matching receiver'}</option>
      {unknown && <option value={value}>{value} (not in a pipeline)</option>}
      {receivers.length > 1 &&
        receivers.map((r) => (
          <option key={r} value={r}>
            {r} · {(bySignal.get(r) ?? []).join(', ')}
          </option>
        ))}
    </select>
  );
}

function InputCard({
  index,
  count,
  input,
  open,
  onToggle,
  receivers,
  bySignal,
  apply,
  clientAddress,
  run,
  blocker,
  running,
  onRemoved,
  onMoved,
  defaultHeaders,
}: {
  defaultHeaders: Header[];
  index: number;
  count: number;
  input: InputModel;
  open: boolean;
  onToggle(): void;
  receivers: string[];
  bySignal: Map<string, string[]>;
  apply(e: Edit): boolean;
  clientAddress: boolean;
  run(): void;
  blocker?: string;
  running: boolean;
  onRemoved(): void;
  onMoved(to: number): void;
}) {
  const inline = input.data !== undefined;
  const what = inline ? describeData(input.data ?? '') : { text: input.file ? `file ${input.file}` : 'no file named', error: !input.file };
  const items: MenuItem[] = [
    { label: 'Duplicate', onSelect: () => apply(duplicateItem('inputs', index)) && onMoved(index + 1) },
    { label: 'Move up', disabled: index === 0, onSelect: () => apply(moveItem('inputs', index, index - 1)) && onMoved(index - 1) },
    { label: 'Move down', disabled: index === count - 1, onSelect: () => apply(moveItem('inputs', index, index + 1)) && onMoved(index + 1) },
    inline
      ? { label: 'Read from a file path instead', detail: 'The server reads the file when the run starts', onSelect: () => apply(setInputSource(index, { file: '' })) }
      : { label: 'Inline the data instead', onSelect: () => apply(setInputData(index, '')) },
    ...(inline && input.data?.trim()
      ? [{ label: 'Download JSON', onSelect: () => downloadText(`input-${index + 1}.json`, input.data ?? '', 'application/json') }]
      : []),
    { label: 'Remove', onSelect: () => apply(removeItem('inputs', index)) && onRemoved() },
  ];
  return (
    <div className={`input-card${open ? ' open' : ''}`}>
      <div className="input-card-head" onClick={onToggle}>
        <button type="button" className="disclosure" aria-expanded={open} aria-label={`${open ? 'Collapse' : 'Expand'} input ${index + 1}`}>
          {open ? '▾' : '▸'}
        </button>
        <strong className="input-index">Input {index + 1}</strong>
        <span className="muted">→</span>
        <ReceiverSelect value={input.receiver} receivers={receivers} bySignal={bySignal} onChange={(v) => apply(setField(['inputs', index, 'receiver'], v))} />
        <span className={`input-what${what.error ? ' warn-text' : ''}`} title={what.text}>
          {what.text}
        </span>
        {input.after && <span className="chip" title="Waits this long after the previous input">after {input.after}</span>}
        {headersChip(input.headers, defaultHeaders) && (
          <span className="chip mono" title="The request headers this input is sent with">
            {headersChip(input.headers, defaultHeaders)}
          </span>
        )}
        <span className="grow" />
        <button
          type="button"
          className="run-one"
          disabled={!!blocker || running}
          title={blocker ?? 'Run only this input, with the mocks'}
          onClick={(e) => {
            e.stopPropagation();
            run();
          }}
        >
          ▶ Run
        </button>
        <span onClick={(e) => e.stopPropagation()}>
          <Menu label="⋯" className="icon" title="More actions" items={items} align="right" />
        </span>
      </div>
      {open && (
        <div className="input-card-body">
          {(index > 0 || input.after !== '' || clientAddress) && (
            <div className="form-line options">
              {(index > 0 || input.after !== '') && (
                <label className="inline" title="A Go duration the run waits before sending this input, measured from the previous one: lets batch timeouts and sampling decisions pass in between">
                  Wait before
                  <CommitInput key={input.after} className="short" value={input.after} placeholder="none, e.g. 5s" onCommit={(v) => apply(setField(['inputs', index, 'after'], v.trim()))} />
                </label>
              )}
              {clientAddress && (
                <label className="inline" title="The sender IP address (optionally with a port), as the receiver would see the connection; k8sattributes associates pods by it">
                  Sent from
                  <CommitInput
                    key={input.clientAddress}
                    className="address mono"
                    value={input.clientAddress}
                    placeholder="10.42.0.17"
                    onCommit={(v) => apply(setField(['inputs', index, 'client_address'], v.trim()))}
                  />
                </label>
              )}
            </div>
          )}
          <details className="input-headers" open={input.headers.length > 0 || undefined}>
            <summary>
              Headers <span className="muted small">{input.headers.length ? `${input.headers.length} set here` : 'none set here'}{defaultHeaders.length ? ` · ${defaultHeaders.length} from every input` : ''}</span>
            </summary>
            <p className="hint">
              Sent with this input as HTTP headers or gRPC metadata. The pipeline sees them only when the receiver sets <code>include_metadata: true</code>; a batch processor's <code>metadata_keys</code> groups by them.
            </p>
            <HeadersEditor label={`Headers of input ${index + 1}`} entries={input.headers} defaults={defaultHeaders} onChange={(h) => apply(setHeaders(['inputs', index, 'headers'], h))} />
          </details>
          {inline ? (
            <InlineData index={index} data={input.data ?? ''} apply={apply} signals={bySignal.get(input.receiver) ?? [...new Set([...bySignal.values()].flat())]} />
          ) : (
            <div className="form-line">
              <label className="inline grow">
                File
                <CommitInput
                  key={input.file}
                  className="grow mono"
                  value={input.file ?? ''}
                  placeholder="input.json"
                  title="The server reads no host file: use inline data, or open the scenario with its files"
                  onCommit={(v) => apply(setInputSource(index, { file: v.trim() }))}
                />
              </label>
              <span className="muted small">the server reads no host file: use inline data</span>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

/**
 * InlineData edits an input's OTLP JSON in place. Edits are buffered and
 * written to the document shortly after typing stops, on blur and when the
 * editor closes, exactly as typed.
 */
function InlineData({ index, data, apply, signals }: { index: number; data: string; apply(e: Edit): boolean; signals: string[] }) {
  const [view, setView] = useState<'json' | 'preview'>('json');
  const [text, setText] = useState(data);
  const committed = useRef(data);
  const latest = useRef(text);
  latest.current = text;
  const commit = () => {
    if (latest.current === committed.current) return;
    committed.current = latest.current;
    apply(setInputData(index, latest.current));
  };
  // The document changed elsewhere (YAML tab, undo, a template): show it.
  useEffect(() => {
    if (data !== committed.current) {
      committed.current = data;
      setText(data);
    }
  }, [data]);
  useEffect(() => {
    if (text === committed.current) return;
    const t = setTimeout(commit, 400);
    return () => clearTimeout(t);
  });
  useEffect(() => commit, []); // eslint-disable-line react-hooks/exhaustive-deps

  const payload = useMemo(() => {
    try {
      return JSON.parse(text) as unknown;
    } catch {
      return undefined;
    }
  }, [text]);
  const markers = useMemo((): Marker[] => {
    const p = text.trim() ? jsonProblem(text) : undefined;
    return p
      ? [{ severity: 'error', code: 'json', message: p.message, location: { startLine: p.line, startColumn: p.column, endLine: p.line, endColumn: p.column + 1 } }]
      : [];
  }, [text]);
  const replace = (t: string) => {
    setText(t);
    committed.current = t;
    apply(setInputData(index, t));
  };
  const templates = templatesFor(signals);

  const empty = !text.trim();
  // The editor keeps its place in the tree, so typing the first character
  // into an empty input does not remount it.
  return (
    <div className={`inline-data${empty ? ' empty' : ''}`}>
      {empty ? (
        <div className="inline-data-start">
          <p className="muted">Start from an example, or paste OTLP JSON below.</p>
          <div className="template-grid compact">
            {templates.map((t) => (
              <button key={t.id} type="button" className={`template-card${signals.length && !signals.includes(t.signal) ? ' dim' : ''}`} onClick={() => replace(t.json())}>
                <span className={`signal-tag signal-${t.signal}`}>{t.signal}</span>
                <strong>{t.label}</strong>
              </button>
            ))}
          </div>
          <div className="form-actions">
            <button
              type="button"
              onClick={async () => {
                const f = await openTextFile('.json');
                if (f) replace(f.text);
              }}
            >
              Open a JSON file…
            </button>
            <SavedFixtures onPick={replace} />
          </div>
        </div>
      ) : (
        <div className="inline-data-bar">
          <div className="tabs subtabs" role="tablist" aria-label="Input view">
            <button type="button" role="tab" aria-selected={view === 'json'} onClick={() => setView('json')}>
              JSON
            </button>
            <button type="button" role="tab" aria-selected={view === 'preview'} onClick={() => setView('preview')}>
              Preview
            </button>
          </div>
          <span className="grow" />
          <button type="button" disabled={payload === undefined} title="Indent the JSON" onClick={() => replace(JSON.stringify(payload, null, 2) + '\n')}>
            Format
          </button>
          <button
            type="button"
            onClick={async () => {
              const f = await openTextFile('.json');
              if (f) replace(f.text);
            }}
            title="Replace this input with a local OTLP JSON file"
          >
            Open…
          </button>
          <SavedFixtures onPick={replace} save={text} />
        </div>
      )}
      {view === 'preview' && !empty ? (
        <div className="input-preview">
          <SignalExplorer payload={payload} label="Input preview" emptyMessage="Fix the JSON to preview its signals." />
        </div>
      ) : (
        <div className="editor-host">
          <YamlEditor path={`input-${index}.json`} language={JSON_LANGUAGE} value={text} onChange={setText} onBlur={commit} markers={markers} />
        </div>
      )}
    </div>
  );
}

/**
 * SavedFixtures opens OTLP JSON payloads saved on the server and saves an
 * input's payload there as a new artifact. It shows nothing when the server
 * cannot list saved work.
 */
function SavedFixtures({ onPick, save }: { onPick(text: string): void; save?: string }) {
  const qc = useQueryClient();
  const list = useQuery({ queryKey: ['artifacts', 'fixture'], queryFn: () => api.artifacts('fixture'), retry: false });
  const [message, setMessage] = useState<string>();
  if (!list.data) return null;
  const items: MenuItem[] = (list.data.items ?? []).map((a) => ({
    label: a.label || a.id,
    detail: `revision ${a.generation} · ${new Date(a.updated).toLocaleString()}`,
    onSelect: async () => {
      try {
        const r = await api.artifactRevision(a.id, a.head_revision_id);
        onPick(decodeText(r.content_base64 ?? ''));
      } catch (e) {
        setMessage(e instanceof Error ? e.message : String(e));
      }
    },
  }));
  if (save?.trim()) {
    items.push({
      group: items.length ? 'Save' : undefined,
      label: 'Save this input on the server',
      onSelect: async () => {
        try {
          const label = window.prompt('Name the saved input', 'input.json');
          if (!label) return;
          const saved = await api.saveArtifact({ workspace_id: list.data.workspace.id, kind: 'fixture', label, expected_generation: 0, content_base64: encodeText(save) });
          setMessage(`Saved as “${saved.artifact.label}”.`);
          void qc.invalidateQueries({ queryKey: ['artifacts', 'fixture'] });
        } catch (e) {
          setMessage(e instanceof Error ? e.message : String(e));
        }
      },
    });
  }
  if (!items.length) items.push({ label: 'Nothing saved yet', disabled: true, onSelect: () => {} });
  return (
    <>
      <Menu label="Saved…" items={items} title="Inputs saved on this server" align="right" />
      {message && <span className="muted small" role="status">{message}</span>}
    </>
  );
}

/** RunSettings edits the document's name and execution settings. */
function RunSettings({ model: m, apply }: { model: { name: string; timeout: string; settle: string; timestamps: string; headers: Header[] }; apply(e: Edit): boolean }) {
  const set = [m.name && 'name', m.timeout && 'timeout', m.settle && 'settle', m.timestamps === 'rebase' && 'rebased timestamps', m.headers.length && 'headers'].filter(Boolean);
  return (
    <details className="run-settings">
      <summary>
        Run settings <span className="muted small">{set.length ? set.join(' · ') : 'defaults'}</span>
      </summary>
      <div className="form-section">
        <div className="form-grid">
          <label>
            Name
            <CommitInput key={m.name} value={m.name} placeholder="what this scenario checks" onCommit={(v) => apply(setField(['name'], v))} />
          </label>
          <label title="A Go duration, for example 30s">
            Timeout
            <CommitInput key={m.timeout} value={m.timeout} placeholder="default" onCommit={(v) => apply(setField(['execution', 'timeout'], v.trim()))} />
          </label>
          <label title="How long the run waits after the last input before the Collector shuts down, for components that emit on a timer. Empty: derived from the config (tail_sampling with drop_pending_traces_on_shutdown waits for decision_wait).">
            Settle
            <CommitInput key={m.settle} value={m.settle} placeholder="derived" onCommit={(v) => apply(setField(['execution', 'settle'], v.trim()))} />
          </label>
          <label title="Rebase shifts every input timestamp so the latest record falls at the run's start, keeping their spacing. Use it when the config compares record times with Now().">
            Timestamps
            <select value={m.timestamps === 'rebase' ? 'rebase' : ''} onChange={(e) => apply(setField(['execution', 'timestamps'], e.target.value))}>
              <option value="">as recorded</option>
              <option value="rebase">rebased to the run's time</option>
            </select>
          </label>
        </div>
        <h4>Headers for every input</h4>
        <p className="hint">Each input sends these unless it sets a header of the same name itself.</p>
        <HeadersEditor label="Headers for every input" entries={m.headers} onChange={(h) => apply(setHeaders(['defaults', 'headers'], h))} />
      </div>
    </details>
  );
}
