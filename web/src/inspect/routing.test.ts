import { describe, expect, it } from 'vitest';
import type { Routing } from '../api/types';
import { explained, trail } from './routing';

const rec = (i: number) => `resource[0].scope[0].log_record[${i}]`;
const routing: Routing = {
  connector: 'routing', input: 4, verification: 'replay_verified',
  rules: [
    { index: 0, condition: 'log.severity_number >= SEVERITY_NUMBER_ERROR', action: 'copy', pipelines: ['logs/pager'] },
    { index: 1, condition: 'resource.attributes["service.name"] == "audit"', action: 'move', pipelines: ['logs/audit'] },
    { index: 2, condition: 'log.body == "x"', action: 'move', pipelines: ['logs/x'] },
  ],
  default_pipelines: ['logs/archive'],
  records: [{ ref: rec(0), rules: [0], default: true }, { ref: rec(1), default: true }, { ref: rec(2), rules: [1] }, { ref: rec(3) }],
};

describe('routing trail', () => {
  it('says which rule routed a record and what it did', () => {
    expect(trail(routing, rec(0))!.sentence).toBe('Matched rule 0 · copied to logs/pager, and the original continued to the default');
    expect(trail(routing, rec(2))!.sentence).toBe('Matched rule 1 · moved to logs/audit');
    expect(trail(routing, rec(1))!.sentence).toBe('No rule matched · sent to the default (logs/archive)');
    expect(trail(routing, rec(3))!.sentence).toBe('No rule matched and there is no default pipeline');
  });
  it('lists the rules in order, and marks the ones a move kept the record from', () => {
    expect(trail(routing, rec(0))!.steps.map((s) => s.result)).toEqual(['matched', 'no-match', 'no-match']);
    expect(trail(routing, rec(2))!.steps.map((s) => s.result)).toEqual(['no-match', 'matched', 'not-reached']);
  });
  it('joins several matching rules', () => {
    const two: Routing = { ...routing, records: [{ ref: rec(0), rules: [0, 1] }] };
    expect(trail(two, rec(0))!.sentence).toBe('Matched rules 0 and 1 · copied to logs/pager and moved to logs/audit');
  });
  it('claims nothing unless the replay was verified', () => {
    const differs: Routing = { ...routing, verification: 'replay_differs', records: undefined };
    expect(explained(differs)).toBe(false);
    expect(trail(differs, rec(0))).toBeUndefined();
    expect(trail(routing, 'resource[9].scope[0].log_record[0]')).toBeUndefined();
  });
});
