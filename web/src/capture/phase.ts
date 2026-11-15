import type { IconName } from '../ui/Icon';
import { formatCount } from './format';

/**
 * The capture lifecycle as the Live capture screens show it. A session's
 * `state` is fleet's operation state (starting, active, stopping, stopped,
 * completed, expired, failed, interrupted), or `uncertain` when the debugger
 * lost track of the capture. This module turns it and the per-target counters
 * into one displayed phase. A closed capture says when it may be incomplete.
 */

/** The fields of CaptureSessionSummary (and CaptureSession, via sessionCounts) the phase needs. */
export interface PhaseInput {
  state: string;
  terminal: boolean;
  target_count: number;
  active_count: number;
  /** Targets fleet no longer reports on. */
  uncertain_count: number;
  failed_count: number;
  /** Why the capture ended, when known (full sessions only). */
  reason?: string;
  /** Records the Collectors or fleet lost on the way (full sessions only); see lostRecords. */
  dropped_records?: number;
}

export type CapturePhaseId = 'starting' | 'receiving' | 'stopping' | 'closed' | 'partial' | 'failed' | 'unknown';
export type PhaseTone = 'running' | 'warn' | 'pass' | 'fail' | 'plain';

export interface CapturePhase {
  id: CapturePhaseId;
  /** The chip text. */
  label: string;
  tone: PhaseTone;
  icon: IconName;
  /** Whether the capture can still receive records or change state. */
  live: boolean;
  /** One-sentence status for the callout above the page, when one is needed. */
  headline?: string;
  /** Supporting sentence for the callout. */
  detail?: string;
}

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

const reasons: Record<string, string> = {
  absolute_expiry: 'The capture window ended.',
  collector_rejected: 'A Collector rejected the capture.',
  peer_disconnected: 'A Collector disconnected from fleet.',
  stop_requested: 'Stopped on request.',
  capture_storage_error: 'The debugger could not store the records.',
  capture_storage_limit: 'The debugger reached its storage limit for this capture.',
  capture_processing_limit: 'The debugger reached its processing limit for this capture.',
  capture_catalog_changed: 'A Collector changed its capture points.',
  capture_channel_lost: 'The connection to fleet for this capture was lost.',
  fleet_no_longer_reports_capture: 'Fleet no longer reports this capture.',
  no_terminal_state_from_fleet_after_expiry: 'Fleet did not report an end after the capture window.',
  consumer_unavailable: 'The debugger stopped reading this capture from fleet.',
};

/** What each drop reason means. Collector reasons are the session's `dropped` stats; `fleet` is fleet's delivery buffer. */
const dropLabels: Record<string, string> = {
  queue_full: 'Collector queue full',
  delivery: 'export to fleet failed',
  rate_records: 'record rate limit',
  rate_bytes: 'byte rate limit',
  record_size: 'record too large',
  time: 'Collector time budget',
  budget: 'capture limit reached',
  sampling: 'sampled out',
  stopping: 'arrived while stopping',
  fleet: 'fleet buffer full or not read in time',
};

/** Drop reasons the capture's own settings ask for; they do not make it incomplete. */
const chosenDrops = new Set(['sampling', 'budget', 'stopping']);

/** The per-target counters the drop helpers read. */
export interface TargetCounters {
  stats?: { dropped?: Record<string, number> };
  delivery?: { dropped_records: number };
}

/** targetDrops lists a target's nonzero drops with their meaning, largest first. */
export function targetDrops(t: TargetCounters): { reason: string; label: string; count: number }[] {
  const all: [string, number][] = Object.entries(t.stats?.dropped ?? {});
  if (t.delivery?.dropped_records) all.push(['fleet', t.delivery.dropped_records]);
  return all
    .filter(([, count]) => count > 0)
    .map(([reason, count]) => ({ reason, label: dropLabels[reason] ?? reason.replace(/_/g, ' '), count }))
    .sort((a, b) => b.count - a.count || a.reason.localeCompare(b.reason));
}

/**
 * lostRecords counts a target's records lost on the way: Collector drops other
 * than sampling, the record limit and the stop, plus fleet's delivery drops.
 * Any loss makes the capture incomplete.
 */
export function lostRecords(t: TargetCounters): number {
  return targetDrops(t).reduce((sum, d) => sum + (chosenDrops.has(d.reason) ? 0 : d.count), 0);
}

/** reasonText turns a fleet or debugger reason code into a sentence. */
export function reasonText(code?: string): string | undefined {
  if (!code) return undefined;
  return code
    .split(';')
    .map((part) => part.trim())
    .filter(Boolean)
    .map((part) => reasons[part] ?? `${part.replace(/_/g, ' ').replace(/^./, (c) => c.toUpperCase())}.`)
    .join(' ');
}

/** capturePhase derives the displayed phase from a session's state and counters. */
export function capturePhase(s: PhaseInput): CapturePhase {
  if (!s.terminal) {
    switch (s.state) {
      case 'starting':
        return { id: 'starting', label: 'Starting', tone: 'running', icon: 'loader-circle', live: true };
      case 'active':
        return s.dropped_records
          ? {
              id: 'receiving',
              label: 'Receiving',
              tone: 'running',
              icon: 'loader-circle',
              live: true,
              headline: `Receiving · ${plural(s.dropped_records, 'record')} dropped so far`,
              detail: 'The Collectors or fleet dropped them to stay within their limits; the capture will be incomplete.',
            }
          : { id: 'receiving', label: 'Receiving', tone: 'running', icon: 'loader-circle', live: true };
      case 'stopping':
        return {
          id: 'stopping',
          label: 'Stopping',
          tone: 'warn',
          icon: 'circle-pause',
          live: true,
          headline: 'Stop requested · waiting for the Collectors to release the capture',
          detail: 'Records may still arrive until every Collector has stopped or the capture window ends.',
        };
      default:
        return { id: 'unknown', label: s.state || 'Unknown', tone: 'plain', icon: 'circle-help', live: true };
    }
  }
  const why = reasonText(s.reason);
  switch (s.state) {
    case 'failed':
      return { id: 'failed', label: 'Failed', tone: 'fail', icon: 'circle-x', live: false, headline: 'The capture failed', detail: why };
    case 'interrupted':
      return {
        id: 'partial',
        label: 'Closed · interrupted',
        tone: 'warn',
        icon: 'triangle-alert',
        live: false,
        headline: 'Fleet interrupted the capture',
        detail: why ?? 'Its records may be incomplete.',
      };
    case 'uncertain':
      return {
        id: 'partial',
        label: 'Closed · uncertain',
        tone: 'warn',
        icon: 'triangle-alert',
        live: false,
        headline: 'The debugger lost track of this capture',
        detail: `${why ? `${why} ` : ''}Its records may be incomplete. Fleet still ends the capture at its expiry.`,
      };
  }
  if (s.uncertain_count > 0 || s.failed_count > 0) {
    const unsure = s.uncertain_count + s.failed_count;
    return {
      id: 'partial',
      label: 'Closed · incomplete',
      tone: 'warn',
      icon: 'triangle-alert',
      live: false,
      headline: `Closed · ${plural(unsure, 'Collector')} did not finish cleanly`,
      detail: 'Their records may be incomplete.',
    };
  }
  if (s.dropped_records) {
    return {
      id: 'partial',
      label: 'Closed · incomplete',
      tone: 'warn',
      icon: 'triangle-alert',
      live: false,
      headline: `Closed · ${plural(s.dropped_records, 'record')} dropped`,
      detail: `${why ? `${why} ` : ''}The Collectors or fleet dropped them to stay within their limits; the stored records are incomplete.`,
    };
  }
  if (s.state === 'stopped' || s.state === 'completed' || s.state === 'expired') {
    return { id: 'closed', label: 'Closed', tone: 'pass', icon: 'circle-check', live: false, detail: why };
  }
  return { id: 'unknown', label: s.state || 'Unknown', tone: 'plain', icon: 'circle-help', live: false, detail: why };
}

export type StepStatus = 'done' | 'current' | 'pending' | 'warn' | 'failed';

export interface LifecycleStep {
  id: 'requested' | 'accepted' | 'receiving' | 'stopping' | 'closed';
  title: string;
  detail: string;
  status: StepStatus;
  /** ISO time the step happened, when the platform reports one. */
  time?: string;
}

export interface LifecycleInput extends PhaseInput {
  created_at: string;
  expires_at: string;
  received_records: number;
  /** Targets past `pending` that did not fail. */
  accepted_count?: number;
  /** Targets that have not ended yet. */
  open_count?: number;
}

/** captureLifecycle lays the phase out as the five lifecycle steps of the detail page. */
export function captureLifecycle(s: LifecycleInput): LifecycleStep[] {
  const phase = capturePhase(s);
  const accepted = s.accepted_count ?? (phase.id === 'starting' ? 0 : s.target_count - s.failed_count);
  const dropped = s.dropped_records ? ` · ${formatCount(s.dropped_records)} dropped` : '';
  const records = `${formatCount(s.received_records)} record${s.received_records === 1 ? '' : 's'}${dropped}`;
  const started = phase.id !== 'starting';
  const ended = !phase.live;
  const stoppedOrBeyond = phase.id === 'stopping' || ended;
  const bad = phase.id === 'partial' || phase.id === 'failed';
  const open = s.open_count;
  return [
    { id: 'requested', title: 'Requested', detail: 'by you', status: 'done', time: s.created_at },
    {
      id: 'accepted',
      title: 'Targets accepted',
      detail: `${accepted} of ${plural(s.target_count, 'Collector')}`,
      status: phase.id === 'failed' && accepted === 0 ? 'failed' : started ? (accepted < s.target_count ? 'warn' : 'done') : 'current',
    },
    {
      id: 'receiving',
      title: 'Receiving',
      detail: records,
      status: !started ? 'pending' : s.dropped_records ? 'warn' : stoppedOrBeyond ? 'done' : 'current',
    },
    {
      id: 'stopping',
      title: 'Stopping',
      detail:
        phase.id === 'stopping'
          ? open !== undefined
            ? `stop requested · ${plural(open, 'Collector')} still open`
            : 'stop requested'
          : ended
            ? reasonText(s.reason) ?? ''
            : 'at the end of the capture window or its record limit',
      status: phase.id === 'stopping' || bad ? 'warn' : ended ? 'done' : 'pending',
    },
    {
      id: 'closed',
      title: 'Closed',
      detail: phase.id === 'failed' ? 'failed' : phase.id === 'partial' ? 'records may be incomplete' : '',
      status: phase.id === 'closed' ? 'done' : phase.id === 'failed' ? 'failed' : phase.id === 'partial' ? 'warn' : ended ? 'done' : 'pending',
    },
  ];
}

/** Elapsed time against the capture window. */
export interface ElapsedView {
  /** Whole seconds shown as m:ss. */
  seconds: number;
  /** 0 to 100. */
  percent: number;
  text: string;
}

/**
 * elapsedView measures a capture against its window. The platform reports no
 * end time, so a live capture is measured to `now` and a closed one is shown
 * as its whole window.
 */
export function elapsedView(s: { created_at: string; expires_at: string; terminal: boolean }, now: number): ElapsedView {
  const start = Date.parse(s.created_at);
  const end = Date.parse(s.expires_at);
  const window = Math.max(0, Math.round((end - start) / 1000));
  const seconds = s.terminal ? window : Math.min(window, Math.max(0, Math.round((now - start) / 1000)));
  const percent = window === 0 ? 100 : Math.min(100, Math.round((seconds / window) * 100));
  return { seconds, percent, text: formatClock(seconds) };
}

/** formatClock writes seconds as m:ss (or h:mm:ss). */
export function formatClock(total: number): string {
  const t = Math.max(0, Math.floor(total));
  const h = Math.floor(t / 3600);
  const m = Math.floor((t % 3600) / 60);
  const s = t % 60;
  const ss = String(s).padStart(2, '0');
  return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${ss}` : `${m}:${ss}`;
}

/** targetChip shows one Collector's state in a capture as fleet reports it. */
export function targetChip(t: { state: string; terminal: boolean }): {
  label: string;
  tone: PhaseTone;
  icon: IconName;
} {
  switch (t.state) {
    case 'pending':
      return { label: 'Starting', tone: 'plain', icon: 'clock' };
    case 'active':
      return { label: 'Receiving', tone: 'running', icon: 'loader-circle' };
    case 'stopping':
      return { label: 'Stopping', tone: 'warn', icon: 'circle-pause' };
    case 'unknown':
      return { label: 'No longer reported', tone: 'warn', icon: 'triangle-alert' };
    case 'failed':
      return { label: 'Failed', tone: 'fail', icon: 'circle-x' };
    case 'disabled':
      return { label: 'Capture disabled', tone: 'plain', icon: 'circle-minus' };
    case 'limit_reached':
      return { label: 'Limit reached', tone: 'pass', icon: 'circle-check' };
    case 'expired':
      return { label: 'Window ended', tone: 'pass', icon: 'circle-check' };
    case 'completed':
      return { label: 'Completed', tone: 'pass', icon: 'circle-check' };
    default:
      return { label: t.state || 'Unknown', tone: 'plain', icon: 'circle-help' };
  }
}

/** The od-chip modifier class for a tone. */
export function chipClass(tone: PhaseTone): string {
  return tone === 'plain' ? 'od-chip' : `od-chip od-chip--${tone}`;
}

/** The od-step modifier for a lifecycle status. */
export function stepClass(status: StepStatus): string {
  return `od-step od-step--${status}`;
}

/** A full session (CaptureSession) reduced to the counters a summary carries. */
export function sessionCounts(s: {
  state: string;
  terminal: boolean;
  reason?: string;
  stop_reason?: string;
  created_at: string;
  expires_at: string;
  data: { received_records: number };
  targets?: ({ state: string; terminal: boolean } & TargetCounters)[];
}): LifecycleInput {
  const targets = s.targets ?? [];
  return {
    state: s.state,
    terminal: s.terminal,
    reason: s.reason || s.stop_reason,
    created_at: s.created_at,
    expires_at: s.expires_at,
    received_records: s.data.received_records,
    target_count: targets.length,
    active_count: targets.filter((t) => t.state === 'active').length,
    uncertain_count: targets.filter((t) => t.state === 'unknown').length,
    failed_count: targets.filter((t) => t.state === 'failed').length,
    accepted_count: targets.filter((t) => t.state !== 'pending' && t.state !== 'failed').length,
    open_count: targets.filter((t) => !t.terminal).length,
    dropped_records: targets.reduce((sum, t) => sum + lostRecords(t), 0),
  };
}
