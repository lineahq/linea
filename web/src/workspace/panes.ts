export const DEFAULT_PANES = { left: 0.34, upper: 0.62 };
export const PANE_STORAGE_KEY = 'linea.panes.v1';

/** Keep both panes usable, including on viewports smaller than their ideal minima. */
export function splitBounds(size: number, before: number, after: number) {
  const available = Math.max(0, size - 8);
  const scale = Math.min(1, available / (before + after));
  return { min: available ? before * scale / available : 0.5, max: available ? 1 - after * scale / available : 0.5 };
}

export function clampSplit(ratio: number, size: number, before: number, after: number) {
  const { min, max } = splitBounds(size, before, after);
  return Math.max(min, Math.min(max, ratio));
}

export function readPanes(raw: string | null, defaults = DEFAULT_PANES): typeof DEFAULT_PANES {
  try {
    const value: unknown = JSON.parse(raw ?? 'null');
    if (value && typeof value === 'object' && 'left' in value && 'upper' in value &&
      typeof value.left === 'number' && Number.isFinite(value.left) && value.left > 0 && value.left < 1 &&
      typeof value.upper === 'number' && Number.isFinite(value.upper) && value.upper > 0 && value.upper < 1) {
      return { left: value.left, upper: value.upper };
    }
  } catch { /* An old or unavailable preference must never block the workspace. */ }
  return { ...defaults };
}
