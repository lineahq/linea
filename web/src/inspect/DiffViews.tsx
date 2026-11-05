import { useLayoutEffect, useMemo, useRef } from 'react';
import { DiffEditor, type MonacoDiffEditor } from '@monaco-editor/react';
import { JSON_LANGUAGE } from '../monaco';
import { useDarkMode } from '../useTheme';
import { unified } from './unified';
import { JsonTree } from './JsonTree';

/** SideBySide shows before and after in Monaco's read-only diff editor. */
export function SideBySide({ before, after }: { before: string; after: string }) {
  const dark = useDarkMode();
  const editor = useRef<MonacoDiffEditor | null>(null);
  useLayoutEffect(() => () => {
    // The installed wrapper disposes models before detaching its diff editor.
    // Detach first to avoid Monaco's "TextModel got disposed" error on close.
    const models = editor.current?.getModel();
    editor.current?.setModel(null);
    models?.original.dispose();
    models?.modified.dispose();
    editor.current = null;
  }, []);
  return (
    <div className="diff-host">
      <DiffEditor
        original={before}
        modified={after}
        language={JSON_LANGUAGE}
        theme={dark ? 'vs-dark' : 'vs'}
        onMount={(instance) => { editor.current = instance; }}
        options={{
          readOnly: true,
          originalEditable: false,
          renderSideBySide: true,
          useInlineViewWhenSpaceIsLimited: false,
          diffWordWrap: 'on',
          minimap: { enabled: false },
          scrollBeyondLastLine: false,
          automaticLayout: true,
          hideUnchangedRegions: { enabled: true },
          fontSize: 12,
        }}
      />
    </div>
  );
}

/** UnifiedDiff shows a line diff with context, like `diff -u`. */
export function UnifiedDiff({ before, after }: { before: string; after: string }) {
  const lines = useMemo(() => unified(before, after), [before, after]);
  if (!lines.length) return <p className="muted">Before and after are identical.</p>;
  return (
    <div className="unified mono">
      {lines.map((l, i) => (
        <div key={i} className={`u-${l.kind}`}>
          <span className="ln">{l.old ?? ''}</span>
          <span className="ln">{l.new ?? ''}</span>
          <span className="sign">{{ add: '+', del: '-', ctx: ' ', hunk: '' }[l.kind]}</span>
          {l.text}
        </div>
      ))}
    </div>
  );
}

/** TreePair shows before and after trees with differing paths highlighted. */
export function TreePair({ before, after }: { before: unknown; after: unknown }) {
  return (
    <div className="pair">
      <div>
        <h5>Before</h5>
        {before === undefined ? <p className="muted">Nothing.</p> : <JsonTree value={before} compareTo={after ?? null} side="before" />}
      </div>
      <div>
        <h5>After</h5>
        {after === undefined ? <p className="muted">Nothing.</p> : <JsonTree value={after} compareTo={before ?? null} side="after" />}
      </div>
    </div>
  );
}

/** RawJSON shows the payloads as text. */
export function RawJSON({ before, after }: { before: string; after: string }) {
  return (
    <div className="pair">
      <div>
        <h5>Before</h5>
        <pre className="code raw">{before || '—'}</pre>
      </div>
      <div>
        <h5>After</h5>
        <pre className="code raw">{after || '—'}</pre>
      </div>
    </div>
  );
}
