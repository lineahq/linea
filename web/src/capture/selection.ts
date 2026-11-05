import type { CapturePointPage, CaptureSelection } from '../api/types';

export function selectionForPoint(collector: string, point: NonNullable<CapturePointPage['items']>[number]): CaptureSelection {
  return {
    collector_ids: [collector], capture_points: [{ pipeline: point.pipeline, point: point.point }], signals: [point.signal],
    duration_seconds: 60, filter: {}, sampling: { rate: 1, mode: 'hash' },
    limits: { bytes_per_second: 1048576, records_per_second: 5000, total_bytes: 67108864, total_records: 100000, record_bytes: 65536 },
  };
}
