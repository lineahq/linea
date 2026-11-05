import { useMemo, useRef, useState } from 'react';
import { useVirtualizer } from '@tanstack/react-virtual';
import { changedPaths, flattenTree } from './tree';

const ROW = 20;

/**
 * JsonTree renders a payload as a collapsible tree, virtualized because a
 * snapshot can hold thousands of records. With compareTo, values that
 * differ from the other side are highlighted, and open containers holding
 * a difference are marked at the edge.
 */
export function JsonTree({ value, compareTo, side }: { value: unknown; compareTo?: unknown; side?: 'before' | 'after' }) {
  const [collapsed, setCollapsed] = useState<Set<string>>(() => new Set());
  const changed = useMemo(() => (compareTo === undefined ? undefined : changedPaths(value, compareTo)), [value, compareTo]);
  const rows = useMemo(() => flattenTree(value, collapsed, changed), [value, collapsed, changed]);
  const scroller = useRef<HTMLDivElement>(null);
  const v = useVirtualizer({ count: rows.length, getScrollElement: () => scroller.current, estimateSize: () => ROW, overscan: 20 });
  const toggle = (path: string) =>
    setCollapsed((c) => {
      const n = new Set(c);
      if (n.has(path)) n.delete(path);
      else n.add(path);
      return n;
    });
  return (
    <div className="json-tree mono" ref={scroller} role="tree">
      <div style={{ height: v.getTotalSize(), position: 'relative' }}>
        {v.getVirtualItems().map((item) => {
          const r = rows[item.index];
          return (
            <div
              key={r.path}
              role="treeitem"
              aria-expanded={r.size !== undefined ? r.open : undefined}
              className={`tree-row${r.changed ? (r.open ? ` contains-${side ?? 'any'}` : ` changed-${side ?? 'any'}`) : ''}`}
              style={{ transform: `translateY(${item.start}px)`, height: ROW, paddingLeft: 6 + r.depth * 14 }}
              onClick={() => r.size !== undefined && toggle(r.path)}
            >
              {r.size !== undefined ? <span className="twisty">{r.open ? '▾' : '▸'}</span> : <span className="twisty" />}
              <span className="tree-key">{r.key}</span>
              {r.value !== undefined ? (
                <span className="tree-val">: {r.value}</span>
              ) : (
                <span className="muted"> {r.open ? '' : `{${r.size}}`}</span>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}
