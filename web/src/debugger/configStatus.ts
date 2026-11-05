import type { Graph, ReportRuntime } from '../api/types';
import { reportRuntime } from '../runtimes/runtimes';

export interface ConfigStatus {
  /** The first, toned fact: "Valid YAML", "1 error", ... */
  items: { tone: 'ok' | 'warn' | 'blocked'; text: string }[];
  /** The plain line after it. */
  detail: string;
}

const plural = (n: number, noun: string) => `${n} ${noun}${n === 1 ? '' : 's'}`;

/** topologySummary counts what a configuration declares: "3 pipelines · 8 components · 1 extension". */
export function topologySummary(g: Pick<Graph, 'pipelines' | 'components'> | undefined): string {
  if (!g) return '';
  const components = g.components ?? [];
  const extensions = components.filter((c) => c.kind === 'extension').length;
  const parts = [plural((g.pipelines ?? []).length, 'pipeline'), plural(components.length - extensions, 'component')];
  if (extensions) parts.push(plural(extensions, 'extension'));
  return parts.join(' · ');
}

/**
 * configStatus is the line under the configuration editor: the problems found
 * as the user types, or that the configuration is valid and what it declares.
 * Nothing is said for an empty configuration. A valid one also names the
 * runtime that checked it: the team's default unless one is chosen.
 */
export function configStatus(
  markers: readonly { severity: string }[],
  inspection: { valid: boolean; topology?: Graph; runtime?: ReportRuntime } | undefined,
  configText: string,
): ConfigStatus | undefined {
  if (!configText.trim()) return undefined;
  const errors = markers.filter((m) => m.severity === 'error').length;
  const warnings = markers.filter((m) => m.severity === 'warning').length;
  const items: ConfigStatus['items'] = [];
  if (errors) items.push({ tone: 'blocked', text: plural(errors, 'error') });
  if (warnings) items.push({ tone: 'warn', text: plural(warnings, 'warning') });
  if (items.length) return { items, detail: 'Checked as you type' };
  if (inspection?.valid) {
    const runtime = reportRuntime(inspection.runtime);
    const detail = [topologySummary(inspection.topology), runtime ? `checked on ${runtime.label}` : ''].filter(Boolean).join(' · ');
    return { items: [{ tone: 'ok', text: 'Valid YAML' }], detail };
  }
  return { items: [], detail: 'Checking the configuration…' };
}
