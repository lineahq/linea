import type { CaptureStream } from '../api/types';

/**
 * The capture-first handoff: records chosen on a capture's page travel to the
 * Debugger, which may not have a configuration yet. The owner decided the
 * selection persists in `sessionStorage` (this tab only, gone with it), so a
 * reload of the Debugger or a detour through opening a configuration keeps it.
 *
 * Contract
 * --------
 * Capture page (producer): `stashCaptureHandoff(...)`, then navigate to `/`.
 *
 * Debugger (consumer, built elsewhere): on mount, `takeCaptureHandoff()`. It
 * reads and clears the entry in one step, so it applies at most once. A
 * returned handoff means "append these captured records as inputs". For each
 * entry of `streams`, once the user has a configuration and has chosen a
 * receiver that accepts the stream's signal (`receiverSignals` over the
 * platform graph; `receiver` here is only a suggestion):
 *
 *   api.captureFixture(handoff.captureId, {
 *     selection: { stream, sequences, watermark, receiver },
 *     config_yaml, setup_yaml,   // from the debugger's draft, as for any input
 *   })
 *
 * then `appendCaptureInputs(scenarioYAML, fixtures)` (capture/inputs.ts) in one
 * draft update. If `containsRaw` is true the draft must stay session-only
 * (`updateWorkspaceSessionOnly`), exactly as for a direct append.
 *
 * What is stored: identifiers only (capture id, stream coordinates, record
 * sequence numbers, the watermark the sequences were read at). Never record
 * payloads, so a raw capture's bytes never touch browser storage. The capture
 * may be deleted or expire before the Debugger reads it; `captureFixture`
 * then fails and the consumer should say so. An entry older than
 * `CAPTURE_HANDOFF_MAX_AGE_MS` is discarded, so an abandoned handoff never
 * surprises a later visit.
 *
 * Every storage access is wrapped: storage may be absent, full or blocked, in
 * which case `stashCaptureHandoff` returns false and the producer must say the
 * selection could not be kept.
 */

export const CAPTURE_HANDOFF_KEY = 'linea.capture-handoff.v1';
export const CAPTURE_HANDOFF_MAX_AGE_MS = 60 * 60 * 1000;
/** Same bounds the platform enforces when deriving inputs (CaptureDataPane). */
export const HANDOFF_MAX_STREAMS = 16;
export const HANDOFF_MAX_SEQUENCES = 1000;

/** One source stream and the records chosen from it. */
export interface CaptureHandoffStream {
  stream: CaptureStream;
  /** Record sequence numbers, ascending and unique. */
  sequences: number[];
  /** The capture's records watermark when the sequences were listed. */
  watermark: number;
  /** A receiver the user already chose for this stream, if any. */
  receiver?: string;
}

export interface CaptureHandoff {
  version: 1;
  captureId: string;
  containsRaw: boolean;
  streams: CaptureHandoffStream[];
  /** ISO time the handoff was stored. */
  stashedAt: string;
}

export type CaptureHandoffInput = Omit<CaptureHandoff, 'version' | 'stashedAt'>;

type Store = Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>;

function defaultStore(): Store | undefined {
  try {
    return typeof sessionStorage === 'undefined' ? undefined : sessionStorage;
  } catch {
    return undefined;
  }
}

/** stashCaptureHandoff stores the selection for the Debugger; false means it could not be kept. */
export function stashCaptureHandoff(input: CaptureHandoffInput, store: Store | undefined = defaultStore(), now: Date = new Date()): boolean {
  const normalized = normalize({ ...input, version: 1, stashedAt: now.toISOString() }, now.getTime());
  if (!normalized || !store) return false;
  try {
    store.setItem(CAPTURE_HANDOFF_KEY, JSON.stringify(normalized));
    return true;
  } catch {
    return false;
  }
}

/** peekCaptureHandoff reads the stored handoff without consuming it. */
export function peekCaptureHandoff(store: Store | undefined = defaultStore(), now: number = Date.now()): CaptureHandoff | undefined {
  let raw: string | null;
  try {
    raw = store?.getItem(CAPTURE_HANDOFF_KEY) ?? null;
  } catch {
    return undefined;
  }
  if (!raw) return undefined;
  try {
    return normalize(JSON.parse(raw), now);
  } catch {
    return undefined;
  }
}

/** takeCaptureHandoff reads and clears the stored handoff. A damaged or stale entry is cleared and ignored. */
export function takeCaptureHandoff(store: Store | undefined = defaultStore(), now: number = Date.now()): CaptureHandoff | undefined {
  const found = peekCaptureHandoff(store, now);
  try {
    store?.removeItem(CAPTURE_HANDOFF_KEY);
  } catch {
    /* Nothing more to do; the entry expires by age. */
  }
  return found;
}

function isInt(n: unknown): n is number {
  return typeof n === 'number' && Number.isSafeInteger(n) && n >= 0;
}

const str = (v: unknown): string | undefined => (typeof v === 'string' && v !== '' ? v : undefined);

/** normalize validates a value read from storage (or about to be written) and returns a clean copy. */
function normalize(value: unknown, now: number): CaptureHandoff | undefined {
  if (!value || typeof value !== 'object') return undefined;
  const v = value as Record<string, unknown>;
  const captureId = str(v.captureId);
  const stashedAt = str(v.stashedAt);
  if (v.version !== 1 || !captureId || !stashedAt || typeof v.containsRaw !== 'boolean' || !Array.isArray(v.streams)) return undefined;
  const at = Date.parse(stashedAt);
  if (Number.isNaN(at) || now - at > CAPTURE_HANDOFF_MAX_AGE_MS || at - now > CAPTURE_HANDOFF_MAX_AGE_MS) return undefined;
  if (v.streams.length < 1 || v.streams.length > HANDOFF_MAX_STREAMS) return undefined;
  const streams: CaptureHandoffStream[] = [];
  for (const entry of v.streams) {
    if (!entry || typeof entry !== 'object') return undefined;
    const e = entry as Record<string, unknown>;
    const s = e.stream as Record<string, unknown> | undefined;
    const collector_id = str(s?.collector_id);
    const pipeline = str(s?.pipeline);
    const point = str(s?.point);
    const signal = str(s?.signal);
    if (!collector_id || !pipeline || !point || !signal) return undefined;
    if (!Array.isArray(e.sequences) || e.sequences.length < 1 || e.sequences.length > HANDOFF_MAX_SEQUENCES || !e.sequences.every(isInt)) return undefined;
    if (!isInt(e.watermark)) return undefined;
    const sequences = [...new Set(e.sequences as number[])].sort((a, b) => a - b);
    const receiver = str(e.receiver);
    streams.push({ stream: { collector_id, pipeline, point, signal }, sequences, watermark: e.watermark, ...(receiver ? { receiver } : {}) });
  }
  return { version: 1, captureId, containsRaw: v.containsRaw, streams, stashedAt };
}
