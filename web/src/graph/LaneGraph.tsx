import { useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent, type MouseEvent, type ReactNode } from 'react';
import type { Graph } from '../api/types';
import type { EdgeStats, NodeStats } from '../runs/annotations';
import type { PathHighlight, PathStatus } from '../inspect/journey';
import { Icon, type IconName } from '../ui/Icon';
import {
  SIGNAL_FILTERS,
  clampZoom,
  fitZoom,
  laneModel,
  linkOnPath,
  nextFocus,
  stepZoom,
  zoomLabel,
  ZOOM_MAX,
  ZOOM_MIN,
  type ArrowDirection,
  type ExtensionItem,
  type Lane,
  type LaneItem,
  type LaneLink,
  type ModeInfo,
  type SignalFilter,
} from './lanes';
import './lanes.css';

export interface LaneGraphProps {
  graph: Graph;
  selected?: string;
  onSelect(key: string | undefined): void;
  nodeStats?: Map<string, NodeStats>;
  edgeStats?: Map<string, EdgeStats>;
  /** A followed record journey: its nodes are emphasized, the rest dimmed. */
  path?: PathHighlight;
  /** Provenance of the shown run, e.g. "Recorded · run #18 · read-only". */
  stamp?: ReactNode;
  /** More toolbar content after the stamp, e.g. the chip of a followed journey. */
  toolbar?: ReactNode;
  /** No run yet: the dotted draft canvas. */
  draft?: boolean;
}

const KIND_ICONS: Record<string, IconName> = {
  receiver: 'node-receiver',
  processor: 'node-processor',
  exporter: 'node-exporter',
  connector: 'connector',
  extension: 'extension',
};

const SIGNAL_ICONS: Record<string, IconName | undefined> = { logs: 'signal-logs', traces: 'signal-traces' };

const SIGNAL_LABELS: Record<SignalFilter, string> = { all: 'All', logs: 'Logs', traces: 'Traces', metrics: 'Metrics' };

const PATH_FLAGS: Partial<Record<PathStatus, { label: string; tone: 'fail' | 'accent' }>> = {
  stopped: { label: 'Stopped here', tone: 'fail' },
  delivered: { label: 'Delivered', tone: 'accent' },
  counted: { label: 'Counted here', tone: 'accent' },
  changed: { label: 'Changed it', tone: 'accent' },
};

function ModeBadge({ mode }: { mode: ModeInfo }) {
  return (
    <span className={`od-mode od-mode--${mode.cls} od-mode--icon`} title={mode.label}>
      <Icon name={mode.icon} size={12} />
      <span className="od-sr">{mode.label}</span>
    </span>
  );
}

function describe(it: LaneItem): string {
  return `${it.kind} ${it.id}, ${it.mode.label}${it.meta ? `, ${it.meta}` : ''}`;
}

function NodeButton({
  item,
  lane,
  selected,
  path,
  onSelect,
}: {
  item: LaneItem;
  lane: string;
  selected: boolean;
  path?: PathHighlight;
  onSelect(key: string): void;
}) {
  const status = path?.nodes.get(item.key);
  const flag = status ? PATH_FLAGS[status] : undefined;
  const cls = [
    'od-node',
    'od-node--compact',
    selected && 'od-node--selected',
    item.mode.cls === 'simulated' && 'od-node--simulated',
    item.mode.cls === 'standin' && 'od-node--standin',
    item.mode.cls === 'unsupported' && 'od-node--unsupported',
    item.errors > 0 && 'od-node--error',
    status && 'od-node--path',
    status === 'stopped' && 'od-node--stopped',
    ((path && !status) || !item.match) && 'od-node--dim',
  ];
  return (
    <button
      type="button"
      className={cls.filter(Boolean).join(' ')}
      aria-label={describe(item)}
      aria-pressed={selected}
      title={item.note ? `${item.mode.label}: ${item.note}` : item.mode.label}
      data-lg-node={item.key}
      data-lg-lane={lane}
      onClick={() => onSelect(item.key)}
    >
      {flag && <span className={`od-node__flag od-node__flag--${flag.tone}`}>{flag.label}</span>}
      <div className="od-node__top">
        <span className="od-node__kind">
          <Icon name={KIND_ICONS[item.kind] ?? 'node-processor'} size={12} />
          {item.kind}
        </span>
        <ModeBadge mode={item.mode} />
      </div>
      <div className="od-node__id">{item.id}</div>
      {item.meta && <div className="od-node__meta">{item.meta}</div>}
    </button>
  );
}

/** Handoff is a connector consumed by a lane; it selects the connector, which is drawn in the lane that feeds it. */
function Handoff({ item, selected, onSelect }: { item: LaneItem; selected: boolean; onSelect(key: string): void }) {
  return (
    <button
      type="button"
      className={['od-handoff', 'lg-handoff-button', !item.match && 'lg-dim'].filter(Boolean).join(' ')}
      aria-label={`connector ${item.id}, hand-off from another pipeline`}
      aria-pressed={selected}
      data-lg-node={item.key}
      onClick={() => onSelect(item.key)}
    >
      <Icon name="connector" size={12} />
      {item.id}
    </button>
  );
}

function Link({ link, path }: { link: LaneLink; path?: PathHighlight }) {
  const onPath = linkOnPath(link, path);
  const cls = ['od-link', (link.active || onPath) && 'od-link--active', link.dropped && 'od-link--dropped', link.short && 'od-link--short'];
  return (
    <span className={cls.filter(Boolean).join(' ')} aria-hidden="true">
      {link.count !== undefined && <span className="od-count">{link.count}</span>}
    </span>
  );
}

function LaneView({
  lane,
  selected,
  path,
  onSelect,
}: {
  lane: Lane;
  selected?: string;
  path?: PathHighlight;
  onSelect(key: string): void;
}) {
  const glyph = SIGNAL_ICONS[lane.signal];
  return (
    <div className="od-lane" role="group" aria-label={`${lane.signal} pipeline ${lane.id}`}>
      <div className="od-lane__head">
        {glyph && <Icon name={glyph} size={12} />}
        {lane.id}
      </div>
      <div className="od-lane__row">
        {lane.columns.map((col, i) => (
          <ColumnView
            key={i}
            col={col}
            link={lane.links[i]}
            lane={lane.id}
            selected={selected}
            path={path}
            onSelect={onSelect}
          />
        ))}
      </div>
    </div>
  );
}

function ColumnView({
  col,
  link,
  lane,
  selected,
  path,
  onSelect,
}: {
  col: LaneItem[];
  link?: LaneLink;
  lane: string;
  selected?: string;
  path?: PathHighlight;
  onSelect(key: string): void;
}) {
  const items = col.map((it) =>
    it.handoff ? (
      <Handoff key={it.key} item={it} selected={it.key === selected} onSelect={onSelect} />
    ) : (
      <NodeButton key={it.key} item={it} lane={lane} selected={it.key === selected} path={path} onSelect={onSelect} />
    ),
  );
  return (
    <>
      {items.length === 1 ? items[0] : <div className="lg-stack">{items}</div>}
      {link && <Link link={link} path={path} />}
    </>
  );
}

function ExtensionButton({ item, selected, onSelect }: { item: ExtensionItem; selected: boolean; onSelect(key: string): void }) {
  return (
    <>
      <button
        type="button"
        className={['od-handoff', 'lg-handoff-button', !item.match && 'lg-dim'].filter(Boolean).join(' ')}
        aria-label={`extension ${item.id}, ${item.mode.label}`}
        aria-pressed={selected}
        title={item.note ? `${item.mode.label}: ${item.note}` : item.mode.label}
        data-lg-node={item.key}
        onClick={() => onSelect(item.key)}
      >
        <Icon name="extension" size={12} />
        {item.id}
        <ModeBadge mode={item.mode} />
      </button>
      {item.summary && <span className="od-subtle lg-ext-note">{item.summary}</span>}
    </>
  );
}

function Legend({ modes, onClose }: { modes: ModeInfo[]; onClose(): void }) {
  return (
    <div className="od-menu lg-legend" role="dialog" aria-label="Execution modes" onKeyDown={(e) => e.key === 'Escape' && onClose()}>
      <div className="od-menu__label">Execution modes</div>
      {modes.map((m) => (
        <div key={m.id} className="lg-legend__item">
          <ModeBadge mode={m} />
          <div>
            <strong>{m.label}</strong>
            <p>{m.meaning}</p>
          </div>
        </div>
      ))}
    </div>
  );
}

/**
 * LaneGraph draws the topology as one lane per pipeline: receivers,
 * processors, then exporters and connectors, with the extensions in a row on
 * top. It takes PipelineGraph's props and the same node keys.
 */
export function LaneGraph({ graph, selected, onSelect, nodeStats, edgeStats, path, stamp, toolbar, draft }: LaneGraphProps) {
  const [query, setQuery] = useState('');
  const [signal, setSignal] = useState<SignalFilter>('all');
  const [zoom, setZoom] = useState(1);
  const [legend, setLegend] = useState(false);
  const [natural, setNatural] = useState({ width: 0, height: 0 });
  const viewport = useRef<HTMLDivElement>(null);
  const content = useRef<HTMLDivElement>(null);
  const legendId = useId();

  const model = useMemo(() => laneModel(graph, { signal, query, nodeStats, edgeStats }), [graph, signal, query, nodeStats, edgeStats]);

  // The scaled content takes no layout space, so the scroll area is sized from its natural size.
  useLayoutEffect(() => {
    const el = content.current;
    if (!el) return;
    const measure = () => setNatural((p) => (p.width === el.offsetWidth && p.height === el.offsetHeight ? p : { width: el.offsetWidth, height: el.offsetHeight }));
    measure();
    if (typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, [model]);

  useEffect(() => {
    if (!legend) return;
    const close = (e: PointerEvent) => {
      if (!(e.target as Element).closest('.lg-legend, [data-lg-legend-toggle]')) setLegend(false);
    };
    document.addEventListener('pointerdown', close);
    return () => document.removeEventListener('pointerdown', close);
  }, [legend]);

  const fit = () => {
    const v = viewport.current;
    if (!v) return;
    setZoom(fitZoom(natural, { width: v.clientWidth, height: v.clientHeight }));
    v.scrollTo?.(0, 0);
  };

  const deselect = (e: MouseEvent) => {
    if (!(e.target as Element).closest('button')) onSelect(undefined);
  };

  const arrows: Record<string, ArrowDirection> = { ArrowLeft: 'left', ArrowRight: 'right', ArrowUp: 'up', ArrowDown: 'down' };
  const onKeyDown = (e: KeyboardEvent) => {
    const dir = arrows[e.key];
    const from = (e.target as Element).closest<HTMLElement>('[data-lg-node]');
    if (!dir || !from || !content.current) return;
    const all = [...content.current.querySelectorAll<HTMLElement>('[data-lg-node]')];
    const at = all.indexOf(from);
    const to = nextFocus(all.map((n) => n.getBoundingClientRect()), at, dir);
    if (to === undefined) return;
    e.preventDefault();
    all[to]!.focus();
  };

  const empty = model.lanes.length === 0 && model.extensions.length === 0;
  return (
    <section
      className={['od-graph', 'lg', draft && 'od-graph--draft'].filter(Boolean).join(' ')}
      aria-label="Pipeline graph"
    >
      <div className="lg-toolbar">
        {stamp && (
          <span className="od-graph__stamp">
            <Icon name="run-marker" size={12} />
            {stamp}
          </span>
        )}
        {toolbar}
        <label className="od-search">
          <Icon name="search" size={14} />
          <input
            aria-label="Search graph"
            placeholder="Component or pipeline"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </label>
        <div className="od-seg" role="group" aria-label="Signal">
          {SIGNAL_FILTERS.map((s) => (
            <button key={s} type="button" aria-pressed={signal === s} onClick={() => setSignal(s)}>
              {SIGNAL_LABELS[s]}
            </button>
          ))}
        </div>
        {model.searching && (
          <span className="lg-status" role="status">
            {model.matches === 0 ? 'No component matches' : `${model.matches} ${model.matches === 1 ? 'match' : 'matches'}`}
          </span>
        )}
        <span className="lg-spacer" />
        <button
          type="button"
          className="od-btn od-btn--sm"
          data-lg-legend-toggle
          aria-expanded={legend}
          aria-controls={legendId}
          onClick={() => setLegend(!legend)}
        >
          <Icon name="circle-help" size={14} />
          <span>Execution modes</span>
        </button>
      </div>
      {legend && (
        <div id={legendId}>
          <Legend modes={model.modes} onClose={() => setLegend(false)} />
        </div>
      )}
      <div className="lg-scroll" ref={viewport} onClick={deselect}>
        <div className="lg-sizer" style={{ width: natural.width * zoom, height: natural.height * zoom }}>
          <div
            className="lg-content"
            ref={content}
            style={{ transform: zoom === 1 ? undefined : `scale(${zoom})` }}
            onKeyDown={onKeyDown}
          >
            {model.extensions.length > 0 && (
              <div className="lg-ext">
                <span className="od-section-label">Extensions</span>
                {model.extensions.map((x) => (
                  <ExtensionButton key={x.key} item={x} selected={x.key === selected} onSelect={onSelect} />
                ))}
              </div>
            )}
            {model.lanes.map((l) => (
              <LaneView key={l.id} lane={l} selected={selected} path={path} onSelect={onSelect} />
            ))}
            {empty && (
              <div className="od-empty">
                <div className="od-empty__title">{signal === 'all' ? 'No pipelines' : `No ${signal} pipelines`}</div>
                <div>{signal === 'all' ? 'This configuration defines no pipelines.' : 'Choose All to see every pipeline.'}</div>
              </div>
            )}
            {!empty && model.lanes.length === 0 && (
              <div className="od-subtle">No {signal} pipelines. Choose All to see every pipeline.</div>
            )}
          </div>
        </div>
      </div>
      <div className="lg-tools-wrap">
        <div className="od-graph__tools" role="group" aria-label="Zoom">
          <button
            type="button"
            className="od-btn od-btn--ghost od-btn--sm od-btn--icon"
            aria-label="Zoom out"
            disabled={zoom <= ZOOM_MIN}
            onClick={() => setZoom(stepZoom(zoom, -1))}
          >
            <Icon name="zoom-out" size={14} />
          </button>
          <span className="lg-zoom" aria-live="polite">{zoomLabel(clampZoom(zoom))}</span>
          <button
            type="button"
            className="od-btn od-btn--ghost od-btn--sm od-btn--icon"
            aria-label="Zoom in"
            disabled={zoom >= ZOOM_MAX}
            onClick={() => setZoom(stepZoom(zoom, 1))}
          >
            <Icon name="zoom-in" size={14} />
          </button>
          <button type="button" className="od-btn od-btn--ghost od-btn--sm od-btn--icon" aria-label="Fit to view" onClick={fit}>
            <Icon name="scan" size={14} />
          </button>
        </div>
      </div>
    </section>
  );
}
