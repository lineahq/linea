import type { Diagnostic, RetargetedManifest, Runtime, UpgradeCheck, UpgradeCheckRequest, UpgradeGateState } from '../api/types';
import type { WorkspaceState } from '../workspace/store';
import { BUILT_IN, olderImage } from './runtimes';

/**
 * The upgrade check view model: which runtimes a check can compare,
 * the request from the workspace drafts, and the check's verdict, diagnostics,
 * feature gates and scenarios worded for the view. The check's sides are not
 * a runtime selection: runs keep the runtime chosen beside Run.
 */

export interface UpgradeSide {
  value: string;
  label: string;
  unverified: boolean;
}

/** upgradeSides lists the built-in runtime and the built ones, oldest release first. */
export function upgradeSides(runtimes: Runtime[] | undefined, builtInVersion?: string): UpgradeSide[] {
  const all = runtimes ?? [];
  const built = all.filter((r) => !olderImage(r, all)).sort((a, b) =>
    compareVersions(a.collector_version, b.collector_version) || a.name.localeCompare(b.name));
  return [
    { value: BUILT_IN, label: `Built-in${builtInVersion ? ` · Collector ${builtInVersion}` : ''}`, unverified: false },
    ...built.map((r) => ({ value: r.name, label: `${r.name} · Collector ${r.collector_version}${r.verified ? '' : ' · unverified'}`, unverified: !r.verified })),
  ];
}

/** compareVersions orders dotted releases numerically ("0.99.0" before "0.160.0"). */
export function compareVersions(a: string, b: string): number {
  const pa = a.split('.').map(Number);
  const pb = b.split('.').map(Number);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const d = (pa[i] || 0) - (pb[i] || 0);
    if (d) return d;
  }
  return 0;
}

/**
 * upgradeRequest checks the configuration draft, with the scenario draft
 * when there is one, from one runtime to another. The empty `to` is the
 * built-in runtime, which the request must name.
 */
export function upgradeRequest(ws: Pick<WorkspaceState, 'configYAML' | 'configPath' | 'scenarioYAML' | 'scenarioPath'>, from: string, to: string): UpgradeCheckRequest {
  const req: UpgradeCheckRequest = { to: to || 'built-in', config_yaml: ws.configYAML };
  if (from) req.from = from;
  if (ws.configPath) req.config_label = ws.configPath;
  if (ws.scenarioYAML.trim()) {
    req.scenarios = [{
      scenario_yaml: ws.scenarioYAML,
      ...(ws.scenarioPath ? { scenario_label: ws.scenarioPath } : {}),
    }];
  }
  return req;
}

/** upgradeProblem says why a check cannot start, or undefined. */
export function upgradeProblem(configYAML: string, from: string, to: string): string | undefined {
  if (!configYAML.trim()) return 'Write or open a configuration first.';
  if (from === to) return 'Choose two different runtimes.';
  return undefined;
}

export interface VerdictText {
  text: string;
  cls: 'good' | 'bad' | 'warn';
}

/** verdictText states the verdict; inconclusive is never worded as a pass. */
export function verdictText(c: UpgradeCheck): VerdictText {
  switch (c.verdict) {
    case 'compatible':
      return c.scenarios?.length
        ? { text: 'Compatible: the configuration loads and every scenario behaves the same', cls: 'good' }
        : { text: 'Compatible: the configuration loads (no scenario compared behavior)', cls: 'good' };
    case 'configuration_breaks':
      return { text: 'Configuration breaks: the target runtime refuses the configuration', cls: 'bad' };
    case 'behavior_changes':
      return { text: 'Behavior changes: a scenario behaves differently on the target runtime', cls: 'bad' };
    case 'inconclusive':
      return { text: 'Inconclusive: the check could not decide (not a pass)', cls: 'warn' };
  }
  return { text: c.verdict, cls: 'warn' };
}

export interface DiagnosticRow {
  change: 'new' | 'gone';
  severity: string;
  code: string;
  component: string;
  message: string;
}

/** diagnosticRows lists the target's new diagnostics first, then those it no longer reports. */
export function diagnosticRows(c: UpgradeCheck): DiagnosticRow[] {
  const row = (change: DiagnosticRow['change']) => (d: Diagnostic): DiagnosticRow =>
    ({ change, severity: d.severity, code: d.code, component: d.component ?? '', message: d.message });
  return [...(c.configuration.added ?? []).map(row('new')), ...(c.configuration.removed ?? []).map(row('gone'))];
}

/** gateState words one side of a feature gate. */
export function gateState(g: UpgradeGateState | undefined): string {
  if (!g) return 'absent';
  return `${g.stage} · ${g.enabled ? 'on' : 'off'}`;
}

export interface ScenarioRow {
  name: string;
  result: string;
  cls: 'good' | 'bad' | 'warn';
  assertions: string;
  inputs: string;
  runs: string[];
}

/** scenarioRows states each scenario's result; a scenario that was not compared says why. */
export function scenarioRows(c: UpgradeCheck): ScenarioRow[] {
  return (c.scenarios ?? []).map((s) => {
    const runs = [s.before, s.after].filter((r): r is string => !!r);
    if (s.error) return { name: s.name, result: `Not compared: ${s.error}`, cls: 'warn', assertions: '', inputs: '', runs };
    const changed = s.outcome === 'different' || (s.before_assertions ?? '') !== (s.after_assertions ?? '');
    return {
      name: s.name,
      result: s.outcome === 'same' && !changed ? 'Same' : s.outcome === 'inconclusive' ? 'Inconclusive' : 'Different',
      cls: s.outcome === 'inconclusive' ? 'warn' : changed ? 'bad' : 'good',
      assertions: s.before_assertions || s.after_assertions ? `${s.before_assertions || 'none'} → ${s.after_assertions || 'none'}` : 'none',
      inputs: s.inputs_identical ? 'identical' : 'not proven identical',
      runs,
    };
  });
}

/** retargetNotes words what moving a manifest left for the user to do. */
export function retargetNotes(m: RetargetedManifest): { text: string; warn: boolean }[] {
  const out = [{ text: `Moved from ${m.from || 'an unknown release'} to ${m.to}: ${m.moved?.length ?? 0} module versions changed.`, warn: false }];
  for (const mod of m.missing ?? []) out.push({ text: `${mod} is not in ${m.to}; replace or remove it before building.`, warn: true });
  for (const mod of m.custom ?? []) out.push({ text: `Custom module ${mod} keeps its version; check that it supports ${m.to}.`, warn: true });
  for (const n of m.notes ?? []) out.push({ text: n, warn: false });
  return out;
}
