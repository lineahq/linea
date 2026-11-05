// Monaco is bundled, never fetched from a CDN: the debugger works offline.
// Only the editor API, its base worker and the Monarch tokenizers the UI
// needs are loaded.
import * as monaco from 'monaco-editor/editor/editor.api';
import 'monaco-editor/languages/definitions/yaml/register';
import 'monaco-editor/languages/definitions/javascript/register';
import EditorWorker from 'monaco-editor/editor/editor.worker?worker';
import { loader } from '@monaco-editor/react';

self.MonacoEnvironment = { getWorker: () => new EditorWorker() };
loader.config({ monaco });

/** JSON_LANGUAGE highlights read-only JSON: the javascript tokenizer needs no worker. */
export const JSON_LANGUAGE = 'javascript';

export { monaco };
