import type { Batching } from '../api/types';

/** sampleBatching is the batching of samples/batch-timeline from a real run. */
export const sampleBatching: Batching = {
  processor: 'batch', pipeline: 'logs', signal: 'logs',
  settings: { send_batch_size: 4, send_batch_max_size: 6, timeout: '300ms', metadata_cardinality_limit: 1000 },
  arrivals: [
    { seq: 4, batcher: 0, offset_ms: 2.5, items: 2, resources: 1, services: ['checkout'] }, { seq: 6, batcher: 0, offset_ms: 2.5, items: 1, resources: 1, services: ['cart'] },
    { seq: 8, batcher: 0, offset_ms: 2.5, items: 2, resources: 1, services: ['checkout'] }, { seq: 12, batcher: 0, offset_ms: 503.4, items: 1, resources: 1, services: ['checkout'] },
    { seq: 16, batcher: 0, offset_ms: 1003.8, items: 9, resources: 1, services: ['inventory'] },
  ],
  flushes: [
    { seq: 9, batcher: 0, offset_ms: 2.5, items: 5, resources: 3, distinct_resources: 2, services: ['checkout', 'cart', 'checkout'], trigger: 'size', pending: 5, left: 0,
      sources: [{ arrival: 4, items: 2 }, { arrival: 6, items: 1 }, { arrival: 8, items: 2 }], wait_ms: { min: 0, max: 0 } },
    { seq: 13, batcher: 0, offset_ms: 603.2, items: 1, resources: 1, distinct_resources: 1, services: ['checkout'], trigger: 'timeout', pending: 1, left: 0,
      sources: [{ arrival: 12, items: 1 }], wait_ms: { min: 99.8, max: 99.8 } },
    { seq: 17, batcher: 0, offset_ms: 1003.8, items: 6, resources: 1, distinct_resources: 1, services: ['inventory'], trigger: 'size', split: true, pending: 9, left: 3,
      sources: [{ arrival: 16, items: 6 }], wait_ms: { min: 0, max: 0 } },
    { seq: 19, batcher: 0, offset_ms: 1003.8, items: 3, resources: 1, distinct_resources: 1, services: ['inventory'], trigger: 'shutdown', pending: 3, left: 0,
      sources: [{ arrival: 16, items: 3 }], wait_ms: { min: 0, max: 0 } },
  ],
  verification: 'metrics_agree',
};

/** metadataBatching is the batching of samples/batch-metadata from a real run. */
export const metadataBatching: Batching = {
  processor: 'batch', pipeline: 'logs', signal: 'logs',
  settings: { send_batch_size: 4, send_batch_max_size: 0, timeout: '300ms', metadata_keys: ['tenant'], metadata_cardinality_limit: 1000 },
  arrivals: [
    { seq: 4, batcher: 0, offset_ms: 3, items: 2, resources: 1, services: ['checkout'] }, { seq: 6, batcher: 1, offset_ms: 3, items: 2, resources: 1, services: ['cart'] },
    { seq: 8, batcher: 0, offset_ms: 3, items: 2, resources: 1, services: ['checkout'] }, { seq: 10, batcher: 2, offset_ms: 3, items: 1, resources: 1, services: ['inventory'] },
  ],
  flushes: [
    { seq: 11, batcher: 0, offset_ms: 3, items: 4, resources: 2, distinct_resources: 1, services: ['checkout', 'checkout'], trigger: 'size', pending: 4, left: 0,
      sources: [{ arrival: 4, items: 2 }, { arrival: 8, items: 2 }], wait_ms: { min: 0, max: 0 } },
    { seq: 13, batcher: 2, offset_ms: 303.4, items: 1, resources: 1, distinct_resources: 1, services: ['inventory'], trigger: 'timeout', pending: 1, left: 0,
      sources: [{ arrival: 10, items: 1 }], wait_ms: { min: 300.4, max: 300.4 } },
    { seq: 15, batcher: 1, offset_ms: 303.4, items: 2, resources: 1, distinct_resources: 1, services: ['cart'], trigger: 'timeout', pending: 2, left: 0,
      sources: [{ arrival: 6, items: 2 }], wait_ms: { min: 300.4, max: 300.4 } },
  ],
  batchers: [
    { metadata: { tenant: ['acme'] }, arrivals: 2, items: 4, flushes: 1 },
    { metadata: { tenant: ['globex'] }, arrivals: 1, items: 2, flushes: 1 },
    { metadata: { tenant: [] }, arrivals: 1, items: 1, flushes: 1 },
  ],
  verification: 'metrics_agree',
};

/** normalized strips the times, as Report.Normalize does. */
export function normalized(b: Batching): Batching {
  return {
    ...b,
    arrivals: b.arrivals?.map(({ offset_ms: _o, ...a }) => a),
    flushes: b.flushes?.map(({ offset_ms: _o, wait_ms: _w, ...f }) => f),
  };
}
