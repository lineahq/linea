/** Small display formatters shared by the Live capture screens. All are pure and locale-independent. */

/** formatCount writes an integer with thousands separators, e.g. 1,204. */
export function formatCount(n: number): string {
  return new Intl.NumberFormat('en-US').format(Math.round(n));
}

/** formatBytes writes a byte count in KiB, MiB or GiB (base 1024), keeping one decimal below 100. */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB'];
  let value = bytes;
  let i = 0;
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024;
    i++;
  }
  const text = i === 0 || value >= 100 ? String(Math.round(value)) : (Math.round(value * 10) / 10).toString();
  return `${text} ${units[i]}`;
}

/** relativeTime says how long ago an instant was: "now", "4 s ago", "2 min ago", "3 h ago", "5 d ago". */
export function relativeTime(iso: string | undefined, now: number): string {
  const t = iso ? Date.parse(iso) : NaN;
  if (Number.isNaN(t)) return '—';
  const s = Math.max(0, Math.round((now - t) / 1000));
  if (s < 3) return 'now';
  if (s < 60) return `${s} s ago`;
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86400)} d ago`;
}

/** clockTime writes the local HH:MM:SS of an instant. */
export function clockTime(iso: string | undefined): string {
  const t = iso ? new Date(iso) : undefined;
  if (!t || Number.isNaN(t.getTime())) return '';
  return [t.getHours(), t.getMinutes(), t.getSeconds()].map((n) => String(n).padStart(2, '0')).join(':');
}

/** shortId keeps the tail of a long identifier, which is the part that differs. */
export function shortId(id: string, keep = 8): string {
  return id.length <= keep ? id : id.slice(-keep);
}

