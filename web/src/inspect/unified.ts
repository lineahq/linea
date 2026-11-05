import { structuredPatch } from 'diff';

export interface UnifiedLine {
  kind: 'hunk' | 'add' | 'del' | 'ctx';
  text: string;
  /** Line numbers in before and after (1-based) where the line exists. */
  old?: number;
  new?: number;
}

/** unified renders a line diff of two texts as hunks with context. */
export function unified(before: string, after: string, context = 3): UnifiedLine[] {
  const patch = structuredPatch('before', 'after', before, after, '', '', { context });
  const out: UnifiedLine[] = [];
  for (const h of patch.hunks) {
    out.push({ kind: 'hunk', text: `@@ -${h.oldStart},${h.oldLines} +${h.newStart},${h.newLines} @@` });
    let o = h.oldStart;
    let n = h.newStart;
    for (const l of h.lines) {
      const text = l.slice(1);
      if (l.startsWith('+')) out.push({ kind: 'add', text, new: n++ });
      else if (l.startsWith('-')) out.push({ kind: 'del', text, old: o++ });
      else if (l.startsWith(' ')) out.push({ kind: 'ctx', text, old: o++, new: n++ });
    }
  }
  return out;
}
