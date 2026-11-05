import type { ArtifactMetadata, SuiteRequest } from '../api/types';
import { baseName } from '../files';
import { runtimeField } from '../runtimes/runtimes';

/**
 * Building the request that runs saved scenarios as a suite. A suite has one
 * configuration (the current draft, or a saved configuration at its head
 * revision) and 1 to 32 saved scenarios, each at its head revision. The
 * runtime is a parameter of this request only; the workspace's runtime
 * (chosen beside Run) is just the default shown.
 */

/** A suite holds at most this many scenarios (the API rejects more). */
export const MAX_SUITE_MEMBERS = 32;

/** The value of the "current draft" configuration choice. */
export const DRAFT_CONFIG = 'draft';

export interface ConfigOption {
  value: string;
  label: string;
}

export interface DraftConfig {
  yaml: string;
  path: string;
}

/**
 * configOptions lists the configurations a suite can run with: the current
 * draft (when it has text) first, then each saved configuration.
 */
export function configOptions(draft: DraftConfig, saved: ArtifactMetadata[]): ConfigOption[] {
  const out: ConfigOption[] = [];
  if (draft.yaml.trim()) out.push({ value: DRAFT_CONFIG, label: `The open configuration${draft.path ? ` · ${baseName(draft.path)}` : ''}` });
  for (const c of [...saved].sort((a, b) => b.updated.localeCompare(a.updated))) {
    out.push({ value: `saved:${c.id}`, label: `${c.label || c.id} · rev ${c.generation}` });
  }
  return out;
}

/** The choice a launcher starts on: the first option, or none. */
export function defaultConfig(options: ConfigOption[]): string {
  return options[0]?.value ?? '';
}

/** suiteBlocker says why a suite cannot be submitted yet, or undefined. */
export function suiteBlocker(members: number, config: string, options: ConfigOption[]): string | undefined {
  if (members === 0) return 'Select at least one scenario.';
  if (members > MAX_SUITE_MEMBERS) return `A suite holds at most ${MAX_SUITE_MEMBERS} scenarios; ${members} are selected.`;
  if (!options.some((o) => o.value === config)) return 'Choose a configuration: write one in the debugger or save one.';
  return undefined;
}

/**
 * buildSuiteRequest names the configuration and every scenario by saved
 * revision (the head each was listed at), so the suite runs exactly what the
 * lists showed. The draft configuration is sent as text. Undefined when the
 * chosen configuration is not available.
 */
export function buildSuiteRequest(
  config: string,
  draft: DraftConfig,
  saved: ArtifactMetadata[],
  members: ArtifactMetadata[],
  runtime?: string,
): SuiteRequest | undefined {
  const scenarios = members.map((m) => ({
    scenario_ref: { artifact_id: m.id, revision_id: m.head_revision_id },
    scenario_label: m.label || m.id,
  }));
  if (config === DRAFT_CONFIG) {
    if (!draft.yaml.trim()) return undefined;
    return { config_yaml: draft.yaml, config_label: (draft.path && baseName(draft.path)) || 'untitled configuration', scenarios, ...runtimeField(runtime) };
  }
  const found = saved.find((c) => `saved:${c.id}` === config);
  if (!found) return undefined;
  return {
    config_ref: { artifact_id: found.id, revision_id: found.head_revision_id },
    config_label: found.label || found.id,
    scenarios,
    ...runtimeField(runtime),
  };
}
