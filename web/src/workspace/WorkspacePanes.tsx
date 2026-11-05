import { useEffect, useRef, useState, type CSSProperties, type ReactNode, type RefObject } from 'react';
import { clampSplit, DEFAULT_PANES, PANE_STORAGE_KEY, readPanes, splitBounds } from './panes';
import './panes.css';

type Axis = 'left' | 'upper';

/** The smallest sizes (px) either side of a divider keeps: [before, after]. */
const MINIMA: Record<Axis, [number, number]> = { left: [260, 420], upper: [160, 140] };

interface SplitterProps {
  axis: Axis;
  /** The element the divider splits, whose box measures a drag. */
  area: RefObject<HTMLElement | null>;
  extent: number;
  value: number;
  update(ratio: number): void;
  onDrag(dragging: boolean): void;
  label: string;
  /** The ratio a double-click restores. */
  reset: number;
}

function Splitter({ axis, area, extent, value, update, onDrag, label, reset }: SplitterProps) {
  const vertical = axis === 'left';
  const { min, max } = splitBounds(extent, ...MINIMA[axis]);
  const set = (n: number) => update(Math.max(min, Math.min(max, n)));
  return (
    <div
      className={`pane-splitter splitter-${axis}`}
      role="separator"
      tabIndex={0}
      aria-label={label}
      aria-orientation={vertical ? 'vertical' : 'horizontal'}
      aria-valuemin={Math.round(min * 100)}
      aria-valuemax={Math.round(max * 100)}
      aria-valuenow={Math.round(value * 100)}
      title="Drag to resize. Arrow keys adjust; double-click resets."
      onDoubleClick={() => set(reset)}
      onKeyDown={(e) => {
        const step = e.shiftKey ? 0.1 : 0.02;
        const delta = vertical ? { ArrowLeft: -step, ArrowRight: step } : { ArrowUp: -step, ArrowDown: step };
        if (e.key === 'Home') set(min);
        else if (e.key === 'End') set(max);
        else if (e.key in delta) set(value + delta[e.key as keyof typeof delta]!);
        else return;
        e.preventDefault();
      }}
      onPointerDown={(e) => {
        if (e.button !== 0) return;
        e.preventDefault();
        e.currentTarget.focus();
        e.currentTarget.setPointerCapture(e.pointerId);
        onDrag(true);
      }}
      onPointerMove={(e) => {
        if (!e.currentTarget.hasPointerCapture(e.pointerId) || !area.current) return;
        const rect = area.current.getBoundingClientRect();
        set(((vertical ? e.clientX - rect.left : e.clientY - rect.top) - 4) / Math.max(1, extent - 8));
      }}
      onPointerUp={(e) => {
        if (e.currentTarget.hasPointerCapture(e.pointerId)) e.currentTarget.releasePointerCapture(e.pointerId);
        onDrag(false);
      }}
      onLostPointerCapture={() => onDrag(false)}
      onPointerCancel={() => onDrag(false)}
    >
      <span />
    </div>
  );
}

/**
 * WorkspacePanes lays out the debugger: a left pane, and beside it a graph
 * with an optional pane under it. Both dividers resize with the pointer and
 * the keyboard, double-click resets one, and only the split ratios persist
 * (local layout preferences: they never touch telemetry or a run). Another
 * page laid out the same way keeps its ratios under its own `storageKey`,
 * starting from its own `defaults`.
 */
export function WorkspacePanes({ left, graph, bottom, storageKey = PANE_STORAGE_KEY, defaults = DEFAULT_PANES, upperLabel = 'Resize graph and component panes' }: {
  left: ReactNode;
  graph: ReactNode;
  bottom?: ReactNode;
  storageKey?: string;
  defaults?: typeof DEFAULT_PANES;
  upperLabel?: string;
}) {
  const host = useRef<HTMLDivElement>(null);
  const column = useRef<HTMLDivElement>(null);
  const [size, setSize] = useState({ width: 1200, height: 800 });
  const [ratios, setRatios] = useState(() => {
    try {
      return readPanes(localStorage.getItem(storageKey), defaults);
    } catch {
      return { ...defaults };
    }
  });
  const [dragging, setDragging] = useState<Axis>();
  useEffect(() => {
    const element = host.current!;
    const update = () => setSize({ width: element.clientWidth, height: element.clientHeight });
    update();
    const observer = new ResizeObserver(update);
    observer.observe(element);
    return () => observer.disconnect();
  }, []);
  useEffect(() => {
    try {
      localStorage.setItem(storageKey, JSON.stringify(ratios));
    } catch {
      /* Storage may be disabled. */
    }
  }, [ratios, storageKey]);
  const leftRatio = clampSplit(ratios.left, size.width, ...MINIMA.left);
  const upperRatio = clampSplit(ratios.upper, size.height, ...MINIMA.upper);
  const drag = (axis: Axis) => (on: boolean) => setDragging((d) => (on ? axis : d === axis ? undefined : d));
  const update = (axis: Axis) => (ratio: number) => setRatios((r) => ({ ...r, [axis]: ratio }));
  return (
    <div
      ref={host}
      className={`od-wbpanes${dragging ? ` resizing-${dragging}` : ''}`}
      style={
        {
          '--left-pane': `${leftRatio * Math.max(0, size.width - 8)}px`,
          '--upper-pane': `${upperRatio * Math.max(0, size.height - 8)}px`,
        } as CSSProperties
      }
    >
      <div className="od-wbpanes__left">{left}</div>
      <Splitter axis="left" area={host} extent={size.width} value={leftRatio} update={update('left')} onDrag={drag('left')} label="Resize left pane" reset={defaults.left} />
      <div ref={column} className="od-wbpanes__column">
        <div className={`od-wbpanes__graph${bottom ? ' od-wbpanes__graph--sized' : ''}`}>{graph}</div>
        {bottom && (
          <>
            <Splitter axis="upper" area={column} extent={size.height} value={upperRatio} update={update('upper')} onDrag={drag('upper')} label={upperLabel} reset={defaults.upper} />
            <div className="od-wbpanes__bottom">{bottom}</div>
          </>
        )}
      </div>
    </div>
  );
}
