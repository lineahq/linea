import { describe, expect, it } from 'vitest';
import {
  agentName, agentPlace, configChip, eventActor, eventText, groupDraft, groupSave, healthChip, isFleetName, parseAgentIDs, problemText,
  relTime, rolloutActions, selectorSuggestions, selectorTail, quoteSelectorValue, attributeKeys, agentSuggestions, rolloutChip, rolloutMove, rolloutPolicy, rolloutTally, shortHex, wavesText, DEFAULT_ROLLOUT,
} from './fleetView';

const ID = 'a'.repeat(32);
const ID2 = 'b'.repeat(32);

describe('agents', () => {
  it('names an agent by service, host, then ID', () => {
    expect(agentName({ id: ID, attributes: { 'service.name': 'gateway', 'host.name': 'h1' } })).toBe('gateway');
    expect(agentName({ id: ID, attributes: { 'host.name': 'h1' } })).toBe('h1');
    expect(agentName({ id: ID })).toBe('aaaaaaaa');
    expect(shortHex('short')).toBe('short');
    expect(agentPlace({ attributes: { 'k8s.namespace.name': 'obs', 'k8s.pod.name': 'gw-0' } })).toBe('obs / gw-0');
  });

  it('words health and config state', () => {
    expect(healthChip({ connected: false }).label).toBe('Offline');
    expect(healthChip({ connected: true }).label).toBe('No health');
    expect(healthChip({ connected: true, health: { healthy: false, components_unhealthy: 2, observed_at: '' } }).label).toBe('Unhealthy · 2');
    expect(configChip({ status: 'applied' }).tone).toBe('pass');
    expect(configChip({ status: 'applied', held_hash: 'h' }).label).toBe('Held back');
    expect(configChip(undefined).label).toBe('Not managed');
  });
});

describe('groups', () => {
  it('checks names and agent IDs', () => {
    expect(isFleetName('edge-1')).toBe(true);
    expect(isFleetName('Edge')).toBe(false);
    expect(parseAgentIDs(`${ID}, ${ID.toUpperCase()}\nnope`)).toEqual({ ids: [ID], invalid: ['nope'] });
  });

  it('turns the form into a save request or a fix', () => {
    expect(groupSave(groupDraft())).toBe('Name the group.');
    expect(groupSave({ ...groupDraft(), name: 'gw' })).toMatch(/selector/);
    expect(groupSave({ ...groupDraft(), name: 'gw', selector: ' service.name=gw ' })).toEqual({ id: undefined, revision: 0, name: 'gw', selector: 'service.name=gw' });
    const g = { id: ID, revision: 3, name: 'pinned', kind: 'static', agent_ids: [ID, ID2] };
    expect(groupSave(groupDraft(g), g)).toEqual({ id: ID, revision: 3, name: 'pinned', agent_ids: [ID, ID2] });
    expect(groupSave({ ...groupDraft(g), agents: 'x' }, g)).toMatch(/Not an agent ID: x/);
  });
});

describe('rollouts', () => {
  it('reads a policy form', () => {
    expect(rolloutPolicy(DEFAULT_ROLLOUT)).toEqual({ waves: [{ percent: 100 }] });
    expect(rolloutPolicy({ waves: '1, 25%,100%', canary: ID, bake: '60', maxFailures: '10%', onFailure: 'rollback' })).toEqual({
      waves: [{ count: 1 }, { percent: 25 }, { percent: 100 }], canary_group_id: ID, bake_seconds: 60, max_failure_percent: 10, on_failure: 'rollback',
    });
    expect(rolloutPolicy({ ...DEFAULT_ROLLOUT, maxFailures: '2' })).toMatchObject({ max_failures: 2 });
    expect(rolloutPolicy({ ...DEFAULT_ROLLOUT, waves: '150%' })).toMatch(/between/);
    expect(rolloutPolicy({ ...DEFAULT_ROLLOUT, waves: 'half' })).toMatch(/not a count/);
    expect(rolloutPolicy({ ...DEFAULT_ROLLOUT, canary: 'gw' })).toMatch(/group ID/);
    expect(rolloutPolicy({ ...DEFAULT_ROLLOUT, bake: '1.5' })).toMatch(/whole seconds/);
  });

  it('words waves, moves and states', () => {
    expect(wavesText({ waves: [{ count: 1 }, { percent: 100 }] })).toBe('1 → 100%');
    expect(wavesText(undefined)).toBe('all at once');
    expect(rolloutMove({ from_version: 1, to_version: 2 })).toBe('v1 → v2');
    expect(rolloutMove({ to_version: 1 })).toBe('introduces v1');
    expect(rolloutMove({ from_version: 2 })).toBe('v2 → removed');
    expect(rolloutChip({ state: 'paused', reason: 'failure_budget' }).label).toBe('Paused · failure budget');
    expect(rolloutChip({ state: 'paused' }).label).toBe('Paused');
  });

  it('offers only the controls fleet accepts', () => {
    expect(rolloutActions('running')).toEqual(['pause', 'rollback', 'cancel']);
    expect(rolloutActions('paused')).toEqual(['resume', 'rollback', 'cancel']);
    expect(rolloutActions('rolling_back')).toEqual(['cancel']);
    expect(rolloutActions('completed')).toEqual([]);
  });

  it('tallies outcomes', () => {
    expect(rolloutTally({ targets: 4, waves: [{ wave: 0, targets: 1, outcomes: { succeeded: 1 } }, { wave: 1, targets: 3, outcomes: { succeeded: 1, failed: 1, pending: 1 } }] }))
      .toEqual({ targets: 4, succeeded: 2, failed: 1, pending: 1, done: 50, known: true });
    // Fleet's rollout list carries no outcomes: the tally is unknown, not zero done.
    expect(rolloutTally({ targets: 1 })).toMatchObject({ done: 0, known: false });
  });
});

describe('feed', () => {
  it('words event kinds', () => {
    expect(eventText({ kind: 'rollout.completed' })).toMatchObject({ label: 'Rollout completed', tone: 'pass' });
    expect(eventText({ kind: 'agent.disconnected' }).tone).toBe('warn');
    expect(eventText({ kind: 'config.version_created' })).toMatchObject({ label: 'Config version created', icon: 'file-code' });
  });

  it('names the acting user before the credential', () => {
    const actor = 'a'.repeat(32);
    expect(eventActor({ actor_id: actor })).toEqual({ label: 'aaaaaaaa', title: actor });
    expect(eventActor({ actor_id: '' }).label).toBe('—');
    expect(eventActor({ actor_id: actor, acting_for: { id: 'user:1f', name: 'Alice', request_id: 'req-1' } }))
      .toEqual({ label: 'Alice', title: `user:1f through credential ${actor}, request req-1` });
    expect(eventActor({ actor_id: actor, acting_for: { id: 'user:1f' } }).label).toBe('user:1f');
  });

  it('words times and problems', () => {
    const now = Date.parse('2026-10-27T12:00:00Z');
    expect(relTime('2026-10-27T11:59:30Z', now)).toBe('just now');
    expect(relTime('2026-10-27T11:30:00Z', now)).toBe('30 min ago');
    expect(relTime('2026-10-25T12:00:00Z', now)).toBe('2 d ago');
    expect(relTime(undefined)).toBe('—');
    expect(problemText({ status: 403, message: 'forbidden.' })).toMatch(/admin role/);
    expect(problemText({ status: 409, message: 'revision mismatch.' })).toMatch(/reload/);
  });
});

describe('group autocomplete', () => {
  const keys = ['host.name', 'k8s.namespace.name', 'service.name', 'service.version'];

  it('finds the term being typed past quoted values and in lists', () => {
    expect(selectorTail('')).toEqual({ head: '', term: '', used: [], valueKey: undefined });
    expect(selectorTail('service.name=a, host')).toMatchObject({ head: 'service.name=a,', term: ' host', used: ['service.name'] });
    expect(selectorTail('a="x,y",env in (p, q),k8s.namespace.name=ob')).toMatchObject({ used: ['a', 'env'], valueKey: 'k8s.namespace.name' });
    expect(selectorTail('env in (p, q').valueKey).toBeUndefined();
  });

  it('suggests unused keys, the prefix matches first', () => {
    expect(selectorSuggestions('serv', keys).map((o) => o.value)).toEqual(['service.name=', 'service.version=']);
    expect(selectorSuggestions('service.name=gw, name', keys).map((o) => o.value)).toEqual(['service.name=gw, host.name=', 'service.name=gw, k8s.namespace.name=']);
    expect(selectorSuggestions('env in (a', keys)).toEqual([]);
  });

  it('suggests values with their agents once the key is typed, quoted as fleet reads them', () => {
    const values = { key: 'service.name', groups: [{ value: 'gateway', total: 3, connected: 2 }, { value: 'a,b', total: 1, connected: 1 }, { value: '', total: 4, connected: 0 }] };
    expect(selectorSuggestions('env=prod,service.name=', keys, values)).toEqual([
      { value: 'env=prod,service.name=gateway', label: '3 agents, 2 connected' },
      { value: 'env=prod,service.name="a,b"', label: '1 agent, 1 connected' },
    ]);
    expect(selectorSuggestions('service.name=GA', keys, values).map((o) => o.value)).toEqual(['service.name=gateway']);
    expect(selectorSuggestions('host.name=', keys, values)).toEqual([]);
    expect(quoteSelectorValue(' x')).toBe('" x"');
  });

  it('collects keys and offers agents not chosen yet', () => {
    expect(attributeKeys([{ attributes: { b: '1', a: '2' } }, { attributes: { a: '3' } }, {}])).toEqual(['a', 'b']);
    const agents = [{ id: ID, connected: true, attributes: { 'service.name': 'gw', 'host.name': 'h1' } }, { id: ID2, connected: false }];
    expect(agentSuggestions(agents, [ID])).toEqual([{ value: ID2, label: 'bbbbbbbb · offline' }]);
    expect(agentSuggestions(agents, [])[0].label).toBe('gw · h1 · connected');
  });
});
