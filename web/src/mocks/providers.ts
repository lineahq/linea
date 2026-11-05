import { stringify } from 'yaml';
import type { Diagnostic, ReportProvider } from '../api/types';
import type { ProviderModel } from '../scenario/scenarioDoc';

/**
 * Module providers in the Mocks panel: the settings a scenario
 * hands each provider under mocks.providers.<name>, checked against the
 * providers compiled into the selected runtime. The settings are the
 * provider's own, so the panel edits them as YAML and never interprets them.
 */

export interface ProviderCard {
  name: string;
  settings: string;
  /** The package the runtime compiled the provider from, when it has it. */
  package?: string;
  /** Why runs on the selected runtime refuse these settings. */
  problem?: string;
}

/**
 * providerCards pairs the scenario's provider settings with the selected
 * runtime's providers (undefined while unknown, which claims nothing).
 */
export function providerCards(settings: ProviderModel[], available: ReportProvider[] | undefined, runtime: string): ProviderCard[] {
  return settings.map((s) => {
    const p = available?.find((a) => a.name === s.name);
    if (p || !available) return { ...s, package: p?.package };
    const which = runtime ? `the runtime ${runtime}` : 'the built-in runtime';
    const has = available.length ? `it has ${available.map((a) => a.name).join(', ')}` : 'it has no providers';
    return { ...s, problem: `Runs on ${which} fail: ${has}. Choose a runtime built with ${s.name}, or remove these settings.` };
  });
}

/** providersToAdd are the selected runtime's providers the scenario has no settings for yet. */
export function providersToAdd(available: ReportProvider[] | undefined, settings: ProviderModel[]): ReportProvider[] {
  return (available ?? []).filter((p) => !settings.some((s) => s.name === p.name));
}

/**
 * proposals are the settings the providers' checks propose, per provider
 * (provider_settings on their findings); a key an earlier finding proposed
 * wins.
 */
export function proposals(findings: Diagnostic[]): Map<string, Record<string, unknown>> {
  const out = new Map<string, Record<string, unknown>>();
  for (const f of findings) {
    const p = f.provider_settings;
    if (!p || typeof p.settings !== 'object' || p.settings === null || Array.isArray(p.settings)) continue;
    out.set(p.provider, { ...(p.settings as Record<string, unknown>), ...(out.get(p.provider) ?? {}) });
  }
  return out;
}

/**
 * providerGaps are the missing-fixture findings not about files or
 * Kubernetes readers: those a provider's own check reports, such as
 * livecapture's "the scenario declares no capture session".
 */
export function providerGaps(diagnostics: Diagnostic[], builtInReaders: string[]): Diagnostic[] {
  return diagnostics.filter((d) => d.code === 'mock_fixture_missing' && d.component && !builtInReaders.includes(d.component));
}

/** proposalText is a proposal as the YAML it adds, for a tooltip. */
export function proposalText(settings: unknown): string {
  return stringify(settings ?? {}).trimEnd();
}
