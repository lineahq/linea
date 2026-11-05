import { useState } from 'react';
import { Link, useNavigate, useSearch } from '@tanstack/react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../api/client';
import type { FleetAssignment } from '../../api/types';
import { InlineConfirm } from '../../capture/pages/InlineConfirm';
import { Icon } from '../../ui/Icon';
import { DEFAULT_ROLLOUT, isFleetName, relTime, rolloutPolicy, shortHex, type RolloutForm } from '../fleetView';
import { FleetGate, FleetHead, Pager, Problem, useCursors } from './FleetHead';
import { RolloutFields } from './RolloutFields';
import { useDenial } from '../../ui/session';

/** AssignmentsSearch preselects a config version for a new assignment. */
export interface AssignmentsSearch { config?: string; version?: number }

export function parseAssignmentsSearch(s: Record<string, unknown>): AssignmentsSearch {
  const version = Number(s.version);
  return {
    config: typeof s.config === 'string' && s.config ? s.config : undefined,
    version: Number.isInteger(version) && version > 0 ? version : undefined,
  };
}

/**
 * AssignmentsPage puts config versions on agents. An assignment
 * targets a group or a selector at a layer; fleet changes it only through a
 * rollout, so creating, re-versioning and removing all start one.
 */
export function AssignmentsPage() {
  const search = useSearch({ strict: false }) as AssignmentsSearch;
  const pages = useCursors();
  const [creating, setCreating] = useState(!!search.config);
  const [selected, setSelected] = useState<string>();
  const assignments = useQuery({ queryKey: ['fleet-assignments', pages.cursor], queryFn: () => api.fleetAssignments(pages.cursor), refetchInterval: 10000, refetchIntervalInBackground: false });
  const configs = useQuery({ queryKey: ['fleet-configs', ''], queryFn: () => api.fleetConfigs('') });
  const configName = new Map((configs.data?.items ?? []).map((c) => [c.id, c.name]));
  const items = assignments.data?.items ?? [];
  const writeDenied = useDenial('fleet.config.write');
  const action = (
    <button type="button" className="od-btn od-btn--sm od-btn--primary" disabled={creating || !!writeDenied} title={writeDenied} onClick={() => setCreating(true)}>
      <Icon name="plus" size={14} /><span>New assignment</span>
    </button>
  );
  return (
    <>
      <FleetHead actions={action} />
      <FleetGate configs>
        <div className="od-page" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          {creating && <NewAssignment initial={search} onDone={(a) => { setCreating(false); if (a) setSelected(a.id); }} />}
          {assignments.error && <Problem title="Could not load assignments" error={assignments.error} retry={() => { pages.reset(); void assignments.refetch(); }} />}
          {assignments.isPending && <p role="status" className="od-muted">Loading assignments…</p>}
          {assignments.data && (
            <div className="od-card" style={{ overflow: 'hidden' }}>
              <table className="od-table">
                <thead><tr><th>Assignment</th><th>Config</th><th className="num">Version</th><th className="num">Layer</th><th>Targets</th><th>Rollout</th><th>Created</th></tr></thead>
                <tbody>
                  {items.map((a) => (
                    <tr key={a.id} aria-selected={selected === a.id}>
                      <td><button type="button" className="od-btn od-btn--link od-btn--sm mono" aria-label={`Show assignment ${a.name}`} onClick={() => setSelected(a.id)}>{a.name}</button></td>
                      <td><Link to="/fleet/configs/$configId" params={{ configId: a.config_id }} className="mono">{configName.get(a.config_id) ?? shortHex(a.config_id)}</Link></td>
                      <td className="num">{a.version ? `v${a.version}` : <span className="od-subtle">none</span>}</td>
                      <td className="num">{a.layer}</td>
                      <td className="mono">{a.group_id ? `group ${shortHex(a.group_id)}` : a.selector || 'all agents'}</td>
                      <td>{a.rollout_id ? <Link to="/fleet/rollouts/$rolloutId" params={{ rolloutId: a.rollout_id }} className="mono">{shortHex(a.rollout_id)}</Link> : <span className="od-subtle">—</span>}</td>
                      <td className="od-subtle">{relTime(a.created_at)} · {a.created_by}</td>
                    </tr>
                  ))}
                  {!items.length && <tr><td colSpan={7} className="od-subtle" style={{ textAlign: 'center', padding: 24 }}>No assignments. Assign a config version to put it on agents.</td></tr>}
                </tbody>
              </table>
              <Pager c={pages} next={assignments.data.next_cursor} noun="assignments" />
            </div>
          )}
          {selected && <AssignmentView key={selected} id={selected} configName={configName} onClose={() => setSelected(undefined)} />}
        </div>
      </FleetGate>
    </>
  );
}

function NewAssignment({ initial, onDone }: { initial: AssignmentsSearch; onDone: (a?: FleetAssignment) => void }) {
  const qc = useQueryClient();
  const configs = useQuery({ queryKey: ['fleet-configs', ''], queryFn: () => api.fleetConfigs('') });
  const groups = useQuery({ queryKey: ['collector-groups', ''], queryFn: () => api.collectorGroups('') });
  const [name, setName] = useState('');
  const [configId, setConfigId] = useState(initial.config ?? '');
  const [version, setVersion] = useState(initial.version ? String(initial.version) : '');
  const [layer, setLayer] = useState('10');
  const [target, setTarget] = useState<'group' | 'selector'>('group');
  const [groupId, setGroupId] = useState('');
  const [selector, setSelector] = useState('');
  const [rollout, setRollout] = useState<RolloutForm>(DEFAULT_ROLLOUT);
  const [fix, setFix] = useState('');
  const config = (configs.data?.items ?? []).find((c) => c.id === configId);
  const create = useMutation({
    mutationFn: api.createFleetAssignment,
    onSuccess: (a) => {
      void qc.invalidateQueries({ queryKey: ['fleet-assignments'] });
      void qc.invalidateQueries({ queryKey: ['fleet-rollouts'] });
      onDone(a);
    },
  });
  const submit = () => {
    const policy = rolloutPolicy(rollout);
    const v = Number(version);
    const l = Number(layer);
    const problem =
      !isFleetName(name.trim()) ? 'Name it with lowercase letters, digits and dashes.'
      : !configId ? 'Choose a config.'
      : !Number.isInteger(v) || v < 1 || (config && v > config.latest_version) ? `Choose a version${config ? ` from 1 to ${config.latest_version}` : ''}.`
      : !Number.isInteger(l) || l < 0 ? 'The layer is a whole number; higher layers win.'
      : target === 'group' && !groupId ? 'Choose a group.'
      : target === 'selector' && !selector.trim() ? 'Write a selector.'
      : typeof policy === 'string' ? policy : '';
    setFix(problem);
    if (problem || typeof policy === 'string') return;
    create.mutate({
      name: name.trim(), config_id: configId, version: v, layer: l,
      group_id: target === 'group' ? groupId : undefined,
      selector: target === 'selector' ? selector.trim() : undefined,
      rollout: policy,
    });
  };
  return (
    <form className="od-card" aria-label="New assignment" onSubmit={(e) => { e.preventDefault(); submit(); }}>
      <div className="od-card__head"><h2 className="od-card__title" style={{ margin: 0 }}>New assignment</h2></div>
      <fieldset disabled={create.isPending} className="od-card__body" style={{ border: 0, margin: 0, display: 'flex', flexDirection: 'column', gap: 12 }}>
        <div style={{ display: 'flex', gap: 12, flexWrap: 'wrap', alignItems: 'flex-start' }}>
          <div className="od-field" style={{ width: 220 }}>
            <label htmlFor="assign-name">Name</label>
            <input id="assign-name" className="od-input mono" autoFocus value={name} onChange={(e) => setName(e.target.value)} />
          </div>
          <div className="od-field" style={{ width: 220 }}>
            <label htmlFor="assign-config">Config</label>
            <select id="assign-config" className="od-select" value={configId} onChange={(e) => { setConfigId(e.target.value); setVersion(''); }}>
              <option value="">Choose a config…</option>
              {(configs.data?.items ?? []).map((c) => <option key={c.id} value={c.id} disabled={!c.latest_version}>{c.name}{c.latest_version ? '' : ' (no versions)'}</option>)}
            </select>
          </div>
          <div className="od-field" style={{ width: 110 }}>
            <label htmlFor="assign-version">Version</label>
            <select id="assign-version" className="od-select" value={version} onChange={(e) => setVersion(e.target.value)}>
              <option value="">—</option>
              {Array.from({ length: config?.latest_version ?? (initial.version ?? 0) }, (_, i) => i + 1).reverse().map((v) => <option key={v} value={v}>v{v}</option>)}
            </select>
          </div>
          <div className="od-field" style={{ width: 100 }}>
            <label htmlFor="assign-layer">Layer</label>
            <input id="assign-layer" className="od-input" inputMode="numeric" value={layer} onChange={(e) => setLayer(e.target.value)} />
          </div>
        </div>
        <div style={{ display: 'flex', gap: 12, flexWrap: 'wrap', alignItems: 'flex-end' }}>
          <div className="od-seg" role="group" aria-label="Targets">
            <button type="button" aria-pressed={target === 'group'} onClick={() => setTarget('group')}>Group</button>
            <button type="button" aria-pressed={target === 'selector'} onClick={() => setTarget('selector')}>Selector</button>
          </div>
          {target === 'group' ? (
            <div className="od-field" style={{ width: 260 }}>
              <label htmlFor="assign-group">Group</label>
              <select id="assign-group" className="od-select" value={groupId} onChange={(e) => setGroupId(e.target.value)}>
                <option value="">Choose a group…</option>
                {(groups.data?.items ?? []).map((g) => <option key={g.id} value={g.id}>{g.name}</option>)}
              </select>
            </div>
          ) : (
            <div className="od-field" style={{ flex: 1, minWidth: 260 }}>
              <label htmlFor="assign-selector">Selector</label>
              <input id="assign-selector" className="od-input mono" placeholder="service.name=gateway" value={selector} onChange={(e) => setSelector(e.target.value)} />
            </div>
          )}
        </div>
        <RolloutFields form={rollout} onChange={setRollout} idPrefix="assign" />
        {fix && <span role="alert" className="od-field__error">{fix}</span>}
        {create.error && <Problem title="Could not create the assignment" error={create.error} />}
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <button type="submit" className="od-btn od-btn--primary od-btn--sm"><Icon name="rocket" size={14} /><span>{create.isPending ? 'Starting…' : 'Assign and roll out'}</span></button>
          <button type="button" className="od-btn od-btn--sm" onClick={() => onDone()}>Cancel</button>
          <span className="od-subtle" style={{ fontSize: 12 }}>Fleet starts a rollout that brings the version to the targets wave by wave.</span>
        </div>
      </fieldset>
    </form>
  );
}

/** AssignmentView is one assignment: who it reaches now, and a rollout to change or remove it. */
function AssignmentView({ id, configName, onClose }: { id: string; configName: Map<string, string>; onClose: () => void }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const pages = useCursors();
  const assignment = useQuery({ queryKey: ['fleet-assignment', id], queryFn: () => api.fleetAssignment(id) });
  const preview = useQuery({ queryKey: ['fleet-assignment-preview', id, pages.cursor], queryFn: () => api.previewFleetAssignment(id, pages.cursor) });
  const [mode, setMode] = useState<'' | 'version' | 'remove'>('');
  const [toVersion, setToVersion] = useState('');
  const [rollout, setRollout] = useState<RolloutForm>(DEFAULT_ROLLOUT);
  const [fix, setFix] = useState('');
  const config = useQuery({ queryKey: ['fleet-config', assignment.data?.config_id], queryFn: () => api.fleetConfig(assignment.data!.config_id), enabled: !!assignment.data });
  const rolloutDenied = useDenial('fleet.rollout');
  const start = useMutation({
    mutationFn: api.startFleetRollout,
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: ['fleet-assignments'] });
      void qc.invalidateQueries({ queryKey: ['fleet-rollouts'] });
      void navigate({ to: '/fleet/rollouts/$rolloutId', params: { rolloutId: r.id } });
    },
  });
  const a = assignment.data;
  const go = (remove: boolean) => {
    const policy = rolloutPolicy(rollout);
    if (typeof policy === 'string') { setFix(policy); return; }
    const v = Number(toVersion);
    if (!remove && (!Number.isInteger(v) || v < 1)) { setFix('Choose the version to roll out.'); return; }
    setFix('');
    start.mutate({ assignment_id: id, policy, ...(remove ? { delete: true } : { to_version: v }) });
  };
  return (
    <section className="od-card" aria-label="Assignment" style={{ overflow: 'hidden' }}>
      <div className="od-card__head" style={{ gap: 8 }}>
        <h3 className="od-card__title mono" style={{ margin: 0 }}>{a?.name ?? shortHex(id)}</h3>
        {a && <span className="od-subtle" style={{ fontSize: 12 }}>{configName.get(a.config_id) ?? shortHex(a.config_id)} {a.version ? `v${a.version}` : ''} · layer {a.layer} · revision {a.revision}</span>}
        <span style={{ flex: 1 }} />
        <button type="button" className="od-btn od-btn--sm" disabled={!a || !!mode || !!rolloutDenied} title={rolloutDenied} onClick={() => setMode('version')}><Icon name="rocket" size={14} /><span>Roll out another version</span></button>
        <button type="button" className="od-btn od-btn--sm od-btn--danger" disabled={!a || !!mode || !!rolloutDenied} title={rolloutDenied} onClick={() => setMode('remove')}><Icon name="trash-2" size={14} /><span>Remove assignment</span></button>
        <button type="button" className="od-btn od-btn--ghost od-btn--sm od-btn--icon" aria-label="Close assignment" onClick={onClose}><Icon name="x" size={14} /></button>
      </div>
      {assignment.error && <Problem bar title="Could not load this assignment" error={assignment.error} retry={() => void assignment.refetch()} />}
      {mode && (
        <form className="od-card__body" style={{ display: 'flex', flexDirection: 'column', gap: 12, borderBottom: '1px solid var(--border)' }} onSubmit={(e) => { e.preventDefault(); go(mode === 'remove'); }}>
          {mode === 'version' && (
            <div className="od-field" style={{ width: 160 }}>
              <label htmlFor="move-version">Roll out version</label>
              <select id="move-version" className="od-select" value={toVersion} onChange={(e) => setToVersion(e.target.value)}>
                <option value="">—</option>
                {Array.from({ length: config.data?.latest_version ?? 0 }, (_, i) => i + 1).reverse().filter((v) => v !== a?.version).map((v) => <option key={v} value={v}>v{v}</option>)}
              </select>
            </div>
          )}
          {mode === 'remove' && <p style={{ margin: 0 }}>Removing takes this layer off its agents through a rollout; the assignment is deleted when it completes.</p>}
          <RolloutFields form={rollout} onChange={setRollout} idPrefix="move" />
          {fix && <span role="alert" className="od-field__error">{fix}</span>}
          {start.error && <Problem title="Could not start the rollout" error={start.error} />}
          {mode === 'remove' ? (
            <InlineConfirm danger confirmLabel="Remove and roll out" busy={start.isPending} onCancel={() => setMode('')} onConfirm={() => go(true)}>
              Take <strong>{a?.name}</strong> off every agent it reaches?
            </InlineConfirm>
          ) : (
            <div style={{ display: 'flex', gap: 8 }}>
              <button type="submit" className="od-btn od-btn--primary od-btn--sm" disabled={start.isPending}>{start.isPending ? 'Starting…' : 'Start rollout'}</button>
              <button type="button" className="od-btn od-btn--sm" onClick={() => setMode('')}>Cancel</button>
            </div>
          )}
        </form>
      )}
      <div className="od-card__head"><h4 className="od-card__title" style={{ margin: 0, fontSize: 13 }}>Reaches now</h4></div>
      {preview.isPending && <p role="status" className="od-muted" style={{ padding: '8px 14px' }}>Resolving targets…</p>}
      {preview.error && <Problem bar title="Could not preview the targets" error={preview.error} retry={() => void preview.refetch()} />}
      {preview.data && !preview.data.items?.length && <div className="od-empty"><div className="od-empty__title">No agent matches</div><div className="od-empty__body">A rollout of this assignment would have no targets.</div></div>}
      {!!preview.data?.items?.length && (
        <table className="od-table">
          <thead><tr><th>Agent</th><th>Effective layers</th><th>Problem</th></tr></thead>
          <tbody>
            {preview.data.items.map((p) => (
              <tr key={p.agent_id}>
                <td className="mono">{p.agent_id}</td>
                <td className="mono">{(p.desired?.layers ?? []).map((l) => `${l.name}${l.version ? ` v${l.version}` : ''}`).join(' · ') || '—'}</td>
                <td className="od-muted">{p.error || '—'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <Pager c={pages} next={preview.data?.next_cursor} noun="targets" />
    </section>
  );
}
