import type { Stage } from '../api/types';

/**
 * A processor that buffers (batch, tail_sampling, groupbytrace) emits later,
 * on its own schedule, so its output cannot be paired with one input. The
 * stage's reconciliation then compares everything it buffered with
 * everything it emitted later. Its record refs index the concatenation of
 * those snapshots in event order, as the analysis builds them.
 */
export interface DeferredWindow {
  /** Inputs whose output was not paired with them, in event order. */
  inputs: number[];
  /** Outputs emitted on the processor's own schedule, in event order. */
  emissions: number[];
}

/** deferredWindow returns the buffered inputs and later emissions of a stage with a reconciliation. */
export function deferredWindow(stage: Stage | undefined): DeferredWindow | undefined {
  if (!stage?.reconciliation) return undefined;
  return {
    inputs: (stage.inputs ?? []).filter((i) => !i.outputs?.length).map((i) => i.seq),
    emissions: stage.emissions ?? [],
  };
}

const RESOURCE_KEYS = ['resourceSpans', 'resourceMetrics', 'resourceLogs'] as const;

/**
 * concatPayloads appends the resources of OTLP JSON payloads in order,
 * without merging equal resources, matching the analysis's concatenation.
 * Returns undefined while any payload is missing.
 */
export function concatPayloads(payloads: readonly unknown[]): unknown {
  if (payloads.some((p) => !p || typeof p !== 'object')) return undefined;
  const out: Record<string, unknown[]> = {};
  for (const p of payloads as Record<string, unknown>[]) {
    for (const key of RESOURCE_KEYS) {
      const list = p[key];
      if (Array.isArray(list)) out[key] = [...(out[key] ?? []), ...list];
    }
  }
  return out;
}

/** resourceCount is the number of resources in an OTLP JSON payload. */
export function resourceCount(payload: unknown): number {
  if (!payload || typeof payload !== 'object') return 0;
  const p = payload as Record<string, unknown>;
  return RESOURCE_KEYS.reduce((n, key) => n + (Array.isArray(p[key]) ? (p[key] as unknown[]).length : 0), 0);
}

/**
 * eventRecord resolves a record ref in a concatenation back to the event it
 * was recorded in and its ref there. counts are the resource counts of the
 * concatenated payloads, in order, and seqs their events.
 */
export function eventRecord(entity: string, seqs: readonly number[], counts: readonly number[]): { seq: number; entity: string } | undefined {
  const m = /^resource\[(\d+)\]/.exec(entity);
  if (!m) return undefined;
  let index = Number(m[1]);
  for (let i = 0; i < seqs.length; i++) {
    if (index < counts[i]) return { seq: seqs[i], entity: `resource[${index}]${entity.slice(m[0].length)}` };
    index -= counts[i];
  }
  return undefined;
}
