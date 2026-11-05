import type { Event } from '../api/types';
import { eventNodeKey } from '../runs/annotations';

/** A place in the inspector: a graph node and the event seq selected there. */
export interface Step {
  node: string;
  input: number;
  label: string;
}

const where = (e: Event) =>
  e.kind === 'input.injected'
    ? `receiver ${e.component}`
    : e.kind === 'exporter.captured'
      ? `exporter ${e.component}`
      : `${e.component}${e.pipeline && e.source !== 'connector_wrapper' ? ` in ${e.pipeline}` : ''}`;

const stepOf = (e: Event, input = e.seq): Step | undefined => {
  const node = eventNodeKey(e);
  return node ? { node, input, label: where(e) } : undefined;
};

/** stepFrom is where an emitting event (an injection or a stage output) is inspected. */
const stepFrom = (e: Event): Step | undefined =>
  e.kind === 'stage.output' ? stepOf(e, e.input_seq ?? e.seq) : e.kind === 'input.injected' ? stepOf(e) : undefined;

/**
 * prevStep is the stage or receiver that handed over the payload of event
 * inputSeq. Events record the emission that called them (from_seq), which is
 * exact even when the same payload passed several places; reports without
 * it fall back to matching content-addressed snapshots along the pipeline.
 */
export function prevStep(events: Event[], inputSeq: number): Step | undefined {
  const input = events.find((e) => e.seq === inputSeq);
  if (!input) return undefined;
  if (input.from_seq) {
    const from = events.find((e) => e.seq === input.from_seq);
    return from && stepFrom(from);
  }
  if (!input.snapshot) return undefined;
  for (let i = events.length - 1; i >= 0; i--) {
    const e = events[i];
    if (e.seq >= inputSeq || e.snapshot !== input.snapshot) continue;
    if (e.kind === 'stage.output' && (!input.pipeline || e.pipeline === input.pipeline)) return stepOf(e, e.input_seq ?? e.seq);
    if (e.kind === 'input.injected') return stepOf(e);
  }
  return undefined;
}

/**
 * nextSteps are the stages and exporters that consumed the emission
 * outputSeq: several when a receiver or connector fans out to pipelines.
 */
export function nextSteps(events: Event[], outputSeq: number | undefined, pipeline: string | undefined): Step[] {
  const out = outputSeq === undefined ? undefined : events.find((e) => e.seq === outputSeq);
  if (!out) return [];
  const called = events.filter((e) => e.from_seq === out.seq && (e.kind === 'stage.input' || e.kind === 'exporter.captured'));
  if (called.length || events.some((e) => e.from_seq)) {
    return called.map((e) => stepOf(e)).filter((s): s is Step => !!s);
  }
  if (!out.snapshot) return [];
  for (const e of events) {
    if (e.seq <= out.seq || e.snapshot !== out.snapshot) continue;
    if ((e.kind === 'stage.input' && (pipeline === undefined || e.pipeline === pipeline)) || e.kind === 'exporter.captured') {
      const s = stepOf(e);
      return s ? [s] : [];
    }
  }
  return [];
}

/** eventStep is where selecting an event in the event list leads. */
export function eventStep(e: Event): Step | undefined {
  switch (e.kind) {
    case 'stage.input':
    case 'input.injected':
    case 'exporter.captured':
      return stepOf(e);
    case 'stage.output':
    case 'stage.error':
      return e.input_seq !== undefined ? stepOf(e, e.input_seq) : undefined;
  }
  return undefined;
}
