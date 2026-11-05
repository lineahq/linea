/**
 * The input pane edits the configuration and one document: its inputs,
 * mocks and assertions, and the document's YAML. Every tab is always
 * shown, so what is possible is always in view; a configuration and one
 * input are all a run needs.
 */
export type EditorTab = 'config' | 'inputs' | 'mocks' | 'assertions' | 'yaml';

export interface EditorTabInfo {
  id: EditorTab;
  label: string;
  purpose: string;
}

export const EDITOR_TABS: EditorTabInfo[] = [
  { id: 'config', label: 'Configuration', purpose: 'The Collector configuration under test. It is never modified on disk.' },
  { id: 'inputs', label: 'Inputs', purpose: 'The OTLP JSON payloads sent to receivers, in order.' },
  {
    id: 'mocks',
    label: 'Mocks',
    purpose: 'Stand-ins for what components read from their environment: files, Kubernetes objects and environment variables.',
  },
  { id: 'assertions', label: 'Assertions', purpose: 'What must hold at exporters and processors for the run to pass.' },
  { id: 'yaml', label: 'YAML', purpose: 'The scenario file the other tabs edit: save it to rerun the run, or check it in CI.' },
];

/**
 * editorTab resolves a requested tab (from the URL) to a tab. Older
 * bookmarks still open their counterpart: input and form open Inputs,
 * scenario opens the YAML.
 */
export function editorTab(requested: string | undefined): EditorTab {
  const id = requested === 'input' || requested === 'form' ? 'inputs' : requested === 'scenario' ? 'yaml' : requested;
  return EDITOR_TABS.find((t) => t.id === id)?.id ?? 'config';
}
