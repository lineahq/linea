import type { RunInputs } from '../api/types';

/** The recorded inputs a comparison can show side by side, in reading order. */
const SOURCES = [
  ['config', 'Configuration'],
  ['scenario', 'Scenario'],
  ['fixture', 'Fixture'],
  ['setup', 'Setup'],
  ['assertions', 'Assertions'],
] as const;

/** One recorded input that differs between the two runs. */
export interface InputDiff {
  key: (typeof SOURCES)[number][0];
  name: string;
  /** The baseline's text; empty when the baseline had no such input. */
  before: string;
  after: string;
}

/**
 * differingInputs lists the recorded inputs whose content differs between the
 * baseline and the candidate, compared by the platform's SHA-256 so equal
 * texts are never diffed. An input present on one side only differs.
 */
export function differingInputs(before: RunInputs, after: RunInputs): InputDiff[] {
  return SOURCES.flatMap(([key, name]) => {
    const a = before[key];
    const b = after[key];
    if (!a && !b) return [];
    if (a && b && a.sha256 === b.sha256) return [];
    return [{ key, name, before: a?.text ?? '', after: b?.text ?? '' }];
  });
}
