import { parseDocument, isMap, isScalar, isPair, type Node as YNode } from 'yaml';

/** A 1-based line and column range in the configuration text. */
export interface Location {
  startLine: number;
  startColumn: number;
  endLine: number;
  endColumn: number;
}

const SECTIONS = ['receivers', 'processors', 'exporters', 'connectors', 'extensions'];

function lineCol(text: string, offset: number): { line: number; column: number } {
  let line = 1;
  let last = -1;
  for (let i = 0; i < offset && i < text.length; i++) {
    if (text[i] === '\n') {
      line++;
      last = i;
    }
  }
  return { line, column: offset - last };
}

function rangeOf(text: string, node: YNode | null | undefined): Location | undefined {
  const r = node?.range;
  if (!r) return undefined;
  const s = lineCol(text, r[0]);
  const e = lineCol(text, r[1]);
  return { startLine: s.line, startColumn: s.column, endLine: e.line, endColumn: e.column };
}

/**
 * locateComponent finds the key of a component ID ("transform/logs") in
 * the configuration's component sections, or under service.pipelines for a
 * pipeline ID. Diagnostics name components, not lines, so this is how the
 * editor places markers.
 */
export function locateComponent(text: string, id: string): Location | undefined {
  let doc;
  try {
    doc = parseDocument(text);
  } catch {
    return undefined;
  }
  const root = doc.contents;
  if (!isMap(root)) return undefined;
  const sections = [...SECTIONS.map((s) => [s]), ['service', 'pipelines']];
  for (const path of sections) {
    let node: unknown = root;
    for (const key of path) {
      node = isMap(node) ? node.get(key, true) : undefined;
    }
    if (!isMap(node)) continue;
    for (const item of node.items) {
      if (isPair(item) && isScalar(item.key) && String(item.key.value) === id) {
        return rangeOf(text, item.key);
      }
    }
  }
  return undefined;
}

/** yamlSyntaxErrors returns YAML parse errors with their locations. */
export function yamlSyntaxErrors(text: string): { message: string; location: Location }[] {
  const doc = parseDocument(text);
  return doc.errors.map((e) => {
    const s = lineCol(text, e.pos[0]);
    const end = lineCol(text, Math.max(e.pos[1], e.pos[0] + 1));
    return {
      message: e.message.split('\n')[0],
      location: { startLine: s.line, startColumn: s.column, endLine: end.line, endColumn: end.column },
    };
  });
}
