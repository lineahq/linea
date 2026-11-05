import { useSyncExternalStore } from 'react';
import type { Draft, SavedKind, SavedSource } from './drafts';
import { migrate } from './migrate';

/**
 * The workspace is what the user edits: Collector configuration text and
 * one scenario document holding the inputs, mocks and assertions. A quick
 * run is a document with one input (see input/quickInput.ts runPlan). It is
 * a copy; the user's files are never written. Drafts persist in
 * localStorage as a convenience only.
 */
export interface WorkspaceState {
	/** Sensitive derived drafts stay in this browser session; never localStorage. */
	sessionOnly?: boolean;
  configYAML: string;
  configPath: string;
  scenarioYAML: string;
  scenarioPath: string;
  compare: boolean;
  /**
   * The runtime runs, validation and previews use, chosen beside Run:
   * a built runtime's name, or empty (unset) for the built-in one.
   */
  runtime?: string;
  /**
   * Each draft's text when it was last loaded or saved (see drafts.ts
   * isDirty); unset until then.
   */
  baselines?: Partial<Record<Draft, string>>;
  /** The saved revisions sources were opened from or saved as. */
  saved?: Partial<Record<SavedKind, SavedSource>>;
  /** When the configuration or scenario text last changed (ISO time). */
  editedAt?: string;
  /** The last run started from this workspace. */
  lastRunId?: string;
}

const KEY = 'linea.workspace.v1';

const empty: WorkspaceState = {
  configYAML: '',
  configPath: '',
  scenarioYAML: '',
  scenarioPath: '',
  compare: false,
};

function load(): WorkspaceState {
  try {
    const raw = localStorage.getItem(KEY);
    if (raw) return migrate({ ...empty, ...(JSON.parse(raw) as Partial<WorkspaceState>) });
  } catch {
    // Storage may be unavailable; start empty.
  }
  return empty;
}

let state: WorkspaceState = typeof localStorage === 'undefined' ? empty : load();
const listeners = new Set<() => void>();

export function getWorkspace(): WorkspaceState {
  return state;
}

export function updateWorkspace(patch: Partial<WorkspaceState>): void {
  const edited =
    (patch.configYAML !== undefined && patch.configYAML !== state.configYAML) ||
    (patch.scenarioYAML !== undefined && patch.scenarioYAML !== state.scenarioYAML);
  state = { ...state, ...patch, ...(edited && !patch.editedAt ? { editedAt: new Date().toISOString() } : {}) };
  try {
	  if (!state.sessionOnly) localStorage.setItem(KEY, JSON.stringify(state));
  } catch {
    // Ignore: drafts are a convenience.
  }
  listeners.forEach((l) => l());
}
export function updateWorkspaceSessionOnly(patch: Partial<WorkspaceState>): void { updateWorkspace({ ...patch, sessionOnly: true }); }

export function useWorkspace(): WorkspaceState {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => state,
  );
}
