import type { UpgradeCheck } from '../../api/types';
import type { IconName } from '../../ui/Icon';
import { diagnosticRows, gateState, scenarioRows, verdictText } from '../upgrade';
import type { CheckSubject } from './upgradeMachine';

/**
 * The upgrade check result worded for the page: the four facets of the
 * verdict, the summary callout, the feature gate and diagnostic rows and the
 * links to the runs that were compared. Built on `../upgrade.ts`; pure.
 */

export type FacetTone = 'pass' | 'warn' | 'fail' | 'neutral';

export interface Facet {
  name: string;
  tone: FacetTone;
  icon: IconName;
  value: string;
  detail: string;
}

const ICON: Record<FacetTone, IconName> = { pass: 'circle-check', warn: 'triangle-alert', fail: 'circle-x', neutral: 'circle-minus' };

const facet = (name: string, tone: FacetTone, value: string, detail: string): Facet => ({ name, tone, icon: ICON[tone], value, detail });

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

/** upgradeFacets states what the check found in four parts: configuration, behavior, feature gates and certainty. */
export function upgradeFacets(c: UpgradeCheck): Facet[] {
  const cfg = c.configuration;
  const added = cfg.added?.length ?? 0;
  let configuration: Facet;
  if (cfg.from_valid && cfg.to_valid) {
    configuration = facet('Configuration', 'pass', 'Valid on both', added ? `${plural(added, 'new diagnostic')} on the target` : 'same diagnostics on both');
  } else if (cfg.from_valid) {
    configuration = facet('Configuration', 'fail', 'Invalid on the target', 'valid on the baseline');
  } else if (cfg.to_valid) {
    configuration = facet('Configuration', 'warn', 'Invalid on the baseline', 'valid on the target');
  } else {
    configuration = facet('Configuration', 'fail', 'Invalid on both', 'neither runtime accepts it');
  }

  const rows = scenarioRows(c);
  const different = rows.filter((r) => r.cls === 'bad').length;
  const inconclusive = rows.filter((r) => r.cls === 'warn').length;
  let behavior: Facet;
  if (rows.length === 0) behavior = facet('Behavior', 'neutral', 'Not compared', 'no scenario is open');
  else if (different) behavior = facet('Behavior', 'fail', `${different} different`, `of ${plural(rows.length, 'scenario')}`);
  else if (inconclusive) behavior = facet('Behavior', 'warn', 'Inconclusive', `${inconclusive} of ${plural(rows.length, 'scenario')}`);
  else behavior = facet('Behavior', 'pass', 'Same behavior', plural(rows.length, 'scenario'));

  const gates = c.feature_gates.changes ?? [];
  const unknown = c.feature_gates.unknown ?? [];
  let gateFacet: Facet;
  if (unknown.length) gateFacet = facet('Feature gates', 'neutral', 'Not compared', `the ${unknown.join(' and ')} runtime lists no gates`);
  else if (gates.length) gateFacet = facet('Feature gates', 'warn', `${gates.length} changed`, gates[0].id + (gates.length > 1 ? ` and ${gates.length - 1} more` : ''));
  else gateFacet = facet('Feature gates', 'pass', 'No change', 'none of the components\' gates differ');

  const reasons = c.reasons ?? [];
  let certainty: Facet;
  if (reasons.length) certainty = facet('Certainty', 'warn', 'Partial', reasons[0].message);
  else if (c.verdict === 'inconclusive' || inconclusive) certainty = facet('Certainty', 'warn', 'Partial', inconclusive ? `${plural(inconclusive, 'scenario')} inconclusive` : 'the check could not decide');
  else certainty = facet('Certainty', 'pass', 'Complete', 'every part was compared');

  return [configuration, behavior, gateFacet, certainty];
}

export interface Summary {
  tone: 'pass' | 'fail' | 'warn';
  title: string;
  body: string;
}

/**
 * summaryOf states the platform's verdict as the callout's title and body.
 * Inconclusive is a warning, never worded as a pass.
 */
export function summaryOf(c: UpgradeCheck): Summary {
  const v = verdictText(c);
  const colon = v.text.indexOf(': ');
  const tone = v.cls === 'good' ? 'pass' : v.cls === 'bad' ? 'fail' : 'warn';
  if (colon < 0) return { tone, title: v.text, body: '' };
  const body = v.text.slice(colon + 2);
  return { tone, title: v.text.slice(0, colon), body: body.charAt(0).toUpperCase() + body.slice(1) + '.' };
}

export interface DiffRow {
  kind: 'changed' | 'added' | 'removed';
  path: string;
  before: string;
  after: string;
  by: string;
}

/**
 * diffRows lists the feature gate changes, the diagnostics the target adds or
 * no longer reports, and the reasons the check is uncertain, in that order.
 */
export function diffRows(c: UpgradeCheck): DiffRow[] {
  const rows: DiffRow[] = [];
  for (const g of c.feature_gates.changes ?? []) {
    rows.push({ kind: 'changed', path: g.id, before: gateState(g.from), after: gateState(g.to), by: `feature gate${g.component ? ` · ${g.component}` : ''}` });
  }
  for (const d of diagnosticRows(c)) {
    const path = `${d.code}${d.component ? ` · ${d.component}` : ''}`;
    rows.push(d.change === 'new'
      ? { kind: 'added', path, before: '', after: d.message, by: `${d.severity} · new on the target` }
      : { kind: 'removed', path, before: d.message, after: '', by: `${d.severity} · no longer reported` });
  }
  for (const r of c.reasons ?? []) rows.push({ kind: 'changed', path: r.code, before: '', after: r.message, by: 'uncertainty' });
  return rows;
}

/** CompareSearch is the debugger search that opens two runs in Compare runs. */
export interface CompareSearch {
  run: string;
  compare: string;
  tab: 'compare';
}

/** compareSearch is the Compare runs location of a scenario's before and after runs. */
export function compareSearch(runs: string[]): CompareSearch | undefined {
  if (runs.length !== 2) return undefined;
  return { run: runs[0], compare: runs[1], tab: 'compare' };
}

/**
 * firstDifference is the first scenario worth opening: one that behaves
 * differently, else an inconclusive one, with the runs to compare.
 */
export function firstDifference(c: UpgradeCheck): { name: string; search: CompareSearch } | undefined {
  const rows = scenarioRows(c);
  for (const cls of ['bad', 'warn'] as const) {
    for (const r of rows) {
      const search = r.cls === cls ? compareSearch(r.runs) : undefined;
      if (search) return { name: r.name, search };
    }
  }
  return undefined;
}

/** checkedLine words what a check was started on: the draft and how many scenarios. */
export function checkedLine(s: CheckSubject): { config: string; scenarios: string } {
  return {
    config: s.configLabel || 'the open configuration',
    scenarios: s.scenarios === 0 ? 'without a scenario' : `with ${plural(s.scenarios, 'scenario')}`,
  };
}

/** sideLine words one runtime of a check: verified or not. */
export function sideLine(r: { verified: boolean }): string {
  return r.verified ? 'verified release' : 'unverified release';
}
