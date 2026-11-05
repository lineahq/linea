// A JSON value flattened into the visible rows of a collapsible tree, so a
// large snapshot can be rendered with a virtualized list.

export interface TreeRow {
  /** JSON pointer-like path, e.g. "resourceSpans/0/scopeSpans". "" is the root. */
  path: string;
  key: string;
  depth: number;
  /** Primitive value as text; undefined for objects and arrays. */
  value?: string;
  /** Child count for objects and arrays. */
  size?: number;
  open: boolean;
  /** Set when compared: the value at this path differs from the other side. */
  changed?: boolean;
}

const isContainer = (v: unknown): v is Record<string, unknown> | unknown[] => !!v && typeof v === 'object';

/** flattenTree lists the visible rows; containers are open unless collapsed. */
export function flattenTree(value: unknown, collapsed: Set<string>, changed?: Set<string>): TreeRow[] {
  const rows: TreeRow[] = [];
  const walk = (v: unknown, key: string, path: string, depth: number) => {
    const row: TreeRow = { path, key, depth, open: false, changed: changed?.has(path) };
    rows.push(row);
    if (!isContainer(v)) {
      row.value = typeof v === 'string' ? JSON.stringify(v) : String(v);
      return;
    }
    const entries = Array.isArray(v) ? v.map((x, i) => [String(i), x] as const) : Object.entries(v);
    row.size = entries.length;
    row.open = !collapsed.has(path);
    if (!row.open) return;
    for (const [k, x] of entries) walk(x, k, path ? `${path}/${k}` : k, depth + 1);
  };
  walk(value, '(root)', '', 0);
  return rows;
}

/**
 * changedPaths returns the paths of value whose content differs from other
 * at the same path, including every ancestor of a difference, so collapsed
 * containers still show that something inside them changed.
 */
export function changedPaths(value: unknown, other: unknown): Set<string> {
  const out = new Set<string>();
  const walk = (a: unknown, b: unknown, path: string): boolean => {
    let diff: boolean;
    if (isContainer(a) && isContainer(b) && Array.isArray(a) === Array.isArray(b)) {
      const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
      diff = false;
      for (const k of keys) {
        const child = path ? `${path}/${k}` : k;
        if (!(k in a)) {
          diff = true;
          continue;
        }
        if (walk((a as Record<string, unknown>)[k], (b as Record<string, unknown>)[k], child)) diff = true;
      }
    } else {
      diff = JSON.stringify(a) !== JSON.stringify(b);
      if (diff && isContainer(a)) markAll(a, path, out);
    }
    if (diff) out.add(path);
    return diff;
  };
  walk(value, other, '');
  return out;
}

function markAll(v: unknown, path: string, out: Set<string>) {
  out.add(path);
  if (!isContainer(v)) return;
  for (const [k, x] of Object.entries(v)) markAll(x, path ? `${path}/${k}` : k, out);
}
