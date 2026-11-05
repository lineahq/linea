import type { DraftKind } from '../workspace/drafts';

/** Artifact kinds that load into a draft (configurations and scenarios). */
export function canOpenAsDraft(kind: string): kind is DraftKind {
  return kind === 'configuration' || kind === 'scenario';
}
