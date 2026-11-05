// @vitest-environment jsdom
import { act, createElement, useEffect } from 'react';
import { createRoot } from 'react-dom/client';
import { describe, expect, it, vi } from 'vitest';
import { SideBySide } from './DiffViews';

const state = vi.hoisted(() => ({ calls: [] as string[], options: {} as Record<string, unknown> }));
vi.mock('../monaco', () => ({ JSON_LANGUAGE: 'javascript' }));
vi.mock('../useTheme', () => ({ useDarkMode: () => false }));
vi.mock('@monaco-editor/react', () => ({
  DiffEditor: ({ onMount, options }: { onMount(instance: unknown): void; options: Record<string, unknown> }) => {
    state.options = options;
    useEffect(() => {
      let model: { original: { dispose(): void }; modified: { dispose(): void } } | null = {
        original: { dispose: () => { if (model) throw new Error('Original still attached'); state.calls.push('original disposed'); } },
        modified: { dispose: () => { if (model) throw new Error('Modified still attached'); state.calls.push('modified disposed'); } },
      };
      const editor = { getModel: () => model, setModel: () => { state.calls.push('detached'); model = null; } };
      onMount(editor);
      // Mirror the installed wrapper: dispose attached models, then editor.
      return () => { editor.getModel()?.original.dispose(); editor.getModel()?.modified.dispose(); state.calls.push('editor disposed'); };
    }, []); // eslint-disable-line react-hooks/exhaustive-deps -- mounts once, like the real editor
    return createElement('div');
  },
}));

describe('raw diff editor lifecycle', () => {
  it('detaches and disposes both models before the wrapper unmounts, without leaking or double-disposing', () => {
    (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
    state.calls = [];
    const host = document.createElement('div'), root = createRoot(host);
    act(() => root.render(createElement(SideBySide, { before: '{}', after: '{"changed":true}' })));
    expect(state.options).toMatchObject({ readOnly: true, originalEditable: false, renderSideBySide: true, useInlineViewWhenSpaceIsLimited: false });
    act(() => root.unmount());
    expect(state.calls).toEqual(['detached', 'original disposed', 'modified disposed', 'editor disposed']);
  });
});
