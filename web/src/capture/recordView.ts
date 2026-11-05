import type { CaptureStream } from '../api/types';

/**
 * Pure view logic for a capture's received records: the one-line summary of an
 * OTLP payload, and the grouping of selected records into the per-stream
 * selections the platform derives inputs from.
 */

export interface RecordSummary {
  signal: 'logs' | 'traces' | 'metrics' | 'unknown';
  /** Severity text for a log, SPAN or METRIC otherwise. */
  kind: string;
  tone: 'error' | 'warn' | undefined;
  /** The log body, span name or metric name. */
  text: string;
}

type Any = Record<string, unknown> | undefined;
const first = (v: unknown): Any => (Array.isArray(v) ? (v[0] as Any) : undefined);

/** anyValueText writes an OTLP AnyValue as short text. */
export function anyValueText(v: unknown): string {
  if (v === null || v === undefined) return '';
  if (typeof v !== 'object') return String(v);
  const o = v as Record<string, unknown>;
  for (const key of ['stringValue', 'intValue', 'doubleValue', 'boolValue']) if (o[key] !== undefined) return String(o[key]);
  return JSON.stringify(v);
}

/** severityName maps an OTLP severity number to its range name. */
export function severityName(n: number): string {
  if (n >= 21) return 'FATAL';
  if (n >= 17) return 'ERROR';
  if (n >= 13) return 'WARN';
  if (n >= 9) return 'INFO';
  if (n >= 5) return 'DEBUG';
  if (n >= 1) return 'TRACE';
  return 'UNSET';
}

/** summarizeRecord describes one received record from its OTLP JSON payload. */
export function summarizeRecord(payload: unknown, sequence: number): RecordSummary {
  const p = (payload ?? {}) as Record<string, unknown>;
  const log = first(first(first(p.resourceLogs)?.scopeLogs)?.logRecords);
  if (log) {
    const number = typeof log.severityNumber === 'number' ? log.severityNumber : Number(log.severityNumber) || 0;
    const kind = (typeof log.severityText === 'string' && log.severityText ? log.severityText : severityName(number)).toUpperCase();
    return { signal: 'logs', kind, tone: number >= 17 ? 'error' : number >= 13 ? 'warn' : undefined, text: anyValueText(log.body) || `Log record ${sequence}` };
  }
  const span = first(first(first(p.resourceSpans)?.scopeSpans)?.spans);
  if (span) return { signal: 'traces', kind: 'SPAN', tone: undefined, text: typeof span.name === 'string' && span.name ? span.name : `Span ${sequence}` };
  const metric = first(first(first(p.resourceMetrics)?.scopeMetrics)?.metrics);
  if (metric) return { signal: 'metrics', kind: 'METRIC', tone: undefined, text: typeof metric.name === 'string' && metric.name ? metric.name : `Metric ${sequence}` };
  return { signal: 'unknown', kind: '—', tone: undefined, text: `Record ${sequence}` };
}

/** clip shortens text for a table cell. */
export function clip(text: string, max = 160): string {
  const one = text.replace(/\s+/g, ' ').trim();
  return one.length > max ? `${one.slice(0, max - 1)}…` : one;
}

export const streamKey = (s: CaptureStream) => JSON.stringify([s.collector_id, s.pipeline, s.point, s.signal]);

/** One stream's chosen records, ready for captureFixture / the handoff. */
export interface StreamSelection {
  stream: CaptureStream;
  sequences: number[];
  watermark: number;
  receiver: string;
}

export const MAX_STREAMS = 16;
export const MAX_SEQUENCES_PER_STREAM = 1000;

/** groupSelection folds selected records (by sequence) into one selection per source stream. */
export function groupSelection(selected: Record<number, CaptureStream>, watermark: number, receiverFor: (s: CaptureStream) => string = () => ''): StreamSelection[] {
  const groups = new Map<string, StreamSelection>();
  for (const [sequence, stream] of Object.entries(selected)) {
    const key = streamKey(stream);
    const group = groups.get(key) ?? { stream, sequences: [], watermark, receiver: receiverFor(stream) };
    group.sequences.push(Number(sequence));
    groups.set(key, group);
  }
  for (const group of groups.values()) group.sequences.sort((a, b) => a - b);
  return [...groups.values()];
}

/** selectionProblem is why a selection cannot become inputs, or undefined. */
export function selectionProblem(groups: StreamSelection[]): string | undefined {
  if (!groups.length) return 'Select received records first.';
  if (groups.length > MAX_STREAMS) return `Select records from at most ${MAX_STREAMS} source streams at once.`;
  if (groups.some((g) => g.sequences.length > MAX_SEQUENCES_PER_STREAM)) return 'Select at most 1,000 records from each source stream at once.';
  return undefined;
}

/** streamLabel names a stream for the selection bar: "logs from otlp · output". */
export function streamLabel(s: CaptureStream): string {
  return `${s.signal ?? 'records'} from ${s.pipeline ?? '?'} · ${s.point ?? '?'}`;
}

/** selectionBarText is the secondary text of the selection bar. */
export function selectionBarText(groups: StreamSelection[]): string {
  if (groups.length === 1) return `1 stream: ${streamLabel(groups[0].stream)}`;
  return `${groups.length} streams`;
}

/** One record of a whole-capture scan: what filters and samples read. */
export interface ScannedRecord {
  sequence: number;
  stream: CaptureStream;
  summary: RecordSummary;
  /** Lower-case text the filter searches: the summary and the whole payload. */
  search: string;
  /** The payload's JSON size, 0 when the page omitted it. */
  bytes: number;
}

/** scanRecord prepares one received record for filtering and sampling. */
export function scanRecord(item: { sequence: number; stream: CaptureStream; payload?: unknown; oversize?: boolean }): ScannedRecord {
  const summary = summarizeRecord(item.payload, item.sequence);
  const json = item.oversize || item.payload === undefined ? '' : JSON.stringify(item.payload);
  return { sequence: item.sequence, stream: item.stream, summary, search: `${summary.kind} ${summary.text} ${json}`.toLowerCase(), bytes: json.length };
}

/** A filter over every record of a capture, not only the page on screen. */
export interface RecordFilter {
  /** Words that must all appear in the record: body, attributes, resource. */
  text: string;
  /** A record kind (severity, SPAN or METRIC), or empty for all. */
  kind: string;
}

export const NO_FILTER: RecordFilter = { text: '', kind: '' };

export const filterActive = (f: RecordFilter) => !!f.text.trim() || !!f.kind;

/** filterRecords keeps the records matching every word of the text and the kind. */
export function filterRecords(records: ScannedRecord[], f: RecordFilter): ScannedRecord[] {
  const words = f.text.toLowerCase().split(/\s+/).filter(Boolean);
  return records.filter((r) => (!f.kind || r.summary.kind === f.kind) && words.every((w) => r.search.includes(w)));
}

/** recordKinds lists the kinds in a scan, with their counts, most frequent first. */
export function recordKinds(records: ScannedRecord[]): { kind: string; count: number }[] {
  const counts = new Map<string, number>();
  for (const r of records) counts.set(r.summary.kind, (counts.get(r.summary.kind) ?? 0) + 1);
  return [...counts].map(([kind, count]) => ({ kind, count })).sort((a, b) => b.count - a.count || a.kind.localeCompare(b.kind));
}

export type SampleId = 'first' | 'spread' | 'kinds';

/** The sampling templates: ways to take a subset rather than every record. */
export const SAMPLES: { id: SampleId; label: string }[] = [
  { id: 'first', label: 'First 100' },
  { id: 'spread', label: '100 spread across the capture' },
  { id: 'kinds', label: 'Up to 10 of each kind' },
];

/** sampleRecords applies a template to records in capture order. */
export function sampleRecords(records: ScannedRecord[], id: SampleId): ScannedRecord[] {
  if (id === 'first') return records.slice(0, 100);
  if (id === 'spread') {
    if (records.length <= 100) return records;
    return Array.from({ length: 100 }, (_, i) => records[Math.floor((i * records.length) / 100)]);
  }
  const taken = new Map<string, number>();
  return records.filter((r) => {
    const n = taken.get(r.summary.kind) ?? 0;
    taken.set(r.summary.kind, n + 1);
    return n < 10;
  });
}

/** asSelection is a selection of exactly these records. */
export function asSelection(records: ScannedRecord[]): Record<number, CaptureStream> {
  return Object.fromEntries(records.map((r) => [r.sequence, r.stream]));
}

/** Above this many records, a selection is called out as large. */
export const LARGE_SELECTION = 200;
const LARGE_BYTES = 1024 * 1024;

/**
 * selectionWarning calls out a selection that makes a large scenario, before
 * the platform's own limits refuse it. `bytes` is what the selected payloads
 * add up to, when known.
 */
export function selectionWarning(count: number, bytes?: number): string | undefined {
  if (count <= LARGE_SELECTION && (bytes ?? 0) <= LARGE_BYTES) return undefined;
  const size = bytes ? ` (${bytes >= LARGE_BYTES ? `${(bytes / LARGE_BYTES).toFixed(1)} MiB` : `${Math.ceil(bytes / 1024)} KiB`})` : '';
  return `${count.toLocaleString('en-US')} records${size} make a large scenario: every run replays all of them. A sample of 100 usually shows the same behavior.`;
}
