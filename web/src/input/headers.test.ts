import { describe, expect, it } from 'vitest';
import { headersChip, overriddenDefaults } from './headers';

describe('input headers', () => {
  const defaults: [string, string][] = [['user-agent', 'client/1.0']];
  it('knows which defaults an input replaces, ignoring case', () => {
    expect([...overriddenDefaults([['User-Agent', 'other']], defaults)]).toEqual(['user-agent']);
    expect(overriddenDefaults([['x-team', 'a']], defaults).size).toBe(0);
  });
  it('names the headers an input is sent with', () => {
    expect(headersChip([['x-team', 'a']], defaults)).toBe('x-team, user-agent');
    expect(headersChip([['User-Agent', 'b']], defaults)).toBe('User-Agent');
    expect(headersChip([['a', '1'], ['b', '2']], defaults)).toBe('3 headers');
    expect(headersChip([], [])).toBeUndefined();
  });
});
