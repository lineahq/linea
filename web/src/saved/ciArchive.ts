import type { ArtifactImport, ArtifactMetadata } from '../api/types';
import { isDirty, kindDraft, type DraftKind } from '../workspace/drafts';
import type { WorkspaceState } from '../workspace/store';
import { plural } from './format';
import { DRAFT_CONFIG } from './suiteRequest';

/** The most scenarios one export holds, as a suite. */
export const MAX_EXPORT_SCENARIOS = 32;

/** savedConfigId is the artifact ID of a saved configuration option, or undefined for the draft. */
export function savedConfigId(config: string): string | undefined {
  return config.startsWith('saved:') ? config.slice('saved:'.length) : undefined;
}

/**
 * ciExportBlocker says why the selection cannot be exported for CI, or
 * undefined when it can: the archive holds saved revisions only, so the
 * configuration must be a saved one.
 */
export function ciExportBlocker(config: string, members: ArtifactMetadata[]): string | undefined {
  if (config === DRAFT_CONFIG) return 'Export for CI uses saved revisions: save the open configuration first, or pick a saved one.';
  if (!savedConfigId(config)) return 'Pick a saved configuration to export.';
  if (members.length === 0) return 'Select at least one scenario to export.';
  if (members.length > MAX_EXPORT_SCENARIOS) return `Select at most ${MAX_EXPORT_SCENARIOS} scenarios to export.`;
  return undefined;
}

/** importSummary counts what an import did with the archive's files. */
export function importSummary(imp: ArtifactImport): string {
  const items = imp.items ?? [];
  const count = (status: string) => items.filter((i) => i.status === status).length;
  const parts = [
    count('created') > 0 && `${count('created')} created`,
    count('revised') > 0 && `${count('revised')} revised`,
    count('unchanged') > 0 && `${count('unchanged')} unchanged`,
  ].filter(Boolean);
  return `Imported ${plural(items.length, 'file')}: ${parts.join(', ')}.`;
}

/** bytesToBase64 encodes file bytes for a JSON request, in chunks. */
export function bytesToBase64(bytes: Uint8Array): string {
  let binary = '';
  for (let i = 0; i < bytes.length; i += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  }
  return btoa(binary);
}

/** The command the exported workflow runs (examples/ci/github-actions.yml). */
export const CI_TEST_COMMAND = 'linea test --config collector/config.yaml --tests collector/tests --report junit.xml';

/** What exporting the open configuration and scenario for CI does first. */
export type OpenExportPlan = { blocker: string } | { saves: DraftKind[] };

const KINDS: DraftKind[] = ['configuration', 'scenario'];

/**
 * openExportPlan says what exporting the open configuration and scenario
 * needs. The archive holds saved head revisions, so whatever is not saved, or
 * changed since it was saved, is saved first (a new revision of its saved
 * item, or a new item). An earlier revision cannot be exported: the archive
 * would hold the newest one instead. lists are the saved items of each kind
 * (undefined when the server cannot list them).
 */
export function openExportPlan(ws: WorkspaceState, lists: Record<DraftKind, ArtifactMetadata[] | undefined>): OpenExportPlan {
  if (!lists.configuration || !lists.scenario) return { blocker: 'Export for CI needs saved work, and this server could not list it.' };
  if (!ws.configYAML.trim()) return { blocker: 'Open or write a configuration first.' };
  if (!ws.scenarioYAML.trim()) return { blocker: 'Add a scenario first: CI runs the configuration on its inputs and checks its assertions.' };
  const saves: DraftKind[] = [];
  for (const kind of KINDS) {
    const source = ws.saved?.[kind];
    if (!source || isDirty(ws, kindDraft(kind))) {
      saves.push(kind);
      continue;
    }
    const meta = lists[kind]!.find((m) => m.id === source.artifactId);
    if (meta && meta.head_revision_id !== source.revisionId) {
      return { blocker: `The open ${kind} is an earlier revision of “${meta.label}”, and an export holds the newest one. Open the newest revision, or save this one as new saved work first.` };
    }
  }
  return { saves };
}

/** exportPlanText says what Export for CI will do, before it does it. */
export function exportPlanText(saves: DraftKind[]): string {
  const first = saves.length === 2 ? 'Saves the configuration and the scenario, then downloads' : saves.length === 1 ? `Saves the ${saves[0]}, then downloads` : 'Downloads';
  return `${first} collector-ci.zip: the configuration, the scenario and a GitHub Actions workflow.`;
}
