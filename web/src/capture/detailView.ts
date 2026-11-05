import type { CaptureSelection, CaptureSession, CaptureSessionPage } from '../api/types';
import { formatCount, formatBytes, shortId } from './format';
import { elapsedView, type ElapsedView } from './phase';
import { formatDuration, formatSize } from './captureForm';

/**
 * Pure view logic of the capture detail page and the capture list: titles,
 * the four headline numbers, provenance lines and which actions are open.
 */

type Summary = CaptureSessionPage['items'] extends (infer T)[] | undefined ? T : never;

/** captureTitle is the capture's name, or its short ID when it has none. */
export function captureTitle(s: { id: string; metadata?: { name?: string } }): string {
  return s.metadata?.name?.trim() || shortId(s.id);
}

/** targetLabel names a target by its pod, falling back to the Collector ID. */
export function targetLabel(t: { collector_id: string; source?: { pod?: string } }): string {
  return t.source?.pod || t.collector_id;
}

export function startedBy(caller: { principal: string; client: string }): string {
  const who = caller.principal === 'local-owner' ? 'you' : caller.principal;
  const via = { http: 'the web client or API', cli: 'the CLI', mcp: 'MCP' }[caller.client] ?? caller.client;
  return `${who}, from ${via}`;
}

export interface Metrics {
  /** Records the Collectors say they enqueued for delivery. Not proof of receipt. */
  reported: number;
  received: number;
  receivedBytes: number;
  rejected: number;
  elapsed: ElapsedView;
  storage: { headline: string; detail: string };
}

/** metricsView gathers the four headline numbers. */
export function metricsView(s: CaptureSession, now: number): Metrics {
  return {
    reported: (s.targets ?? []).reduce((sum, t) => sum + (t.stats?.enqueued_records ?? 0), 0),
    received: s.data.received_records,
    receivedBytes: s.data.received_bytes,
    rejected: s.data.rejected_records,
    elapsed: elapsedView(s, now),
    storage: storageText(s.data),
  };
}

/** receivedDetail is the line under "Debugger received". */
export function receivedDetail(m: Pick<Metrics, 'receivedBytes' | 'rejected'>): string {
  const rejected = m.rejected > 0 ? ` · ${formatCount(m.rejected)} rejected` : '';
  return `${formatBytes(m.receivedBytes)}${rejected}`;
}

export interface FilterLine {
  label: string;
  conditions: string[];
}

/** filterLines lists the OTTL conditions a capture applied, per kind. */
export function filterLines(filter: CaptureSelection['filter'] | undefined): FilterLine[] {
  const lines: FilterLine[] = [
    { label: 'Resource', conditions: filter?.resource ?? [] },
    { label: 'Logs', conditions: filter?.logs?.log ?? [] },
    { label: 'Spans', conditions: filter?.traces?.span ?? [] },
    { label: 'Metrics', conditions: filter?.metrics?.metric ?? [] },
    { label: 'Data points', conditions: filter?.metrics?.datapoint ?? [] },
  ];
  return lines.filter((l) => l.conditions.length > 0);
}

/** stopAfterText states the bounds a capture stops at, first reached wins. */
export function stopAfterText(selection: CaptureSelection, limits: { total_records: number; total_bytes: number }): string {
  const parts = [formatDuration(selection.duration_seconds ?? 60)];
  if (limits.total_records > 0) parts.push(`${formatCount(limits.total_records)} records`);
  if (limits.total_bytes > 0) parts.push(formatSize(limits.total_bytes));
  return parts.join(' or ');
}

/** The capture points chosen, as "pipeline · point". */
export function pointLines(selection: CaptureSelection): string[] {
  return (selection.capture_points ?? []).map((p) => `${p.pipeline} · ${p.point}`);
}

/** canStop: a stop can be requested while the capture is live and none is pending. */
export function canStop(s: { terminal: boolean; state: string }): boolean {
  return !s.terminal && s.state !== 'stopping';
}

/** deleteBlocker says why a capture cannot be deleted yet, or undefined when it can. */
export function deleteBlocker(s: { terminal: boolean; data: { state: string } }): string | undefined {
  if (!s.terminal) return 'Stop the capture and wait for it to close before deleting it.';
  if (s.data.state === 'open' || s.data.state === 'draining') return 'The capture is still receiving delivered records. Delete it once delivery has finished.';
  return undefined;
}

/** dataNotice is the line that explains the state of the stored records, if there is something to say. */
export function dataNotice(data: CaptureSession['data']): string | undefined {
  if (!data.enabled) return 'Live capture data was unavailable for this capture, so no records were stored.';
  if (data.error) return `Capture storage failed: ${data.error}. Inspect receipt counts; enqueue counts do not prove receipt.`;
  if (data.state === 'lost_on_restart') return 'Memory-only payloads were discarded when the platform restarted.';
  return undefined;
}

/** storageText says where a capture's records are kept. Captures have no retention period. */
export function storageText(data: CaptureSession['data']): { headline: string; detail: string } {
  if (!data.enabled) return { headline: '—', detail: '' };
  if (data.state === 'lost_on_restart') return { headline: 'Lost', detail: 'discarded on restart' };
  if (data.memory_only) return { headline: 'In memory', detail: 'no server storage' };
  return { headline: 'Server storage', detail: 'kept until deleted' };
}

/** storageNote describes where payloads are held and what a capture is. */
export function storageNote(data: CaptureSession['data']): string {
  const where = data.state === 'lost_on_restart' ? 'Memory-only payloads were discarded on restart.' : data.memory_only ? 'Payloads are held in memory because the debugger has no storage.' : 'Accepted payloads are committed to the server storage and kept until the capture is deleted.';
  return `${where} Captures are filtered and sampled subsets, redacted unless taken at a raw point; retries may duplicate records. Limit: ${formatCount(data.record_limit)} records / ${formatBytes(data.byte_limit)}.`;
}

/** dataStateText is the plain-language state of the stored records. */
export function dataStateText(state: string): string {
  if (state === 'open') return 'Receiving';
  if (state === 'frozen') return 'Complete';
  return state.replaceAll('_', ' ');
}

/** isReceiving: the record list refreshes itself only while data can still arrive. */
export function isReceiving(data: { state: string }): boolean {
  return data.state === 'open' || data.state === 'draining';
}

/** sessionsNewestFirst orders list rows by creation time, newest first. */
export function sessionsNewestFirst<T extends { created_at: string }>(items: readonly T[] | undefined): T[] {
  return [...(items ?? [])].sort((a, b) => b.created_at.localeCompare(a.created_at));
}

/** anyLive is true while at least one capture in a list has not finished. */
export function anyLive(items: readonly Pick<Summary, 'terminal'>[] | undefined): boolean {
  return (items ?? []).some((s) => !s.terminal);
}

/** collectorsText: "3 Collectors" or the single pod's name. */
export function collectorsText(count: number): string {
  return `${count} Collector${count === 1 ? '' : 's'}`;
}
