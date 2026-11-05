import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../api/client';
import type { CaptureRecordsPage, CaptureSession } from '../api/types';
import { scanRecord, type ScannedRecord } from './recordView';

type Item = NonNullable<CaptureRecordsPage['items']>[number];

/** The records endpoint's largest page. */
const SCAN_PAGE = 100;
/** A capture holds at most 10,000 records by default; this bounds a runaway cursor. */
const MAX_SCAN_PAGES = 250;

export interface CaptureScan {
  records: ScannedRecord[];
  /** The received records, by sequence, so an opened row has its payload. */
  items: Map<number, Item>;
  watermark: number;
}

/**
 * readWholeCapture pages through every record a capture holds, 100 at a
 * time, so filters, "select all" and samples cover the whole capture rather
 * than the page on screen.
 */
export async function readWholeCapture(
  id: string,
  read: typeof api.captureRecords,
  signal?: AbortSignal,
  onProgress?: (loaded: number) => void,
): Promise<CaptureScan> {
  const records: ScannedRecord[] = [];
  const items = new Map<number, Item>();
  let cursor = '';
  let watermark = 0;
  for (let page = 0; page < MAX_SCAN_PAGES; page++) {
    const next = await read(id, cursor, undefined, SCAN_PAGE, signal);
    watermark = Math.max(watermark, next.watermark ?? 0);
    for (const item of next.items ?? []) {
      records.push(scanRecord(item));
      items.set(item.sequence, item);
    }
    onProgress?.(records.length);
    if (!next.next_cursor) break;
    cursor = next.next_cursor;
  }
  return { records, items, watermark };
}

/**
 * useCaptureScan reads the whole capture once it is `wanted`. A finished
 * capture is read once; a live one again after `reset` (the Refresh button).
 */
export function useCaptureScan(session: CaptureSession, wanted: boolean) {
  const [loaded, setLoaded] = useState(0);
  const query = useQuery({
    queryKey: ['capture-scan', session.id, session.terminal],
    queryFn: ({ signal }) => readWholeCapture(session.id, api.captureRecords, signal, setLoaded),
    enabled: wanted && session.data.enabled,
    staleTime: Infinity,
  });
  return { ...query, loaded };
}
