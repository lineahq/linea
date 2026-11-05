import { describe, expect, it } from 'vitest';
import { runActive } from './runState';

describe('runActive', () => {
  it('treats queued, assigned and running runs as not ended', () => {
    for (const state of ['queued', 'assigned', 'running']) expect(runActive(state)).toBe(true);
  });
  it('treats done, error, cancelled and lost runs as ended', () => {
    for (const state of ['done', 'error', 'cancelled', 'lost']) expect(runActive(state)).toBe(false);
  });
});
