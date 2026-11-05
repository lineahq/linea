import type { CollectorGroup, CollectorGroupSave, FleetAgent, FleetEvent, FleetRemoteConfigInfo, FleetRollout, FleetRolloutPolicy } from '../api/types';
import type { PhaseTone } from '../capture/phase';
import type { IconName } from '../ui/Icon';

/** Chip is a status chip's tone, icon and words. */
export interface Chip {
  tone: PhaseTone;
  icon: IconName;
  label: string;
}

/** shortHex shows the head of a fleet ID; fleet IDs are 32 hex characters. */
export function shortHex(id: string): string {
  return id.length > 12 ? id.slice(0, 8) : id;
}

/** agentName names an agent by its service, then its host, then its ID. */
export function agentName(a: Pick<FleetAgent, 'id' | 'attributes'>): string {
  const at = a.attributes ?? {};
  return at['service.name'] || at['host.name'] || at['k8s.pod.name'] || shortHex(a.id);
}

/** agentPlace is where an agent runs: namespace, pod or host. */
export function agentPlace(a: Pick<FleetAgent, 'attributes'>): string {
  const at = a.attributes ?? {};
  return [at['k8s.namespace.name'], at['k8s.pod.name'] || at['host.name']].filter(Boolean).join(' / ');
}

/** healthChip words an agent's connection and last reported health. */
export function healthChip(a: Pick<FleetAgent, 'connected' | 'health'>): Chip {
  if (!a.connected) return { tone: 'plain', icon: 'circle-minus', label: 'Offline' };
  if (!a.health) return { tone: 'plain', icon: 'circle-dashed', label: 'No health' };
  if (a.health.healthy) return { tone: 'pass', icon: 'circle-check', label: 'Healthy' };
  return { tone: 'fail', icon: 'circle-x', label: a.health.components_unhealthy ? `Unhealthy · ${a.health.components_unhealthy}` : 'Unhealthy' };
}

/** configChip words the remote config state an agent reported. */
export function configChip(rc: FleetRemoteConfigInfo | undefined | null, heldBack = false): Chip {
  if (heldBack || rc?.held_hash) return { tone: 'fail', icon: 'circle-pause', label: 'Held back' };
  switch (rc?.status) {
    case 'applied': return { tone: 'pass', icon: 'circle-check', label: 'Applied' };
    case 'applying': return { tone: 'running', icon: 'loader-circle', label: 'Applying' };
    case 'failed': return { tone: 'fail', icon: 'circle-x', label: 'Failed' };
    default: return { tone: 'plain', icon: 'circle-minus', label: 'Not managed' };
  }
}

const FLEET_ID = /^[0-9a-f]{32}$/;

/** isFleetName is fleet's rule for config and assignment names. */
export function isFleetName(name: string): boolean {
  return /^[a-z0-9][a-z0-9-]{0,62}$/.test(name);
}

/** parseAgentIDs reads agent IDs separated by spaces, commas or lines. */
export function parseAgentIDs(text: string): { ids: string[]; invalid: string[] } {
  const all = [...new Set(text.split(/[\s,]+/).map((s) => s.trim().toLowerCase()).filter(Boolean))];
  return { ids: all.filter((s) => FLEET_ID.test(s)), invalid: all.filter((s) => !FLEET_ID.test(s)) };
}

/** Suggestion is one autocomplete option: the whole field value and a hint. */
export interface Suggestion {
  value: string;
  label: string;
}

/**
 * selectorTail splits a selector at the term being typed. head keeps the
 * finished terms as typed, used names their keys, and valueKey is the key
 * whose value is being typed (after `=`). Commas inside `in (…)` and quoted
 * values do not end a term.
 */
export function selectorTail(selector: string): { head: string; term: string; used: string[]; valueKey?: string } {
  const cuts: number[] = [];
  let depth = 0, quoted = false;
  for (let i = 0; i < selector.length; i++) {
    const c = selector[i];
    if (quoted) {
      if (c === '\\') i++;
      else if (c === '"') quoted = false;
    } else if (c === '"') quoted = true;
    else if (c === '(') depth++;
    else if (c === ')') depth = Math.max(0, depth - 1);
    else if (c === ',' && depth === 0) cuts.push(i);
  }
  const at = cuts.length ? cuts[cuts.length - 1] + 1 : 0;
  const used = cuts.map((end, n) => selector.slice(n ? cuts[n - 1] + 1 : 0, end).trim().split(/[=\s(]/)[0]).filter(Boolean);
  const term = selector.slice(at);
  const eq = term.indexOf('=');
  const key = eq >= 0 ? term.slice(0, eq).trim() : '';
  return { head: selector.slice(0, at), term, used, valueKey: key && !/[\s(]/.test(key) ? key : undefined };
}

/** quoteSelectorValue writes a value as fleet's selector grammar needs it. */
export function quoteSelectorValue(v: string): string {
  return /[,()"=]/.test(v) || v.trim() !== v ? JSON.stringify(v) : v;
}

/**
 * selectorSuggestions completes the term being typed: attribute keys not
 * used yet, or, after `key=`, the values fleet counts for that key with
 * their agents. Each suggestion is the whole selector, as a datalist needs.
 */
export function selectorSuggestions(
  selector: string,
  keys: string[],
  values?: { key: string; groups: { value: string; total: number; connected: number }[] },
  max = 50,
): Suggestion[] {
  const { head, term, used, valueKey } = selectorTail(selector);
  const typed = term.trimStart();
  const lead = term.slice(0, term.length - typed.length);
  if (valueKey) {
    if (values?.key !== valueKey) return [];
    const want = typed.slice(typed.indexOf('=') + 1).trim().replace(/^"/, '').toLowerCase();
    return values.groups
      .filter((g) => g.value !== '' && g.value.toLowerCase().startsWith(want))
      .slice(0, max)
      .map((g) => ({ value: `${head}${lead}${valueKey}=${quoteSelectorValue(g.value)}`, label: `${g.total} agent${g.total === 1 ? '' : 's'}, ${g.connected} connected` }));
  }
  if (/[\s(="]/.test(typed.trimEnd())) return [];
  const want = typed.trim().toLowerCase();
  return keys
    .filter((k) => !used.includes(k) && k.toLowerCase().includes(want))
    .sort((a, b) => Number(!a.toLowerCase().startsWith(want)) - Number(!b.toLowerCase().startsWith(want)) || a.localeCompare(b))
    .slice(0, max)
    .map((k) => ({ value: `${head}${lead}${k}=`, label: 'attribute' }));
}

/** attributeKeys lists the attribute keys the agents report, sorted. */
export function attributeKeys(agents: Pick<FleetAgent, 'attributes'>[]): string[] {
  return [...new Set(agents.flatMap((a) => Object.keys(a.attributes ?? {})))].sort();
}

/** agentSuggestions offers the agents not chosen yet, by ID, named for the picker. */
export function agentSuggestions(agents: Pick<FleetAgent, 'id' | 'attributes' | 'connected'>[], chosen: string[]): Suggestion[] {
  return agents
    .filter((a) => !chosen.includes(a.id))
    .map((a) => ({ value: a.id, label: [agentName(a), agentPlace(a), a.connected ? 'connected' : 'offline'].filter(Boolean).join(' · ') }));
}

/** GroupDraft is the group editor's form. */
export interface GroupDraft {
  name: string;
  kind: 'selector' | 'static';
  selector: string;
  agents: string;
}

export function groupDraft(g?: Pick<CollectorGroup, 'name' | 'kind' | 'selector' | 'agent_ids'>): GroupDraft {
  if (!g) return { name: '', kind: 'selector', selector: '', agents: '' };
  return { name: g.name, kind: g.kind === 'static' ? 'static' : 'selector', selector: g.selector ?? '', agents: (g.agent_ids ?? []).join('\n') };
}

/** groupSave turns the form into fleet's request, or says what to fix. */
export function groupSave(d: GroupDraft, current?: Pick<CollectorGroup, 'id' | 'revision'>): CollectorGroupSave | string {
  const name = d.name.trim();
  if (!name) return 'Name the group.';
  const base = { id: current?.id, revision: current?.revision ?? 0, name };
  if (d.kind === 'selector') {
    const selector = d.selector.trim();
    if (!selector) return 'Write a selector, such as service.name=checkout.';
    return { ...base, selector };
  }
  const { ids, invalid } = parseAgentIDs(d.agents);
  if (invalid.length) return `Not an agent ID: ${invalid[0]}. Agent IDs are 32 hex characters.`;
  return { ...base, agent_ids: ids };
}

/** RolloutForm is the rollout policy as the forms edit it. */
export interface RolloutForm {
  /** Waves such as "1, 25%, 100%": a count of agents or a percent of the targets. */
  waves: string;
  canary: string;
  bake: string;
  maxFailures: string;
  onFailure: '' | 'pause' | 'rollback';
}

export const DEFAULT_ROLLOUT: RolloutForm = { waves: '100%', canary: '', bake: '', maxFailures: '', onFailure: '' };

const whole = (s: string) => /^\d+$/.test(s.trim());

/** rolloutPolicy reads the form into fleet's policy, or says what to fix. Empty fields keep fleet's defaults. */
export function rolloutPolicy(f: RolloutForm): FleetRolloutPolicy | string {
  const policy: FleetRolloutPolicy = {};
  const tokens = f.waves.split(/[\s,]+/).filter(Boolean);
  if (tokens.length) {
    policy.waves = [];
    for (const t of tokens) {
      const m = /^(\d+)(%?)$/.exec(t);
      if (!m) return `Wave "${t}" is not a count of agents or a percent.`;
      const n = Number(m[1]);
      if (m[2]) {
        if (n < 1 || n > 100) return `Wave ${t} is not between 1% and 100%.`;
        policy.waves.push({ percent: n });
      } else {
        if (n < 1) return 'A wave releases at least one agent.';
        policy.waves.push({ count: n });
      }
    }
  }
  if (f.canary.trim()) {
    if (!FLEET_ID.test(f.canary.trim())) return 'The canary group is a group ID.';
    policy.canary_group_id = f.canary.trim();
  }
  if (f.bake.trim()) {
    if (!whole(f.bake)) return 'Bake time is whole seconds.';
    policy.bake_seconds = Number(f.bake);
  }
  if (f.maxFailures.trim()) {
    const m = /^(\d+)(%?)$/.exec(f.maxFailures.trim());
    if (!m) return 'Failure budget is a count or a percent.';
    if (m[2]) policy.max_failure_percent = Number(m[1]);
    else policy.max_failures = Number(m[1]);
  }
  if (f.onFailure) policy.on_failure = f.onFailure;
  return policy;
}

/** wavesText words a policy's waves, such as "1 → 25% → 100%". */
export function wavesText(p: Pick<FleetRolloutPolicy, 'waves'> | undefined): string {
  const waves = p?.waves ?? [];
  if (!waves.length) return 'all at once';
  return waves.map((w) => (w.percent !== undefined && w.percent !== null ? `${w.percent}%` : String(w.count ?? '?'))).join(' → ');
}

export type RolloutAction = 'pause' | 'resume' | 'rollback' | 'cancel';

/** rolloutActions lists the controls fleet accepts in a state. */
export function rolloutActions(state: string): RolloutAction[] {
  switch (state) {
    case 'running': return ['pause', 'rollback', 'cancel'];
    case 'paused': return ['resume', 'rollback', 'cancel'];
    case 'rolling_back': return ['cancel'];
    default: return [];
  }
}

export const ACTION_LABEL: Record<RolloutAction, string> = { pause: 'Pause', resume: 'Resume', rollback: 'Roll back', cancel: 'Cancel rollout' };

/** isActive is true while fleet still drives the rollout. */
export function isActive(state: string): boolean {
  return state === 'running' || state === 'paused' || state === 'rolling_back';
}

/** rolloutChip words a rollout's state. */
export function rolloutChip(r: Pick<FleetRollout, 'state' | 'reason'>): Chip {
  switch (r.state) {
    case 'running': return { tone: 'running', icon: 'loader-circle', label: 'Running' };
    case 'paused': return { tone: 'warn', icon: 'circle-pause', label: r.reason === 'operator' || !r.reason ? 'Paused' : `Paused · ${r.reason.replaceAll('_', ' ')}` };
    case 'rolling_back': return { tone: 'warn', icon: 'rotate-ccw', label: 'Rolling back' };
    case 'completed': return { tone: 'pass', icon: 'circle-check', label: 'Completed' };
    case 'rolled_back': return { tone: 'fail', icon: 'rotate-ccw', label: 'Rolled back' };
    case 'cancelled': return { tone: 'plain', icon: 'circle-minus', label: 'Cancelled' };
    default: return { tone: 'plain', icon: 'circle-dashed', label: r.state };
  }
}

const FAILED = ['failed', 'unhealthy', 'timed_out', 'incompatible'];

/** RolloutTally sums a rollout's waves. */
export interface RolloutTally {
  targets: number;
  succeeded: number;
  failed: number;
  pending: number;
  /** Percent of targets settled successfully. */
  done: number;
  /** False when fleet sent no wave outcomes, as in its rollout list. */
  known: boolean;
}

export function rolloutTally(r: Pick<FleetRollout, 'targets' | 'waves'>): RolloutTally {
  let succeeded = 0, failed = 0;
  for (const w of r.waves ?? []) {
    for (const [k, n] of Object.entries(w.outcomes ?? {})) {
      if (k === 'succeeded') succeeded += n;
      else if (FAILED.includes(k)) failed += n;
    }
  }
  const targets = r.targets ?? 0;
  return { targets, succeeded, failed, pending: Math.max(0, targets - succeeded - failed), done: targets ? Math.round((100 * succeeded) / targets) : 0, known: r.waves !== undefined };
}

/** rolloutMove words what a rollout changes. */
export function rolloutMove(r: Pick<FleetRollout, 'from_version' | 'to_version'>): string {
  if (!r.to_version) return r.from_version ? `v${r.from_version} → removed` : 'removes the layer';
  return r.from_version ? `v${r.from_version} → v${r.to_version}` : `introduces v${r.to_version}`;
}

/**
 * eventActor names who made a fleet change: the debugger user a delegated
 * call acted for, else fleet's credential. The title keeps both IDs.
 */
export function eventActor(e: Pick<FleetEvent, 'actor_id' | 'acting_for'>): { label: string; title: string } {
  const a = e.acting_for;
  if (!a) return { label: e.actor_id ? shortHex(e.actor_id) : '—', title: e.actor_id };
  const request = a.request_id ? `, request ${a.request_id}` : '';
  return { label: a.name || a.id, title: `${a.id} through credential ${e.actor_id}${request}` };
}

/** eventText words a fleet event kind for the feed. */
export function eventText(e: Pick<FleetEvent, 'kind'>): { label: string; icon: IconName; tone: PhaseTone } {
  const [area, what = ''] = e.kind.split('.');
  const words = what.replaceAll('_', ' ');
  const label = `${area[0]?.toUpperCase() ?? ''}${area.slice(1)} ${words}`.trim();
  if (/failed|rolled_back|identity_conflict|held|revoked|disconnected/.test(what)) return { label, icon: 'triangle-alert', tone: 'warn' };
  if (/completed|connected|hold_cleared/.test(what)) return { label, icon: 'circle-check', tone: 'pass' };
  const icon: IconName = area === 'agent' ? 'server' : area === 'rollout' ? 'rocket' : area === 'group' ? 'users' : area === 'credential' ? 'key-round' : 'file-code';
  return { label, icon, tone: 'plain' };
}

/** relTime words how long ago a time was, coarse to a minute. */
export function relTime(iso: string | undefined | null, now = Date.now()): string {
  if (!iso) return '—';
  const s = Math.max(0, Math.round((now - Date.parse(iso)) / 1000));
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86400)} d ago`;
}

/** problemText words an API failure, with the fix for the common fleet refusals. */
export function problemText(e: unknown): string {
  const err = e as { status?: number; message?: string; body?: { code?: string } };
  const msg = err?.message ?? String(e);
  if (err?.status === 403) return `${msg} Writes need serve's fleet credential to have the admin role.`;
  if (err?.status === 409 && /revision|precondition/i.test(msg)) return `${msg} Someone changed it meanwhile; reload and try again.`;
  return msg;
}
