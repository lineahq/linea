import type { Report, RunInputs, ScenarioFromRun } from '../api/types';
import { appendAssertion, describeAssertion, readChecks, suggestFromRun, type Suggestion } from '../scenario/checks';
import { editScenario, readScenario, targetChoices } from '../scenario/scenarioDoc';
import type { Graph } from '../api/types';
import { scenarioFromRunDraft } from '../workspace/drafts';
import type { WorkspaceState } from '../workspace/store';

/** Where the test starts: the scenario the run used, or one derived from the run. */
export type TestBase = 'update' | 'new';

/** One assertion of the test being made. */
export interface TestRow {
  /** Stable across renders; a suggestion's key or "existing:<index>". */
  key: string;
  text: string;
  /** Existing assertions are the scenario's own; suggestions come from what the run observed. */
  origin: 'existing' | 'suggested';
}

/**
 * testBase picks where assertions from a run go: into the scenario the run
 * used, or, for a run without a scenario file (one input), into a new
 * scenario made from the run's input, mocks and settings.
 */
export function testBase(inputs: RunInputs | undefined): TestBase {
  return inputs?.mode === 'scenario' && inputs.scenario ? 'update' : 'new';
}

/** destinationText says where the assertions go, in the glossary's words. */
export function destinationText(base: TestBase, inputs: RunInputs | undefined): string {
  if (base === 'update') return `They are added to the scenario ${inputs?.scenario?.label ? `“${inputs.scenario.label}”` : 'this run used'}, which becomes the open scenario.`;
  return 'This run had no scenario file, so a new scenario is made from its input, mocks and settings, with the assertions you add.';
}

/** The scenario text the test starts from, when that base is available. */
export function baseYaml(base: TestBase, inputs: RunInputs | undefined, derived: ScenarioFromRun | undefined): string | undefined {
  if (base === 'update') return inputs?.mode === 'scenario' ? inputs.scenario?.text : undefined;
  return derived?.yaml;
}

/** existingRows words the assertions a scenario already has. */
export function existingRows(yaml: string): TestRow[] {
  const read = readScenario(yaml);
  if ('error' in read) return [];
  return read.model.assertions.map((a, i) => {
    const checks = readChecks(a.conditions);
    return { key: `existing:${i}`, origin: 'existing', text: a.name || ('checks' in checks ? describeAssertion(a.target, checks.checks) : 'an assertion only the YAML can show') };
  });
}

/**
 * newSuggestions are what the run observed that the scenario does not assert
 * yet. They are proposals: none is part of the test until confirmed.
 */
export function newSuggestions(report: Report | undefined, topology: Graph | undefined, yaml: string): Suggestion[] {
  const read = readScenario(yaml);
  const asserted = 'error' in read ? [] : read.model.assertions;
  return suggestFromRun(report, targetChoices(topology, report), asserted).filter((s) => !s.present);
}

/** suggestionText words a suggestion as an assertion row. */
export function suggestionText(s: Suggestion): string {
  return describeAssertion(s.target, s.checks);
}

/** suggestionEvidence says what the run observed behind a suggestion. */
export function suggestionEvidence(s: Suggestion): string {
  const c = s.checks[0];
  if (c?.kind === 'count' && c.bound.op === 'equals') return `Observed ${c.bound.n} record${c.bound.n === 1 ? '' : 's'} in this run`;
  if (c?.kind === 'dropped' && c.bound.op === 'equals') return `Observed ${c.bound.n} dropped in this run`;
  return 'Observed in this run';
}

/** withConfirmed appends the confirmed suggestions to the scenario's assertions. */
export function withConfirmed(yaml: string, confirmed: Suggestion[]): { yaml: string } | { error: string } {
  if (!confirmed.length) return { yaml };
  return editScenario(yaml, (doc) => confirmed.forEach((s) => appendAssertion(s.target, s.checks)(doc)));
}

/**
 * testDraftPatch is the workspace patch that makes the test the scenario
 * draft. Updating the run's scenario keeps its saved source, so saving adds a
 * revision to it; a derived scenario is unsaved. An empty configuration draft
 * takes the run's configuration.
 */
export function testDraftPatch(
  ws: WorkspaceState,
  base: TestBase,
  yaml: string,
  inputs: RunInputs,
  required?: string[],
): { patch: Partial<WorkspaceState>; notes: string[] } {
  const d = scenarioFromRunDraft(ws, { run_id: inputs.run_id, yaml, required_environment: required }, inputs.config.text);
  if (base === 'update' && inputs.scenario) {
    const ref = inputs.scenario.artifact;
    d.patch.scenarioPath = inputs.scenario.label ?? '';
    d.patch.saved = { ...ws.saved, scenario: ref ? { artifactId: ref.artifact_id, revisionId: ref.revision_id } : undefined };
    d.patch.baselines = { ...ws.baselines, scenario: inputs.scenario.text };
  }
  // A run opened in a fresh session has no configuration draft: the test needs the run's.
  if (!ws.configYAML.trim()) {
    const ref = inputs.config.artifact;
    d.patch.configYAML = inputs.config.text;
    d.patch.configPath = inputs.config.label ?? '';
    d.patch.saved = { ...d.patch.saved, configuration: ref ? { artifactId: ref.artifact_id, revisionId: ref.revision_id } : undefined };
    d.patch.baselines = { ...d.patch.baselines, config: inputs.config.text };
  }
  return { patch: d.patch, notes: d.notes };
}
