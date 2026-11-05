import type { CapturePointPage, CaptureSelection, Collector, FilterLimits } from '../api/types';
import { formatCount } from './format';

/**
 * The New capture form as a model. Everything the page edits is a plain value
 * here, and `buildSelection` is the only place it becomes the platform's
 * CaptureSelection (the same request the CLI and API use).
 */

export type CatalogPoint = NonNullable<CapturePointPage['items']>[number];

/** Fleet's ceilings: larger limits are clamped by the server, so the form refuses them. */
export const MAX_DURATION_SECONDS = 280;
export const MIN_DURATION_SECONDS = 5;
export const MAX_TOTAL_RECORDS = 10000;
export const MAX_TOTAL_BYTES = 16 * 1024 * 1024;
export const MAX_COLLECTORS = 16;
export const MAX_POINTS = 64;

/** What a Collector that reports no filter limits compiles (livecapture protocol version 1). */
export const V1_FILTER_LIMITS: FilterLimits = { max_conditions: 16, max_condition_chars: 256, max_regex_chars: 256 };

/**
 * filterLimitsFor is the largest filter worth sending to these Collectors.
 * Chosen Collectors must each compile it, so the smallest of their limits; a
 * group capture keeps the members that compile it, so the largest. The
 * server checks again and names the limit.
 */
export function filterLimitsFor(collectors: Pick<Collector, 'filter_limits'>[], group: boolean): FilterLimits {
  const known = collectors.map((c) => c.filter_limits ?? V1_FILTER_LIMITS);
  if (!known.length) return V1_FILTER_LIMITS;
  const pick = group ? Math.max : Math.min;
  return {
    max_conditions: pick(...known.map((l) => l.max_conditions)),
    max_condition_chars: pick(...known.map((l) => l.max_condition_chars)),
    max_regex_chars: pick(...known.map((l) => l.max_regex_chars)),
  };
}

export interface CaptureForm {
  /** Selected capture points, as pointKey values. */
  points: string[];
  /** One OTTL condition per line. */
  resource: string;
  logs: string;
  traces: string;
  metrics: string;
  datapoint: string;
  /** Fraction of records kept, 0 to 1. */
  samplingRate: number;
  duration: string;
  maxRecords: string;
  maxSize: string;
}

export const defaultForm: CaptureForm = {
  points: [],
  resource: '',
  logs: '',
  traces: '',
  metrics: '',
  datapoint: '',
  samplingRate: 1,
  duration: '1m',
  maxRecords: '10000',
  maxSize: '16 MiB',
};

/** pointKey identifies one capture point of the catalog. */
export function pointKey(p: { pipeline: string; point: string; signal: string }): string {
  return JSON.stringify([p.pipeline, p.point, p.signal]);
}

/** parseDuration reads "2m", "90s", "1m30s", "2 min" or a bare number of seconds. */
export function parseDuration(text: string): number | undefined {
  const t = text.trim().toLowerCase();
  if (!t) return undefined;
  if (/^\d+(\.\d+)?$/.test(t)) return Math.round(Number(t));
  const re = /(\d+(?:\.\d+)?)\s*(hours?|h|minutes?|mins?|m|seconds?|secs?|s)/y;
  let total = 0;
  let pos = 0;
  while (pos < t.length) {
    while (t[pos] === ' ') pos++;
    if (pos >= t.length) break;
    re.lastIndex = pos;
    const m = re.exec(t);
    if (!m) return undefined;
    const unit = m[2][0];
    total += Number(m[1]) * (unit === 'h' ? 3600 : unit === 'm' ? 60 : 1);
    pos = re.lastIndex;
  }
  return Math.round(total);
}

/** formatDuration writes seconds as "2m", "90s" or "1m30s". */
export function formatDuration(seconds: number): string {
  const s = Math.max(0, Math.round(seconds));
  const m = Math.floor(s / 60);
  const r = s % 60;
  if (m === 0) return `${r}s`;
  return r === 0 ? `${m}m` : `${m}m${r}s`;
}

/** parseSize reads "50 MB", "64MiB", "512k" or a bare number of bytes. Units are powers of 1024. */
export function parseSize(text: string): number | undefined {
  const m = /^\s*(\d+(?:\.\d+)?)\s*([kmg]?)(?:i?b)?\s*$/i.exec(text);
  if (!m) return undefined;
  const mult = { '': 1, k: 1024, m: 1024 ** 2, g: 1024 ** 3 }[m[2].toLowerCase() as '' | 'k' | 'm' | 'g'];
  return Math.round(Number(m[1]) * mult);
}

/** formatSize writes bytes as a whole number of KiB, MiB or GiB when exact, else bytes. */
export function formatSize(bytes: number): string {
  for (const [unit, size] of [['GiB', 1024 ** 3], ['MiB', 1024 ** 2], ['KiB', 1024]] as const) {
    if (bytes >= size && bytes % size === 0) return `${bytes / size} ${unit}`;
  }
  return `${bytes} B`;
}

/** parseCount reads a positive whole number, tolerating thousands separators. */
export function parseCount(text: string): number | undefined {
  const t = text.replace(/[,_\s]/g, '');
  return /^\d+$/.test(t) ? Number(t) : undefined;
}

/** Preset sampling choices; the current rate is added when it is not one of them. */
export const SAMPLING_PRESETS: { rate: number; label: string }[] = [
  { rate: 1, label: 'All records' },
  { rate: 0.5, label: '1 in 2 records' },
  { rate: 0.1, label: '1 in 10 records' },
  { rate: 0.01, label: '1 in 100 records' },
];

export function samplingOptions(rate: number): { rate: number; label: string }[] {
  if (SAMPLING_PRESETS.some((p) => Math.abs(p.rate - rate) < 1e-9)) return SAMPLING_PRESETS;
  return [...SAMPLING_PRESETS, { rate, label: `${Math.round(rate * 10000) / 100}% of records` }];
}

/** samplingText describes a rate for the review panel: "all records" or "1 in 10". */
export function samplingText(rate: number): string {
  if (rate >= 1) return 'all records';
  const inv = 1 / rate;
  if (Math.abs(inv - Math.round(inv)) < 1e-6) return `1 in ${Math.round(inv)}`;
  return `${Math.round(rate * 10000) / 100}% of records`;
}

const lines = (text: string) => text.split('\n').map((l) => l.trim()).filter(Boolean);

export type FieldProblem = { field: 'points' | 'filter' | 'sampling' | 'duration' | 'maxRecords' | 'maxSize' | 'targets'; message: string };

/**
 * The Collectors a capture names: chosen ones, or a fleet group that fleet
 * resolves when the capture starts. `reachable` is how many connected group
 * Collectors report every chosen point now.
 */
export type CaptureTargets = string[] | { groupId: string; reachable: number };

export interface BuildResult {
  selection?: CaptureSelection;
  problems: FieldProblem[];
  /** Signals the chosen points cover. */
  signals: string[];
}

/**
 * buildSelection turns the form into a CaptureSelection for the given
 * targets. `catalog` is the set of points the targets offer; a selected
 * point missing from it is dropped (a changed target set resets the choice,
 * see reconcilePoints). `limits` bound the filter (filterLimitsFor). Problems
 * list everything that blocks starting.
 */
export function buildSelection(targets: CaptureTargets, catalog: CatalogPoint[], form: CaptureForm, limits: FilterLimits = V1_FILTER_LIMITS): BuildResult {
  const problems: FieldProblem[] = [];
  const chosen = catalog.filter((p) => form.points.includes(pointKey(p)));
  const signals = ['traces', 'logs', 'metrics'].filter((s) => chosen.some((p) => p.signal === s));

  const count = Array.isArray(targets) ? targets.length : targets.reachable;
  if (!Array.isArray(targets) && !targets.groupId) problems.push({ field: 'targets', message: 'Choose a group.' });
  else if (count < 1) problems.push({ field: 'targets', message: Array.isArray(targets) ? 'Choose at least one Collector.' : 'No connected Collector in this group reports every chosen point.' });
  if (count > MAX_COLLECTORS) problems.push({ field: 'targets', message: Array.isArray(targets) ? `One capture supports up to ${MAX_COLLECTORS} Collectors.` : `More than ${MAX_COLLECTORS} Collectors in this group report these points; one capture supports up to ${MAX_COLLECTORS}.` });
  if (chosen.length < 1) problems.push({ field: 'points', message: 'Choose at least one capture point.' });
  if (chosen.length > MAX_POINTS) problems.push({ field: 'points', message: `One capture supports up to ${MAX_POINTS} points.` });

  const seconds = parseDuration(form.duration);
  if (seconds === undefined) problems.push({ field: 'duration', message: 'Write the duration like 2m, 90s or 120.' });
  else if (seconds < MIN_DURATION_SECONDS || seconds > MAX_DURATION_SECONDS) problems.push({ field: 'duration', message: `A capture lasts ${MIN_DURATION_SECONDS} seconds to ${formatDuration(MAX_DURATION_SECONDS)}.` });

  const records = parseCount(form.maxRecords);
  if (records === undefined || records < 1) problems.push({ field: 'maxRecords', message: 'Enter a whole number of records.' });
  else if (records > MAX_TOTAL_RECORDS) problems.push({ field: 'maxRecords', message: `At most ${formatCount(MAX_TOTAL_RECORDS)} records.` });

  const bytes = parseSize(form.maxSize);
  if (bytes === undefined || bytes < 1) problems.push({ field: 'maxSize', message: 'Write the size like 50 MiB.' });
  else if (bytes > MAX_TOTAL_BYTES) problems.push({ field: 'maxSize', message: `At most ${formatSize(MAX_TOTAL_BYTES)}.` });

  if (!(form.samplingRate > 0 && form.samplingRate <= 1)) problems.push({ field: 'sampling', message: 'Sampling must keep between 0% and 100% of records.' });

  const filter: CaptureSelection['filter'] = {};
  const resource = lines(form.resource);
  const logs = signals.includes('logs') ? lines(form.logs) : [];
  const traces = signals.includes('traces') ? lines(form.traces) : [];
  const metrics = signals.includes('metrics') ? lines(form.metrics) : [];
  const datapoint = signals.includes('metrics') ? lines(form.datapoint) : [];
  const all = [...resource, ...logs, ...traces, ...metrics, ...datapoint];
  if (all.length > limits.max_conditions) problems.push({ field: 'filter', message: `These Collectors compile at most ${limits.max_conditions} filter conditions in all.` });
  // The Collector counts characters (code points), not bytes.
  if (all.some((c) => [...c].length > limits.max_condition_chars)) problems.push({ field: 'filter', message: `Each condition may be at most ${limits.max_condition_chars} characters.` });
  if (resource.length) filter.resource = resource;
  if (logs.length) filter.logs = { log: logs };
  if (traces.length) filter.traces = { span: traces };
  if (metrics.length || datapoint.length) filter.metrics = { ...(metrics.length ? { metric: metrics } : {}), ...(datapoint.length ? { datapoint } : {}) };

  if (problems.length) return { problems, signals };
  return {
    problems,
    signals,
    selection: {
      ...(Array.isArray(targets) ? { collector_ids: targets } : { group_id: targets.groupId }),
      capture_points: chosen.map((p) => ({ pipeline: p.pipeline, point: p.point })),
      signals,
      duration_seconds: seconds,
      filter,
      sampling: { mode: 'hash', rate: form.samplingRate },
      limits: { bytes_per_second: 1 << 20, records_per_second: 5000, total_bytes: bytes!, total_records: records!, record_bytes: 64 << 10 },
    },
  };
}

/** formFromSelection seeds the form from an earlier capture's request (Duplicate). */
export function formFromSelection(selection: CaptureSelection, catalog: CatalogPoint[]): CaptureForm {
  const wanted = new Set((selection.capture_points ?? []).map((p) => `${p.pipeline}\u0000${p.point}`));
  const points = catalog.filter((p) => wanted.has(`${p.pipeline}\u0000${p.point}`)).map(pointKey);
  const join = (v?: string[]) => (v ?? []).join('\n');
  return {
    points,
    resource: join(selection.filter?.resource),
    logs: join(selection.filter?.logs?.log),
    traces: join(selection.filter?.traces?.span),
    metrics: join(selection.filter?.metrics?.metric),
    datapoint: join(selection.filter?.metrics?.datapoint),
    samplingRate: selection.sampling?.rate ?? 1,
    duration: formatDuration(selection.duration_seconds ?? 60),
    maxRecords: String(selection.limits?.total_records || defaultForm.maxRecords),
    maxSize: formatSize(selection.limits?.total_bytes || MAX_TOTAL_BYTES),
  };
}

/** isFormDirty is true once the user changed anything from the defaults. */
export function isFormDirty(form: CaptureForm): boolean {
  return JSON.stringify(form) !== JSON.stringify(defaultForm);
}

/** reconcilePoints keeps the chosen points that the new catalog still offers. */
export function reconcilePoints(form: CaptureForm, catalog: CatalogPoint[]): CaptureForm {
  const offered = new Set(catalog.map(pointKey));
  return { ...form, points: form.points.filter((k) => offered.has(k)) };
}

export interface PointRow {
  key: string;
  point: CatalogPoint;
  /** How many of the targets report the point. */
  availableOn: number;
  /** The targets that do not report it (pods). */
  missingOn: string[];
  /** Selectable when every target reports it, or, for a group, when any member does. */
  selectable: boolean;
  /** Why it cannot be selected, or which members a group capture leaves out. */
  reason?: string;
}

/**
 * pointRows lays out the capture-point table. `catalogs` is each target's own
 * point list in `targets` order. Chosen Collectors must all report a point.
 * A group capture (`group`) reaches only the members that report every chosen
 * point, so any member's point is selectable. Raw points need fleet's raw
 * permission, which only the start request can check.
 */
export function pointRows(targets: Pick<Collector, 'pod'>[], catalogs: CatalogPoint[][], group = false): PointRow[] {
  const rows = new Map<string, { point: CatalogPoint; on: number[] }>();
  catalogs.forEach((catalog, index) => {
    for (const point of catalog) {
      const key = pointKey(point);
      const row = rows.get(key) ?? { point: { ...point }, on: [] };
      row.on.push(index);
      if (point.sensitivity === 'raw') row.point.sensitivity = 'raw';
      rows.set(key, row);
    }
  });
  const order = (a: CatalogPoint, b: CatalogPoint) =>
    ['logs', 'traces', 'metrics'].indexOf(a.signal) - ['logs', 'traces', 'metrics'].indexOf(b.signal) ||
    (a.point === b.point ? a.pipeline.localeCompare(b.pipeline) : a.point === 'ingress' ? -1 : b.point === 'ingress' ? 1 : a.point.localeCompare(b.point));
  return [...rows.entries()]
    .map(([key, row]): PointRow => {
      const missingOn = targets.filter((_, i) => !row.on.includes(i)).map((t) => t.pod);
      return {
        key,
        point: row.point,
        availableOn: row.on.length,
        missingOn,
        selectable: group || missingOn.length === 0,
        reason: !missingOn.length ? undefined : group ? `${missingOn.join(', ')} left out` : `not on ${missingOn.join(', ')}`,
      };
    })
    .sort((a, b) => order(a.point, b.point));
}

/**
 * reachableTargets is the group members a capture of `points` would reach:
 * those reporting every chosen point. `catalogs` is in `targets` order.
 */
export function reachableTargets<T>(targets: T[], catalogs: CatalogPoint[][], points: string[]): T[] {
  if (!points.length) return [];
  return targets.filter((_, i) => {
    const offered = new Set((catalogs[i] ?? []).map(pointKey));
    return points.every((k) => offered.has(k));
  });
}
