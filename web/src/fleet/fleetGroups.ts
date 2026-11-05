import type { CollectorGroup } from '../api/types';

/** Fleet's group kinds: static groups list agents, selector groups match attributes. */
export function groupKindText(kind: string): string {
  return kind === 'static' ? 'Static' : kind === 'selector' ? 'By selector' : kind;
}

/** groupMembership is a group's one-line membership summary for lists. */
export function groupMembership(g: Pick<CollectorGroup, 'kind' | 'selector' | 'agent_ids' | 'member_count'>): string {
  if (g.kind === 'selector') return g.selector ? `Agents matching ${g.selector}` : 'Agents matching a selector';
  const n = g.member_count ?? g.agent_ids?.length ?? 0;
  return `${n} agent${n === 1 ? '' : 's'} listed`;
}
