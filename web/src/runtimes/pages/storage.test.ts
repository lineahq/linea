import { describe, expect, it } from 'vitest';
import type { RuntimeStorage, StoredRuntime } from '../../api/types';
import { pruneQuestion } from '../runtimes';
import { cleanupRows, cleanupScope, DEFAULT_SELECTION, diskUse, pruneRequest } from './storage';

const image = (name: string, state: string, size: number, inUse = false): StoredRuntime => ({
  name, state, size, in_use: inUse, collector_version: '0.162.0', verified: true, go_image: 'golang', image_id: `sha256:${name}${state}`,
  manifest: '', module_set: '', product_version: 'dev', protocol: 1,
});

const storage: RuntimeStorage = {
  reclaimable: 0,
  runtimes: [
    image('a', 'named', 3_000_000_000, true),
    image('a', 'replaced', 2_000_000_000),
    image('b', 'replaced', 1_000_000_000),
    image('c', 'replaced', 5_000_000_000, true),
    image('d', 'tagged', 700_000_000),
    image('e', 'stale', 400_000_000),
  ],
  caches: [{ name: 'go-mod', size: 1_100_000_000 }],
  leftovers: [{ name: 'build-1', state: 'exited', running: false }, { name: 'worker-1', state: 'running', running: true }],
};

describe('cleanupRows', () => {
  it('offers only unused images, the caches and stopped containers', () => {
    expect(cleanupRows(storage)).toEqual([
      { id: 'replaced', title: '2 unused runtime images', why: 'a rebuild took their names; no runtime points at them', frees: 3_000_000_000, count: 2, fixed: false },
      { id: 'stale', title: '1 stale runtime image', why: 'built for another worker protocol; this debugger cannot run them', frees: 400_000_000, count: 1, fixed: false },
      { id: 'caches', title: 'Build cache', why: 'rebuilt on the next build, which is slower', frees: 1_100_000_000, count: 1, fixed: false },
      { id: 'containers', title: '1 stopped build or worker container', why: 'removed with every clean-up', count: 1, fixed: true },
    ]);
  });

  it('has no rows when nothing can go', () => {
    expect(cleanupRows({ reclaimable: 0 })).toEqual([]);
    expect(cleanupRows({ reclaimable: 0, runtimes: [image('a', 'named', 1, true)], leftovers: [{ name: 'w', state: 'running', running: true }] })).toEqual([]);
  });

  it('counts an unknown size as nothing freed', () => {
    const rows = cleanupRows({ reclaimable: 0, runtimes: [image('a', 'replaced', -1)] });
    expect(rows[0].frees).toBe(0);
  });
});

describe('cleanupScope', () => {
  const rows = cleanupRows(storage);

  it('totals the ticked rows and always includes stopped containers', () => {
    expect(cleanupScope(rows, DEFAULT_SELECTION)).toEqual({ images: 2, bytes: 3_000_000_000, containers: 1, caches: 0, empty: false });
    expect(cleanupScope(rows, { replaced: true, stale: true, caches: true })).toEqual({ images: 3, bytes: 4_500_000_000, containers: 1, caches: 1, empty: false });
    expect(cleanupScope(rows, { replaced: false, stale: false, caches: false })).toEqual({ images: 0, bytes: 0, containers: 1, caches: 0, empty: false });
  });

  it('is empty when no row is left', () => {
    expect(cleanupScope([], DEFAULT_SELECTION).empty).toBe(true);
  });

  it('words the confirmation question from the scope', () => {
    expect(pruneQuestion(cleanupScope(rows, { replaced: true, stale: false, caches: true }))).toBe(
      'Remove 2 runtime images, 1 build cache and 1 stopped container (4.1 GB)? Runs keep their evidence.',
    );
  });
});

describe('pruneRequest', () => {
  it('sends the selection', () => {
    expect(pruneRequest(DEFAULT_SELECTION)).toEqual({ replaced: true, stale: false, caches: false });
    expect(pruneRequest({ replaced: false, stale: true, caches: true })).toEqual({ replaced: false, stale: true, caches: true });
  });
});

describe('diskUse', () => {
  it('splits what the runtimes use into images and caches', () => {
    const d = diskUse(storage);
    expect(d.total).toBe(13_200_000_000);
    expect(d.segments.map((s) => [s.id, s.bytes, s.percent])).toEqual([['images', 12_100_000_000, 91.7], ['caches', 1_100_000_000, 8.3]]);
    expect(d.summary).toBe('Runtime images 12.1 GB, Build caches 1.1 GB');
  });

  it('has no share when nothing is stored', () => {
    expect(diskUse({ reclaimable: 0 })).toMatchObject({ total: 0, segments: [{ percent: 0 }, { percent: 0 }] });
  });
});
