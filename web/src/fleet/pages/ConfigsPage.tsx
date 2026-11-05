import { useState } from 'react';
import { Link, useNavigate } from '@tanstack/react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../api/client';
import { Icon } from '../../ui/Icon';
import { isFleetName, relTime, shortHex } from '../fleetView';
import { FleetGate, FleetHead, Pager, Problem, useCursors } from './FleetHead';
import { useDenial } from '../../ui/session';

/** ConfigsPage lists fleet's managed configs and creates new ones. */
export function ConfigsPage() {
  const pages = useCursors();
  const [creating, setCreating] = useState(false);
  const configs = useQuery({ queryKey: ['fleet-configs', pages.cursor], queryFn: () => api.fleetConfigs(pages.cursor), refetchInterval: 10000, refetchIntervalInBackground: false });
  const items = configs.data?.items ?? [];
  const writeDenied = useDenial('fleet.config.write');
  const action = (
    <button type="button" className="od-btn od-btn--sm od-btn--primary" disabled={creating || !!writeDenied} title={writeDenied} onClick={() => setCreating(true)}>
      <Icon name="plus" size={14} /><span>New config</span>
    </button>
  );
  return (
    <>
      <FleetHead actions={action} />
      <FleetGate configs>
        <div className="od-page" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          {creating && <NewConfig onCancel={() => setCreating(false)} />}
          {configs.error && <Problem title="Could not load configs" error={configs.error} retry={() => { pages.reset(); void configs.refetch(); }} />}
          {configs.isPending && <p role="status" className="od-muted">Loading configs…</p>}
          {configs.data && (
            <div className="od-card" style={{ overflow: 'hidden' }}>
              <table className="od-table">
                <thead><tr><th>Config</th><th>Description</th><th className="num">Latest version</th><th>Created</th><th>ID</th></tr></thead>
                <tbody>
                  {items.map((c) => (
                    <tr key={c.id}>
                      <td><Link to="/fleet/configs/$configId" params={{ configId: c.id }} className="mono">{c.name}</Link></td>
                      <td className="od-muted">{c.description || '—'}</td>
                      <td className="num">{c.latest_version ? `v${c.latest_version}` : <span className="od-subtle">none</span>}</td>
                      <td className="od-subtle">{relTime(c.created_at)} · {c.created_by}</td>
                      <td className="mono od-subtle" title={c.id}>{shortHex(c.id)}</td>
                    </tr>
                  ))}
                  {!items.length && (
                    <tr><td colSpan={5} className="od-subtle" style={{ textAlign: 'center', padding: 24 }}>No configs yet. A config keeps the versions of one configuration layer.</td></tr>
                  )}
                </tbody>
              </table>
              <Pager c={pages} next={configs.data.next_cursor} noun="configs" />
            </div>
          )}
        </div>
      </FleetGate>
    </>
  );
}

function NewConfig({ onCancel }: { onCancel: () => void }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [fix, setFix] = useState('');
  const create = useMutation({
    mutationFn: () => api.createFleetConfig({ name: name.trim(), description: description.trim() || undefined }),
    onSuccess: (c) => {
      void qc.invalidateQueries({ queryKey: ['fleet-configs'] });
      void navigate({ to: '/fleet/configs/$configId', params: { configId: c.id } });
    },
  });
  return (
    <form
      className="od-card"
      aria-label="New config"
      onSubmit={(e) => {
        e.preventDefault();
        if (!isFleetName(name.trim())) { setFix('Use lowercase letters, digits and dashes, starting with a letter or digit.'); return; }
        setFix('');
        create.mutate();
      }}
    >
      <div className="od-card__head"><h2 className="od-card__title" style={{ margin: 0 }}>New config</h2></div>
      <fieldset disabled={create.isPending} className="od-card__body" style={{ border: 0, margin: 0, display: 'flex', gap: 12, flexWrap: 'wrap', alignItems: 'flex-end' }}>
        <div className="od-field" style={{ width: 240 }}>
          <label htmlFor="config-name">Name</label>
          <input id="config-name" className="od-input mono" autoFocus placeholder="edge-batching" value={name} onChange={(e) => setName(e.target.value)} />
        </div>
        <div className="od-field" style={{ flex: 1, minWidth: 240 }}>
          <label htmlFor="config-description">Description</label>
          <input id="config-description" className="od-input" value={description} onChange={(e) => setDescription(e.target.value)} />
        </div>
        <button type="submit" className="od-btn od-btn--primary od-btn--sm">{create.isPending ? 'Creating…' : 'Create config'}</button>
        <button type="button" className="od-btn od-btn--sm" onClick={onCancel}>Cancel</button>
      </fieldset>
      {fix && <p role="alert" className="od-field__error" style={{ margin: '0 14px 12px' }}>{fix}</p>}
      {create.error && <div style={{ padding: '0 14px 12px' }}><Problem title="Could not create the config" error={create.error} /></div>}
    </form>
  );
}
