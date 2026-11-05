import { describe, expect, it } from 'vitest';
import { EDITOR_TABS, editorTab } from './editorTabs';

describe('EDITOR_TABS', () => {
  it('always offers every part of a run, mocks included', () => {
    expect(EDITOR_TABS.map((t) => t.id)).toEqual(['config', 'inputs', 'mocks', 'assertions', 'yaml']);
  });
});

describe('editorTab', () => {
  it.each([
    [undefined, 'config'],
    ['inputs', 'inputs'],
    ['input', 'inputs'],
    ['form', 'inputs'],
    ['scenario', 'yaml'],
    ['mocks', 'mocks'],
    ['assertions', 'assertions'],
    ['nonsense', 'config'],
  ] as const)('%s -> %s', (requested, want) => {
    expect(editorTab(requested)).toBe(want);
  });
});
