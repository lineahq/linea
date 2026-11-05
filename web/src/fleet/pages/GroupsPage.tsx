import { useEffect, useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../api/client';
import type { CollectorGroup } from '../../api/types';
import { availabilityChip, locationText } from '../../capture/collectorView';
import { loadCollectorInventory } from '../../capture/groupCapture';
import { chipClass } from '../../capture/phase';
import { InlineConfirm } from '../../capture/pages/InlineConfirm';
import { Icon } from '../../ui/Icon';
import { groupKindText, groupMembership } from '../fleetGroups';
import { agentSuggestions, attributeKeys, groupDraft, groupSave, parseAgentIDs, selectorSuggestions, selectorTail, type GroupDraft } from '../fleetView';
import { FleetGate, FleetHead, Problem } from './FleetHead';
import { Permitted } from '../../ui/Permitted';
import { useDenial } from '../../ui/session';

/**
 * Groups: fleet's agent groups. A group is a selector over agent
 * attributes or a fixed list of agents; assignments, rollout canaries and
 * captures name one, and fleet resolves its members when they act.
 */
export function GroupsPage() {
  const [cursors, setCursors] = useState(['']);
  const cursor = cursors[cursors.length - 1];
  const [selected, setSelected] = useState<string>();
  const [creating, setCreating] = useState(false);
  const groups = useQuery({ queryKey: ['collector-groups', cursor], queryFn: () => api.collectorGroups(cursor), refetchInterval: 5000, refetchIntervalInBackground: false });
  const items = groups.data?.items ?? [];
  const firstID = items[0]?.id;

  // Show the first group once the list arrives.
  useEffect(() => {
    if (!selected && firstID) setSelected(firstID);
  }, [firstID, selected]);

  const writeDenied = useDenial('fleet.config.write');
  const newGroup = (
    <button type="button" className="od-btn od-btn--sm od-btn--primary" disabled={creating || !!writeDenied} title={writeDenied} onClick={() => setCreating(true)}>
      <Icon name="plus" size={14} />
      <span>New group</span>
    </button>
  );

  return (
    <>
      <FleetHead actions={newGroup} />
      <FleetGate>
        <div style={{ display: 'flex', flex: 1, minHeight: 0 }}>
          <nav aria-label="Collector groups" style={{ width: 280, flexShrink: 0, borderRight: '1px solid var(--border)', overflow: 'auto', padding: 12, display: 'flex', flexDirection: 'column', gap: 6 }}>
            {groups.isPending && <p role="status" className="od-muted">Loading groups…</p>}
            {groups.error && <Problem title="Could not load groups" error={groups.error} retry={() => { setCursors(['']); void groups.refetch(); }} />}
            {groups.data && !items.length && cursors.length === 1 && (
              <div className="od-empty">
                <div className="od-empty__title">No groups yet</div>
                <div className="od-empty__body">A group names a set of agents, by selector or by ID, so you can assign configs to them or capture from all of their Collectors at once.</div>
              </div>
            )}
            {items.map((g) => {
              const current = selected === g.id && !creating;
              return (
                <button
                  type="button"
                  key={g.id}
                  className="od-card"
                  aria-current={current ? 'true' : undefined}
                  onClick={() => { setCreating(false); setSelected(g.id); }}
                  style={{ textAlign: 'left', padding: '8px 12px', cursor: 'pointer', borderColor: current ? 'var(--accent)' : undefined, font: 'inherit', color: 'inherit', background: current ? 'var(--accent-soft, var(--surface))' : undefined }}
                >
                  <strong>{g.name}</strong>
                  <div className="od-subtle" style={{ fontSize: 12 }}>{groupMembership(g)}</div>
                </button>
              );
            })}
            {(cursors.length > 1 || groups.data?.next_cursor) && (
              <div className="od-pager" style={{ padding: 0 }}>
                <button type="button" className="od-btn od-btn--ghost od-btn--sm" disabled={cursors.length === 1} onClick={() => setCursors((p) => p.slice(0, -1))}>Previous groups</button>
                <button type="button" className="od-btn od-btn--ghost od-btn--sm" disabled={!groups.data?.next_cursor} onClick={() => { const n = groups.data?.next_cursor; if (n) setCursors((p) => [...p, n]); }}>Next groups</button>
              </div>
            )}
          </nav>
          <div className="od-page" style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column', gap: 12 }}>
            {creating && <GroupEditor onDone={(g) => { setCreating(false); if (g) setSelected(g.id); }} />}
            {!creating && !selected && !groups.isPending && items.length > 0 && <p className="od-muted">Choose a group to see its members.</p>}
            {!creating && selected && <GroupView key={selected} id={selected} onGone={() => setSelected(undefined)} />}
          </div>
        </div>
      </FleetGate>
    </>
  );
}

function GroupView({ id, onGone }: { id: string; onGone: () => void }) {
  const qc = useQueryClient();
  const [editing, setEditing] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [cursors, setCursors] = useState(['']);
  const cursor = cursors[cursors.length - 1];
  const group = useQuery({ queryKey: ['collector-group', id], queryFn: () => api.collectorGroup(id), retry: false });
  const members = useQuery({ queryKey: ['collector-group-members', id, cursor], queryFn: () => api.collectorGroupMembers(id, cursor), refetchInterval: 5000, refetchIntervalInBackground: false });
  // A fleet without the livecapture plugin has no Collectors to look up.
  const capture = members.data?.capture_linked !== false;
  const inventory = useQuery({ queryKey: ['capture-inventory'], queryFn: loadCollectorInventory, refetchInterval: 10000, refetchIntervalInBackground: false, enabled: members.data?.capture_linked === true });
  const writeDenied = useDenial('fleet.config.write');
  const remove = useMutation({
    mutationFn: (g: CollectorGroup) => api.deleteCollectorGroup(g.id, g.revision),
    onSuccess: () => { void qc.invalidateQueries({ queryKey: ['collector-groups'] }); onGone(); },
  });

  if (group.isPending) return <p role="status" className="od-muted">Loading group…</p>;
  if (group.error) {
    return (
      <div className="od-callout od-callout--fail" role="alert">
        <Icon name="circle-x" size={16} />
        <div className="od-callout__main">
          <div className="od-callout__title">Could not load this group</div>
          <div className="od-callout__body">{group.error.message}</div>
          <div className="od-callout__actions">
            <button type="button" className="od-btn od-btn--sm" onClick={() => void group.refetch()}>Try again</button>
            <button type="button" className="od-btn od-btn--sm" onClick={onGone}>Close</button>
          </div>
        </div>
      </div>
    );
  }
  const g = group.data;
  if (editing) return <GroupEditor group={g} onDone={() => setEditing(false)} />;
  const list = members.data?.items ?? [];
  const connected = list.filter((m) => m.connected).length;
  const collectors = new Map((inventory.data ?? []).map((c) => [c.id, c]));
  return (
    <>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
        <h2 style={{ margin: 0, fontSize: 18 }}>{g.name}</h2>
        <span className="mono od-subtle" style={{ fontSize: 12 }}>{g.id}</span>
        <span style={{ flex: 1 }} />
        <button type="button" className="od-btn od-btn--sm" disabled={!!writeDenied} title={writeDenied} onClick={() => setEditing(true)}><Icon name="pencil" size={14} /><span>Edit group</span></button>
        <button type="button" className="od-btn od-btn--sm od-btn--danger" disabled={remove.isPending || confirming || !!writeDenied} title={writeDenied} onClick={() => setConfirming(true)}><Icon name="trash-2" size={14} /><span>Delete group</span></button>
        {capture && (
          <Permitted perm="capture.start"><Link to="/captures/new" search={{ group: g.id }} className="od-btn od-btn--primary od-btn--sm">
            <Icon name="radio" size={14} />
            <span>Capture this group</span>
          </Link></Permitted>
        )}
      </div>
      {confirming && (
        <InlineConfirm danger confirmLabel="Delete group" busy={remove.isPending} onCancel={() => setConfirming(false)} onConfirm={() => remove.mutate(g)}>
          Delete <strong>{g.name}</strong>? Fleet refuses while an assignment or a rollout canary names it. Running captures keep the agents they resolved.
        </InlineConfirm>
      )}
      {remove.error && <Problem title="Could not delete the group" error={remove.error} />}
      <section className="od-card">
        <div className="od-card__head"><h3 className="od-card__title" style={{ margin: 0 }}>Membership</h3><span className="od-subtle" style={{ fontSize: 12 }}>{groupKindText(g.kind)} · revision {g.revision}</span></div>
        <div className="od-card__body">
          {g.selector ? (
            <dl className="od-kv">
              <dt>Selector</dt>
              <dd className="mono">{g.selector}</dd>
            </dl>
          ) : (
            <p className="od-muted" style={{ margin: 0 }}>{groupMembership(g)}.</p>
          )}
          <p className="od-subtle" style={{ margin: '8px 0 0', fontSize: 12.5 }}>Fleet resolves the members when a capture or rollout starts; agents that join later are not added to a running capture.</p>
        </div>
      </section>
      <section className="od-card" style={{ overflow: 'hidden' }}>
        <div className="od-card__head">
          <h3 className="od-card__title" style={{ margin: 0 }}>Members now</h3>
          {members.data && <span className="od-subtle" style={{ fontSize: 12 }}>{connected} of {list.length}{members.data.next_cursor ? '+' : ''} connected on this page</span>}
        </div>
        {members.isPending && <p role="status" className="od-muted" style={{ padding: '10px 14px', margin: 0 }}>Loading members…</p>}
        {members.error && (
          <div className="od-callout od-callout--fail od-callout--bar" role="alert">
            <Icon name="circle-x" size={16} />
            <div className="od-callout__main"><span className="od-callout__title">Could not resolve members</span><div className="od-callout__body">{members.error.message}</div></div>
            <div className="od-callout__actions"><button type="button" className="od-btn od-btn--sm" onClick={() => void members.refetch()}>Try again</button></div>
          </div>
        )}
        {members.data && !capture && <p className="od-subtle" style={{ padding: '10px 14px', margin: 0, fontSize: 12.5 }}>This fleet has no live capture, so members list no Collectors.</p>}
        {members.data && !list.length && <div className="od-empty"><div className="od-empty__title">No agents in this group right now</div><div className="od-empty__body">An assignment or capture of this group would have no targets.</div></div>}
        {list.length > 0 && (
          <table className="od-table">
            <thead><tr><th>Agent</th><th>Agent status</th><th>Collectors</th></tr></thead>
            <tbody>
              {list.map((m) => (
                <tr key={m.agent_id}>
                  <td className="mono">{m.agent_id}</td>
                  <td>{m.connected ? <span className={chipClass('pass')}><Icon name="circle-check" size={12} />Connected</span> : <span className={chipClass('plain')}><Icon name="circle-minus" size={12} />Offline</span>}</td>
                  <td>
                    {!(m.collector_ids ?? []).length && <span className="od-subtle">{m.connected && capture ? 'No Collector with the livecapture extension' : '—'}</span>}
                    {(m.collector_ids ?? []).map((cid) => {
                      const c = collectors.get(cid);
                      if (!c) return <div key={cid} className="mono od-subtle">{cid.slice(0, 12)}</div>;
                      const chip = availabilityChip(c);
                      return (
                        <div key={cid} style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                          <span className="mono">{c.pod}</span>
                          <span className="od-subtle">{locationText(c)}</span>
                          <span className={chipClass(chip.tone)}><Icon name={chip.icon} size={12} />{chip.label}</span>
                        </div>
                      );
                    })}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {(cursors.length > 1 || members.data?.next_cursor) && (
          <div className="od-pager">
            <span>Page {cursors.length}</span>
            <span style={{ display: 'flex', gap: 4 }}>
              <button type="button" className="od-btn od-btn--ghost od-btn--sm" disabled={cursors.length === 1} onClick={() => setCursors((p) => p.slice(0, -1))}>Previous members</button>
              <button type="button" className="od-btn od-btn--ghost od-btn--sm" disabled={!members.data?.next_cursor} onClick={() => { const n = members.data?.next_cursor; if (n) setCursors((p) => [...p, n]); }}>Next members</button>
            </span>
          </div>
        )}
      </section>
    </>
  );
}

/** GroupEditor creates a group, or changes one at the revision it was loaded at. */
function GroupEditor({ group, onDone }: { group?: CollectorGroup; onDone: (saved?: CollectorGroup) => void }) {
  const qc = useQueryClient();
  const [draft, setDraft] = useState<GroupDraft>(() => groupDraft(group));
  const [fix, setFix] = useState('');
  const [pick, setPick] = useState('');
  // Suggestions: keys and agents from fleet's first page of agents; values
  // with their counts across the whole fleet, once a key is typed.
  const agents = useQuery({ queryKey: ['fleet-agents-suggest'], queryFn: () => api.fleetAgents({ limit: 100 }), staleTime: 30_000 });
  const valueKey = draft.kind === 'selector' ? selectorTail(draft.selector).valueKey : undefined;
  const values = useQuery({ queryKey: ['fleet-overview', valueKey], queryFn: () => api.fleetOverview(valueKey), enabled: !!valueKey, staleTime: 30_000 });
  const known = agents.data?.items ?? [];
  const selectorOptions = selectorSuggestions(draft.selector, attributeKeys(known), valueKey && values.data ? { key: valueKey, groups: values.data.groups ?? [] } : undefined);
  const agentOptions = agentSuggestions(known, parseAgentIDs(draft.agents).ids);
  const addAgent = (v: string) => {
    if (!known.some((a) => a.id === v)) { setPick(v); return; }
    setDraft((d) => ({ ...d, agents: d.agents.trim() ? `${d.agents.trimEnd()}
${v}` : v }));
    setPick('');
  };
  const save = useMutation({
    mutationFn: api.saveCollectorGroup,
    onSuccess: (g) => {
      void qc.invalidateQueries({ queryKey: ['collector-groups'] });
      qc.setQueryData(['collector-group', g.id], g);
      void qc.invalidateQueries({ queryKey: ['collector-group-members', g.id] });
      onDone(g);
    },
  });
  const submit = () => {
    const req = groupSave(draft, group);
    if (typeof req === 'string') { setFix(req); return; }
    setFix('');
    save.mutate(req);
  };
  return (
    <form className="od-card" aria-label={group ? 'Edit group' : 'New group'} onSubmit={(e) => { e.preventDefault(); submit(); }}>
      <div className="od-card__head"><h2 className="od-card__title" style={{ margin: 0 }}>{group ? `Edit ${group.name}` : 'New group'}</h2></div>
      <fieldset disabled={save.isPending} className="od-card__body" style={{ border: 0, margin: 0, display: 'flex', flexDirection: 'column', gap: 12 }}>
        <div className="od-field" style={{ maxWidth: 360 }}>
          <label htmlFor="group-name">Name</label>
          <input id="group-name" className="od-input" autoFocus value={draft.name} onChange={(e) => setDraft({ ...draft, name: e.target.value })} />
        </div>
        <div className="od-seg" role="group" aria-label="Membership">
          <button type="button" aria-pressed={draft.kind === 'selector'} onClick={() => setDraft({ ...draft, kind: 'selector' })}>By selector</button>
          <button type="button" aria-pressed={draft.kind === 'static'} onClick={() => setDraft({ ...draft, kind: 'static' })}>Fixed agents</button>
        </div>
        {draft.kind === 'selector' ? (
          <div className="od-field">
            <label htmlFor="group-selector">Selector</label>
            <input id="group-selector" className="od-input mono" list="group-selector-options" autoComplete="off" placeholder="service.name=gateway,k8s.namespace.name=obs" value={draft.selector} onChange={(e) => setDraft({ ...draft, selector: e.target.value })} />
            <datalist id="group-selector-options">
              {selectorOptions.map((o) => <option key={o.value} value={o.value} label={o.label} />)}
            </datalist>
            <span className="od-field__help">Agents whose attributes match join and leave on their own. Suggestions name the attributes agents report and, after <code>=</code>, each value with its agents.</span>
          </div>
        ) : (
          <div className="od-field">
            <label htmlFor="group-agents">Agent IDs</label>
            <input id="group-agent-pick" className="od-input" list="group-agent-options" autoComplete="off" aria-label="Add an agent" placeholder="Add an agent: type its name or ID" value={pick} onChange={(e) => addAgent(e.target.value.trim())} style={{ marginBottom: 6 }} />
            <datalist id="group-agent-options">
              {agentOptions.map((o) => <option key={o.value} value={o.value} label={o.label} />)}
            </datalist>
            <textarea id="group-agents" className="od-input mono" rows={5} spellCheck={false} value={draft.agents} onChange={(e) => setDraft({ ...draft, agents: e.target.value })} />
            <span className="od-field__help">One per line or separated by commas; pick from the list above or paste IDs.</span>
          </div>
        )}
        {fix && <span role="alert" className="od-field__error">{fix}</span>}
        {save.error && <Problem title="Could not save the group" error={save.error} />}
        <div style={{ display: 'flex', gap: 8 }}>
          <button type="submit" className="od-btn od-btn--primary od-btn--sm">{save.isPending ? 'Saving…' : group ? 'Save group' : 'Create group'}</button>
          <button type="button" className="od-btn od-btn--sm" onClick={() => onDone()}>Cancel</button>
        </div>
      </fieldset>
    </form>
  );
}
