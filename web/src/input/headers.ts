import type { Header } from '../scenario/scenarioDoc';

/** overriddenDefaults are the lowercased default names an input's own headers replace. */
export function overriddenDefaults(own: Header[], defaults: Header[]): Set<string> {
  const names = new Set(own.map(([k]) => k.toLowerCase()));
  return new Set(defaults.map(([k]) => k.toLowerCase()).filter((k) => names.has(k)));
}

/** headersChip summarizes an input's headers for its collapsed card, defaults included. */
export function headersChip(own: Header[], defaults: Header[]): string | undefined {
  const replaced = overriddenDefaults(own, defaults);
  const names = [...own.map(([k]) => k), ...defaults.filter(([k]) => !replaced.has(k.toLowerCase())).map(([k]) => k)];
  if (!names.length) return undefined;
  return names.length <= 2 ? names.join(', ') : `${names.length} headers`;
}
