import { describe, expect, it } from 'vitest';
import { groupKindText, groupMembership } from './fleetGroups';

describe('fleet group text', () => {
  it('names the kinds fleet reports and passes unknown ones through', () => {
    expect(groupKindText('static')).toBe('Static');
    expect(groupKindText('selector')).toBe('By selector');
    expect(groupKindText('future')).toBe('future');
  });
  it('summarises membership', () => {
    expect(groupMembership({ kind: 'selector', selector: 'env=prod' })).toBe('Agents matching env=prod');
    expect(groupMembership({ kind: 'static', agent_ids: ['a'] })).toBe('1 agent listed');
    expect(groupMembership({ kind: 'static', member_count: 3 })).toBe('3 agents listed');
    expect(groupMembership({ kind: 'static' })).toBe('0 agents listed');
  });
});
