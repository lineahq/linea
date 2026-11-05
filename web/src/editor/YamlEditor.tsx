import { useEffect, useRef } from 'react';
import Editor, { type OnMount } from '@monaco-editor/react';
import { monaco } from '../monaco';
import { useDarkMode } from '../useTheme';
import type { Marker } from './markers';

type StandaloneEditor = Parameters<OnMount>[0];

export interface EditorHandle {
  reveal(line: number, column?: number): void;
}

interface Props {
  value: string;
  onChange(value: string): void;
  markers?: Marker[];
  /** Model path, so each editor keeps its own undo history. */
  path: string;
  language?: string;
  readOnly?: boolean;
  handle?: (h: EditorHandle) => void;
  /** Called when the editor loses focus, to commit buffered edits. */
  onBlur?: () => void;
}

const severities = {
  error: monaco.MarkerSeverity.Error,
  warning: monaco.MarkerSeverity.Warning,
  info: monaco.MarkerSeverity.Info,
};

export function YamlEditor({ value, onChange, markers = [], path, language = 'yaml', readOnly, handle, onBlur }: Props) {
  const dark = useDarkMode();
  const editorRef = useRef<StandaloneEditor | null>(null);
  const blur = useRef(onBlur);
  blur.current = onBlur;

  useEffect(() => {
    const model = editorRef.current?.getModel();
    if (!model) return;
    monaco.editor.setModelMarkers(
      model,
      'linea',
      markers.map((m) => ({
        severity: severities[m.severity],
        message: m.message,
        code: m.code,
        startLineNumber: m.location.startLine,
        startColumn: m.location.startColumn,
        endLineNumber: m.location.endLine,
        endColumn: m.location.endColumn,
      })),
    );
  }, [markers]);

  return (
    <Editor
      path={path}
      language={language}
      value={value}
      theme={dark ? 'vs-dark' : 'vs'}
      onChange={(v) => onChange(v ?? '')}
      onMount={(editor) => {
        editorRef.current = editor;
        editor.onDidBlurEditorText(() => blur.current?.());
        handle?.({
          reveal(line, column = 1) {
            editor.revealLineInCenter(line);
            editor.setPosition({ lineNumber: line, column });
            editor.focus();
          },
        });
      }}
      options={{
        readOnly,
        minimap: { enabled: false },
        fontSize: 13,
        tabSize: 2,
        scrollBeyondLastLine: false,
        automaticLayout: true,
        renderWhitespace: 'boundary',
      }}
    />
  );
}
