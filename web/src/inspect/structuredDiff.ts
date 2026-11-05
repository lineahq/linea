import { diffChars } from 'diff';
import type { Change } from '../api/types';
import { attributeKey, type Attribute } from './grouped';
import { decodeValue } from './signals';

export interface DiffToken { text: string; changed: boolean }
export interface DiffValue { key: string; text: string; type?: string }
export interface DiffLine {
  kind: 'field' | 'section' | 'context';
  depth: number;
  before?: DiffValue;
  after?: DiffValue;
  operation?: string;
  confidence?: string;
  /** Only record section headings carry contextual navigation. */
  record?: string;
  side?: 'before' | 'after';
}

/** JSON-style values distinguish an empty string, a missing value and a type change. */
export function diffValue(value: unknown, type?: string): string {
  if (value === undefined) return 'not recorded';
  if (type && /^(int|double|bool)/i.test(type)) return String(value);
  return JSON.stringify(value) ?? 'not recorded';
}

/** Format OTLP values without making an int64 string look like a string attribute. */
export function otlpValue(value: unknown): string {
  const v = value && typeof value === 'object' ? value as Record<string, unknown> : {};
  if ('intValue' in v) return String(v.intValue);
  return diffValue(decodeValue(value));
}

export function fieldLines(changes: Change[], unchanged: Attribute[], depth: number, original?: unknown): DiffLine[] {
  const attrs = original && typeof original === 'object' ? (original as Record<string, unknown>).attributes : undefined;
  const raw = new Map((Array.isArray(attrs) ? attrs : []).map((a) => [a.key, a.value]));
  const context: DiffLine[] = unchanged.slice(0, 4).map((a) => {
    const value = { key: a.key, text: raw.has(a.key) ? otlpValue(raw.get(a.key)) : JSON.stringify(a.value) };
    return { kind: 'field', depth, before: value, after: value };
  });
  if (unchanged.length > 4) context.push(sectionLine(`… ${unchanged.length - 4} unchanged attributes`, depth, 'context'));
  return [...context, ...changes.map((c): DiffLine => {
    const key = attributeKey(c.path);
    const name = key === undefined ? c.path : key + c.path.slice(`attributes[${JSON.stringify(key)}]`.length);
    return { kind: 'field', depth, operation: c.op, confidence: c.confidence,
      before: c.op === 'added' ? undefined : { key: name, text: diffValue(c.before, c.before_type), type: c.op === 'type_changed' ? c.before_type : undefined },
      after: c.op === 'removed' ? undefined : { key: name, text: diffValue(c.after, c.after_type), type: c.op === 'type_changed' ? c.after_type : undefined },
    };
  })];
}

export function sectionLine(label: string, depth: number, kind: DiffLine['kind'] = 'section'): DiffLine {
  const value = { key: label, text: '' };
  return { kind, depth, before: value, after: value };
}

/** Bounded character diff: a very large telemetry value cannot monopolize the UI. */
export function inlineDiff(before: string, after: string): { before: DiffToken[]; after: DiffToken[] } {
  if (before === after) return { before: [{ text: before, changed: false }], after: [{ text: after, changed: false }] };
  const chunks = before.length + after.length <= 4000 ? diffChars(before, after, { timeout: 10, maxEditLength: 512 }) : undefined;
  if (!chunks) return { before: [{ text: before, changed: true }], after: [{ text: after, changed: true }] };
  // Unrelated values (for example a name replaced by its hash) should read as
  // whole replacements, not as a mosaic of coincidental matching letters.
  const common = chunks.filter((c) => !c.added && !c.removed).reduce((n, c) => n + c.value.length, 0);
  if (common / Math.max(before.length, after.length) < 0.25) return {
    before: [{ text: before, changed: true }], after: [{ text: after, changed: true }],
  };
  return {
    before: chunks.filter((c) => !c.added).map((c) => ({ text: c.value, changed: !!c.removed })),
    after: chunks.filter((c) => !c.removed).map((c) => ({ text: c.value, changed: !!c.added })),
  };
}
