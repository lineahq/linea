import type { ArtifactMetadata, Run } from '../api/types';
import { baseName } from '../files';
import { readScenario } from '../scenario/scenarioDoc';
import { isDirty, kindDraft, type DraftKind } from '../workspace/drafts';
import type { WorkspaceState } from '../workspace/store';
import { runtimeText, sourceText, whenText } from './runFacts';

/** elapsedText words a duration: "1.1 s", "2 min 3 s". */
export function elapsedText(ms: number): string {
  const s = Math.max(0, ms) / 1000;
  if (s < 60) return `${s.toFixed(1)} s`;
  const whole = Math.floor(s);
  return `${Math.floor(whole / 60)} min ${whole % 60} s`;
}

/** runDuration is how long a run took, once it started and ended. */
export function runDuration(run: Pick<Run, 'started' | 'finished'>): string | undefined {
  if (!run.started || !run.finished) return undefined;
  const ms = new Date(run.finished).getTime() - new Date(run.started).getTime();
  return Number.isNaN(ms) ? undefined : elapsedText(ms);
}

/**
 * runMeta is the line beside a run's name: when it started, how long it ran
 * and on which runtime. Parts the run does not know are left out.
 */
export function runMeta(run: Run, now?: Date): string {
  const state = run.state === 'done' ? undefined : run.state === 'error' ? 'failed' : run.state;
  const duration = runDuration(run);
  return [whenText(run.created, now), state && duration ? `${state} after ${duration}` : (duration ?? state), runtimeText(run.report)]
    .filter(Boolean)
    .join(' · ');
}

/** runConfigName names the configuration a run used. */
export function runConfigName(run: Run | undefined): string {
  return sourceText(run?.report) ?? 'configuration';
}

/** The tone of a save state: saved, edited since saved, or never saved. */
export type SaveTone = 'saved' | 'changed' | 'unsaved';

/** What the header says about the open configuration or scenario. */
export interface SaveState {
  name: string;
  tone: SaveTone;
  /** "Not saved", "Unsaved changes", "Saved · revision 3" (docs/glossary.md). */
  text: string;
}

/**
 * saveState names the open configuration or scenario and says whether it is
 * in saved work: "Not saved" until it is saved (a file or an example is not),
 * "Unsaved changes" once edited after opening or saving, otherwise the
 * revision it matches. items are the saved artifacts of the kind, when known.
 */
export function saveState(ws: WorkspaceState, kind: DraftKind, items: ArtifactMetadata[] | undefined): SaveState {
  const source = ws.saved?.[kind];
  const meta = source ? items?.find((m) => m.id === source.artifactId) : undefined;
  const name = meta?.label || openName(ws, kind);
  if (!source) return { name, tone: 'unsaved', text: 'Not saved' };
  if (isDirty(ws, kindDraft(kind))) return { name, tone: 'changed', text: 'Unsaved changes' };
  if (!meta) return { name, tone: 'saved', text: 'Saved' };
  return { name, tone: 'saved', text: meta.head_revision_id === source.revisionId ? `Saved · revision ${meta.generation}` : 'Saved · earlier revision' };
}

/** openName names what is open by its file or its scenario name. */
function openName(ws: WorkspaceState, kind: DraftKind): string {
  if (kind === 'configuration') return ws.configPath ? baseName(ws.configPath) : 'Untitled configuration';
  const read = ws.scenarioYAML.trim() ? readScenario(ws.scenarioYAML) : undefined;
  const named = read && !('error' in read) ? read.model.name : '';
  return named || (ws.scenarioPath ? baseName(ws.scenarioPath) : 'Untitled scenario');
}

/** progressFacts counts what a live run has recorded so far, from its events. */
export function progressFacts(events: readonly { kind: string; component?: string; pipeline?: string }[]): { events: number; components: number } {
  const seen = new Set<string>();
  for (const e of events) if (e.component) seen.add(`${e.pipeline ?? ''}|${e.component}`);
  return { events: events.length, components: seen.size };
}
