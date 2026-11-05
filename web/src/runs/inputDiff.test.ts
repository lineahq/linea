import { describe, expect, it } from 'vitest';
import type { RunInputs } from '../api/types';
import { differingInputs } from './inputDiff';

const src = (text: string) => ({ text, sha256: `sha-${text}` });
const inputs = (over: Partial<RunInputs> = {}): RunInputs => ({ run_id: 'run-1', mode: 'scenario', config: src('a: 1'), ...over });

describe('differingInputs', () => {
  it.each([
    ['identical inputs', inputs({ scenario: src('s') }), inputs({ scenario: src('s') }), []],
    ['a changed configuration', inputs(), inputs({ config: src('a: 2') }), [['config', 'a: 1', 'a: 2']]],
    ['an input on one side only', inputs(), inputs({ fixture: src('f') }), [['fixture', '', 'f']]],
    [
      'several inputs in reading order',
      inputs({ assertions: src('x'), scenario: src('s1') }),
      inputs({ config: src('b'), scenario: src('s2') }),
      [['config', 'a: 1', 'b'], ['scenario', 's1', 's2'], ['assertions', 'x', '']],
    ],
  ])('lists %s', (_, before, after, want) => {
    expect(differingInputs(before, after).map((d) => [d.key, d.before, d.after])).toEqual(want);
  });

  it('compares by hash, not by text', () => {
    const before = inputs({ config: { text: 'a: 1', sha256: 'same' } });
    const after = inputs({ config: { text: 'a: 1 ', sha256: 'same' } });
    expect(differingInputs(before, after)).toEqual([]);
  });
});
