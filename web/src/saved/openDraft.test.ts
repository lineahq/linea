import { describe, expect, it } from 'vitest';
import { canOpenAsDraft } from './openDraft';

describe('canOpenAsDraft', () => {
  it.each([
    ['configuration', true],
    ['scenario', true],
    ['fixture', false],
    ['setup', false],
    ['manifest', false],
  ])('%s', (kind, want) => expect(canOpenAsDraft(kind)).toBe(want));
});
