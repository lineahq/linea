import type { RunInputs, ScenarioFromRun } from '../api/types';
import { baseName } from '../files';
import { quickDoc } from '../input/quickInput';
import { readScenario } from '../scenario/scenarioDoc';
import type { WorkspaceState } from './store';

/**
 * The two drafts the editors hold: the configuration and the scenario
 * document (inputs, mocks and assertions; a quick run is a document with
 * one input).
 */
export type Draft = 'config' | 'scenario';

/**
 * The saved artifact kinds. Configurations and scenarios are drafts; a
 * fixture is one input's payload, opened into or saved from an input.
 */
export type SavedKind = 'configuration' | 'fixture' | 'scenario';

/** The saved kinds that hold a whole draft. */
export type DraftKind = Exclude<SavedKind, 'fixture'>;

/** A saved revision a source was opened from or saved as. */
export interface SavedSource {
  artifactId: string;
  revisionId: string;
}

export const DRAFT_NAMES: Record<Draft, string> = {
  config: 'configuration',
  scenario: 'scenario',
};

/** draftText is what a draft's dirty check compares. */
export function draftText(ws: WorkspaceState, draft: Draft): string {
  return draft === 'config' ? ws.configYAML : ws.scenarioYAML;
}

function empty(ws: WorkspaceState, draft: Draft): boolean {
  return !draftText(ws, draft).trim();
}

/**
 * isDirty: a draft is dirty when it differs from what was last loaded into it
 * or saved from it. Without such a baseline, any content is dirty.
 */
export function isDirty(ws: WorkspaceState, draft: Draft): boolean {
  const baseline = ws.baselines?.[draft];
  return baseline === undefined ? !empty(ws, draft) : draftText(ws, draft) !== baseline;
}

/**
 * loaded returns the patch that applies `patch` and records the drafts it
 * loaded as their new baselines, so they start clean.
 */
export function loaded(ws: WorkspaceState, patch: Partial<WorkspaceState>, drafts: Draft[]): Partial<WorkspaceState> {
  const next = { ...ws, ...patch };
  const baselines = { ...ws.baselines };
  for (const d of drafts) baselines[d] = draftText(next, d);
  return { ...patch, baselines };
}

/** What using a run's inputs as drafts would change. */
export interface RunDraft {
  patch: Partial<WorkspaceState>;
  /** The drafts replaced: the configuration and the run mode's own. */
  drafts: Draft[];
  /** What the drafts cannot hold, said plainly. */
  notes: string[];
}

function saved(ref?: { artifact_id: string; revision_id: string }): SavedSource | undefined {
  return ref ? { artifactId: ref.artifact_id, revisionId: ref.revision_id } : undefined;
}

/**
 * runDraft maps a run's admitted inputs onto the workspace: the configuration
 * and the document are replaced. A quick run becomes a document with its
 * payload as the only input, with its mocks and assertions.
 */
export function runDraft(ws: WorkspaceState, inputs: RunInputs): RunDraft {
  const quick = inputs.mode === 'quick';
  const patch: Partial<WorkspaceState> = {
    configYAML: inputs.config.text,
    configPath: inputs.config.label ?? '',
  };
  const sources = { ...ws.saved, configuration: saved(inputs.config.artifact) };
  const notes: string[] = [];
  if (quick) {
    const doc = quickDoc({
      fixture: inputs.fixture?.text ?? '',
      receiver: inputs.receiver ?? '',
      clientAddress: inputs.client_address,
      setup: inputs.setup?.text,
      assertions: inputs.assertions?.text,
    });
    if ('error' in doc) notes.push(`The run's mocks or assertions could not be read (${doc.error}); only its input was opened.`);
    const yaml = 'error' in doc ? quickDoc({ fixture: inputs.fixture?.text ?? '', receiver: inputs.receiver ?? '' }) : doc;
    Object.assign(patch, { scenarioYAML: 'error' in yaml ? '' : yaml.yaml, scenarioPath: '' });
    sources.scenario = undefined;
  } else {
    // The run's scenario directory is a server path: relative files are
    // named below, never guessed.
    Object.assign(patch, { scenarioYAML: inputs.scenario?.text ?? '', scenarioPath: inputs.scenario?.label ?? '' });
    sources.scenario = saved(inputs.scenario?.artifact);
  }
  patch.saved = sources;

  const files = inputs.files ?? [];
  if (files.length) {
    notes.push(`The run read ${files.length === 1 ? 'a file' : `${files.length} files`} beside its scenario (${files.join(', ')}); a rerun needs them there, or their data inlined.`);
  }
  const env = inputs.required_environment ?? [];
  if (env.length) {
    notes.push(`The configuration reads environment ${env.length === 1 ? 'value' : 'values'} ${env.join(', ')}; values are never copied.`);
  }
  const drafts: Draft[] = ['config', 'scenario'];
  return { patch: loaded(ws, patch, drafts), drafts, notes };
}

/**
 * runExportNeedsScenario: a run that read files beside its scenario needs the
 * self-contained scenario derived from it (`api.scenarioFromRun`) before it
 * can be exported for CI, since the archive holds only the scenario.
 */
export function runExportNeedsScenario(inputs: RunInputs): boolean {
  return (inputs.files ?? []).length > 0;
}

/**
 * runExportDraft opens a run's configuration and scenario for Export for CI:
 * the run's own sources (saved ones stay linked, so the export reuses them),
 * or with `derived`, the self-contained scenario in place of one that read
 * files beside it, unsaved.
 */
export function runExportDraft(ws: WorkspaceState, inputs: RunInputs, derived?: ScenarioFromRun): RunDraft {
  const d = runDraft(ws, inputs);
  if (!derived) return d;
  const patch: Partial<WorkspaceState> = {
    ...d.patch,
    scenarioYAML: derived.yaml,
    scenarioPath: '',
    saved: { ...d.patch.saved, scenario: undefined },
    baselines: { ...d.patch.baselines, scenario: undefined },
  };
  return { patch, drafts: d.drafts, notes: d.notes.filter((n) => !n.startsWith('The run read')) };
}

/**
 * scenarioFromRunDraft puts a scenario derived from a run into the scenario
 * draft and switches to scenario mode. The scenario is not saved anywhere, so
 * it has no baseline: it counts as unsaved until it is saved.
 */
export function scenarioFromRunDraft(ws: WorkspaceState, sc: ScenarioFromRun, runConfig: string): RunDraft {
  const patch: Partial<WorkspaceState> = {
    scenarioYAML: sc.yaml,
    scenarioPath: '',
    saved: { ...ws.saved, scenario: undefined },
    baselines: { ...ws.baselines, scenario: undefined },
  };
  const notes: string[] = [];
  if (ws.configYAML !== runConfig) {
    notes.push(`The scenario belongs to the run's configuration, which differs from your open configuration: "Open the run's configuration and scenario" loads it.`);
  }
  const env = sc.required_environment ?? [];
  if (env.length) {
    notes.push(`A rerun needs environment ${env.length === 1 ? 'value' : 'values'} ${env.join(', ')}; values are never copied.`);
  }
  return { patch, drafts: ['scenario'], notes };
}

/** dirtyDrafts lists which of the drafts would lose unsaved edits. */
export function dirtyDrafts(ws: WorkspaceState, drafts: Draft[]): Draft[] {
  return drafts.filter((d) => isDirty(ws, d));
}

/** confirmText asks before replacing dirty drafts. */
export function confirmText(dirty: Draft[]): string {
  const names = dirty.map((d) => DRAFT_NAMES[d]);
  const list = names.length > 1 ? `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}` : names[0];
  return `Replace your unsaved ${list}?`;
}

/** The draft a saved kind loads into. */
export function kindDraft(kind: DraftKind): Draft {
  return kind === 'configuration' ? 'config' : 'scenario';
}

/**
 * openedPatch loads text into the draft of its kind as its new baseline:
 * from a saved revision (source), or from a local file (no source).
 */
export function openedPatch(ws: WorkspaceState, kind: DraftKind, text: string, label: string, source?: SavedSource): Partial<WorkspaceState> {
  const fields: Partial<WorkspaceState> =
    kind === 'configuration' ? { configYAML: text, configPath: label } : { scenarioYAML: text, scenarioPath: label };
  return loaded(ws, { ...fields, saved: { ...ws.saved, [kind]: source } }, [kindDraft(kind)]);
}

/**
 * newPatch starts over: the configuration from a template and a scenario
 * (empty unless given), neither saved anywhere. Both start clean, so leaving untouched
 * new work does not ask first; the header still says "Not saved".
 */
export function newPatch(ws: WorkspaceState, configYAML: string, scenarioYAML = ''): Partial<WorkspaceState> {
  return loaded(ws, { configYAML, configPath: '', scenarioYAML, scenarioPath: '', saved: {}, lastRunId: undefined }, ['config', 'scenario']);
}

/**
 * newLabel names a new saved artifact: a scenario by its `name:`, otherwise
 * the draft's file, otherwise its kind's file name.
 */
export function newLabel(ws: WorkspaceState, kind: DraftKind): string {
  if (kind === 'scenario' && ws.scenarioYAML.trim()) {
    const read = readScenario(ws.scenarioYAML);
    const named = 'error' in read ? '' : read.model.name.trim();
    if (named) return yamlName(named);
  }
  const path = kind === 'configuration' ? ws.configPath : ws.scenarioPath;
  return yamlName((path && baseName(path)) || (kind === 'configuration' ? 'config.yaml' : 'scenario.yaml'));
}

/** MAX_LABEL is the longest saved-work name the server accepts. */
export const MAX_LABEL = 1024;

/**
 * yamlName is name as a YAML file name: kept when it ends in .yaml or .yml
 * (any case), otherwise with .yaml appended, so config.bin saves as
 * config.bin.yaml. It stays within MAX_LABEL.
 */
export function yamlName(name: string): string {
  const trimmed = name.trim();
  if (/\.ya?ml$/i.test(trimmed)) return trimmed.slice(0, MAX_LABEL);
  return `${trimmed.slice(0, MAX_LABEL - '.yaml'.length).trimEnd()}.yaml`;
}

/** saveLabel is the name a save uses: the typed name, or fallback when it is blank, as a YAML file name. */
export function saveLabel(typed: string | undefined, fallback: string): string {
  const name = typed?.trim() ?? '';
  return yamlName(name || fallback);
}

/** The text a saved kind stores. */
export function kindText(ws: WorkspaceState, kind: DraftKind): string {
  return kind === 'configuration' ? ws.configYAML : ws.scenarioYAML;
}

/** encodeText and decodeText carry exact UTF-8 bytes as the API's base64. */
export function encodeText(text: string): string {
  let binary = '';
  for (const b of new TextEncoder().encode(text)) binary += String.fromCharCode(b);
  return btoa(binary);
}

export function decodeText(base64: string): string {
  const binary = atob(base64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return new TextDecoder('utf-8', { fatal: true }).decode(bytes);
}
