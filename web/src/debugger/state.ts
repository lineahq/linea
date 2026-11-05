import type { Diagnostic } from '../api/types';
import { runActive } from '../api/runState';

/**
 * The debugger's working states: start (nothing loaded), prepare (editing
 * the next experiment), running (a queued or running run), investigate (a
 * finished run) and compare (a run compared with another).
 */
export type DebuggerState = 'start' | 'prepare' | 'running' | 'investigate' | 'compare';

export interface DebuggerStateInput {
  /** The selected run once loaded; only its state matters. */
  run?: { state: string };
  /** The run named in the URL, loaded or not. */
  runId?: string;
  /** The run the selected run is compared with (the URL's compare). */
  compare?: string;
  /** The configuration draft holds text. */
  hasConfig: boolean;
  /** The start state was asked for (the URL's start), e.g. to resume or open something else. */
  start?: boolean;
}

/**
 * debuggerState says which working state the URL and the drafts put the
 * debugger in. A run in the URL is always an investigation, even while it
 * loads or when it is gone, so a bookmark never falls back to editing.
 */
export function debuggerState({ run, runId, compare, hasConfig, start }: DebuggerStateInput): DebuggerState {
  if (runId || run) {
    if (compare) return 'compare';
    if (run && runActive(run.state)) return 'running';
    return 'investigate';
  }
  return hasConfig && !start ? 'prepare' : 'start';
}

/** A way to resolve a problem where it is shown. */
export interface ProblemFix {
  action: 'config' | 'inputs' | 'capture' | 'runtimes';
  label: string;
  /** The configuration line to reveal, for action "config". */
  line?: number;
}

export interface Problem {
  text: string;
  fix?: ProblemFix;
  /** Blocking problems keep the run from starting; others are warnings. */
  blocking: boolean;
  /** The header label when this is the only blocker: a prompt, not a failure. */
  prompt?: string;
  /** Which part of the draft the problem is in, for ordering and tests. */
  kind: 'config' | 'input' | 'runtime' | 'scenario';
}

export interface Readiness {
  tone: 'ok' | 'warn' | 'blocked' | 'running';
  label: string;
  problems: Problem[];
}

export interface ReadinessInput {
  configText: string;
  /** The first YAML syntax error found while typing. */
  yamlError?: { message: string; line: number };
  /** The latest validation, if it has answered. */
  inspection?: { valid: boolean; diagnostics?: Pick<Diagnostic, 'severity' | 'message' | 'component' | 'code'>[] };
  validating?: boolean;
  /** The validation request itself failed. */
  validationError?: string;
  /** Why the document cannot run (input/quickInput.ts runBlocker), if it cannot. */
  inputBlocker?: string;
  /** The selected runtime, when known. */
  runtime?: { name: string; missing?: boolean; unverified?: boolean };
  /** A run of the draft is queued or running. */
  running?: boolean;
}

const plural = (n: number, noun: string) => `${n} ${noun}${n === 1 ? '' : 's'}`;

/** MAX_CONFIG_PROBLEMS caps how many diagnostics are listed as separate problems. */
const MAX_CONFIG_PROBLEMS = 5;

/**
 * readiness states whether the draft can run and what stops it, worded as the
 * workbench header shows it: "Ready to run", "Add an input to run",
 * "3 problems block running". Every problem names where the remedy is.
 */
export function readiness(i: ReadinessInput): Readiness {
  if (i.running) return { tone: 'running', label: 'Running', problems: [] };
  const problems: Problem[] = [];

  if (!i.configText.trim()) {
    problems.push({ kind: 'config', blocking: true, prompt: 'Add a configuration to run', text: 'There is no configuration', fix: { action: 'config', label: 'Write or open a configuration' } });
  } else if (i.yamlError) {
    problems.push({
      kind: 'config',
      blocking: true,
      text: `YAML syntax error on line ${i.yamlError.line}: ${i.yamlError.message}`,
      fix: { action: 'config', label: `Go to line ${i.yamlError.line}`, line: i.yamlError.line },
    });
  } else if (i.validationError) {
    problems.push({ kind: 'config', blocking: true, text: `The configuration could not be checked: ${i.validationError}` });
  } else if (i.inspection && !i.inspection.valid) {
    const errors = (i.inspection.diagnostics ?? []).filter((d) => d.severity !== 'warning');
    if (!errors.length) problems.push({ kind: 'config', blocking: true, text: 'The configuration is not valid', fix: { action: 'config', label: 'Open the configuration' } });
    for (const d of errors.slice(0, MAX_CONFIG_PROBLEMS)) {
      problems.push({
        kind: 'config',
        blocking: true,
        text: d.component ? `${d.component}: ${d.message}` : d.message,
        fix: { action: 'config', label: 'Show in the configuration' },
      });
    }
    if (errors.length > MAX_CONFIG_PROBLEMS) {
      problems.push({ kind: 'config', blocking: true, text: `${plural(errors.length - MAX_CONFIG_PROBLEMS, 'more configuration problem')}`, fix: { action: 'config', label: 'Open the configuration' } });
    }
  }

  if (i.inputBlocker && i.configText.trim() && i.inputBlocker !== 'The configuration is not valid') {
    const noInput = i.inputBlocker === 'Add an input to run';
    problems.push({
      kind: noInput ? 'input' : 'scenario',
      blocking: true,
      prompt: noInput ? 'Add an input to run' : undefined,
      text: i.inputBlocker,
      fix: noInput ? { action: 'inputs', label: 'Add an input' } : { action: 'inputs', label: 'Open the inputs' },
    });
  }

  if (i.runtime?.missing) {
    problems.push({
      kind: 'runtime',
      blocking: true,
      text: `Runtime ${i.runtime.name} is not built here`,
      fix: { action: 'runtimes', label: 'Choose or add a runtime' },
    });
  } else if (i.runtime?.unverified) {
    problems.push({
      kind: 'runtime',
      blocking: false,
      text: `Runtime ${i.runtime.name} is unverified; results may differ from a verified Collector`,
      fix: { action: 'runtimes', label: 'Open runtimes' },
    });
  }

  const blocking = problems.filter((p) => p.blocking);
  if (blocking.length === 0) {
    if (i.configText.trim() && !i.inspection && !i.validationError) return { tone: 'warn', label: 'Checking the configuration', problems };
    const warnings = problems.length;
    return warnings ? { tone: 'warn', label: `Ready to run, ${plural(warnings, 'warning')}`, problems } : { tone: 'ok', label: 'Ready to run', problems };
  }
  // A missing configuration or input alone is a gentle prompt, not a failure.
  if (blocking.length === 1 && blocking[0].prompt) return { tone: 'warn', label: blocking[0].prompt, problems };
  return {
    tone: 'blocked',
    label: blocking.length === 1 ? '1 problem blocks running' : `${blocking.length} problems block running`,
    problems,
  };
}
