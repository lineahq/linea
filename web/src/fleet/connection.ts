import type { FleetCheck, FleetConnection } from '../api/types';
import type { Chip } from './fleetView';

/** CHECK_TITLE names each step of a connection check. */
const CHECK_TITLE: Record<string, string> = {
  fleet: 'Fleet server',
  live_capture: 'Live capture plugin',
  config_management: 'Config management',
};

/** checkTitle names a step; an unknown one keeps fleet's name. */
export function checkTitle(name: string): string {
  return CHECK_TITLE[name] ?? name;
}

/** checkChip words a step's status. */
export function checkChip(c: Pick<FleetCheck, 'status'>): Chip {
  switch (c.status) {
    case 'ok': return { tone: 'pass', icon: 'circle-check', label: 'Working' };
    case 'failed': return { tone: 'fail', icon: 'circle-x', label: 'Failed' };
    case 'off': return { tone: 'plain', icon: 'circle-minus', label: 'Off' };
    case 'skipped': return { tone: 'plain', icon: 'circle-dashed', label: 'Not checked' };
    default: return { tone: 'plain', icon: 'circle-help', label: c.status };
  }
}

export interface ConnectionVerdict {
  tone: 'pass' | 'warn' | 'fail';
  title: string;
  body: string;
}

/**
 * connectionVerdict sums up a check: fleet unreachable or refusing the
 * credential fails it; a missing livecapture plugin only limits it.
 */
export function connectionVerdict(c: Pick<FleetConnection, 'configured' | 'checks'>): ConnectionVerdict | undefined {
  if (!c.configured) return undefined;
  const step = (name: string) => (c.checks ?? []).find((s) => s.name === name);
  const fleet = step('fleet');
  if (!fleet || fleet.status !== 'ok') {
    return { tone: 'fail', title: 'Fleet is not reachable', body: fleet?.message ?? 'Fleet did not answer.' };
  }
  const capture = step('live_capture');
  if (capture && capture.status !== 'ok') {
    return { tone: 'warn', title: 'Fleet is connected; live capture is not', body: capture.message };
  }
  return { tone: 'pass', title: 'Fleet is connected', body: 'Agents, groups, configs and live capture work through this connection.' };
}

/** durationText shows a step's duration. */
export function durationText(ms: number): string {
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`;
}
