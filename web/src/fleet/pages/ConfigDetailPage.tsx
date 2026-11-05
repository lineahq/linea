import { useEffect, useState } from 'react';
import { Link, useNavigate, useParams } from '@tanstack/react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../api/client';
import type { FleetConfig } from '../../api/types';
import { InlineConfirm } from '../../capture/pages/InlineConfirm';
import { YamlEditor } from '../../editor/YamlEditor';
import { UnifiedDiff } from '../../inspect/DiffViews';
import { Icon } from '../../ui/Icon';
import { Crumbs } from '../../ui/PageHead';
import { isFleetName, relTime } from '../fleetView';
import { FleetGate, FleetHead, Pager, Problem, useCursors } from './FleetHead';
import { useDenial } from '../../ui/session';

/**
 * ConfigDetailPage is one managed config: its versions, each
 * version's YAML and its change from the version before, a new version,
 * renaming and deleting. Versions are immutable; assigning one starts a
 * rollout on Assignments.
 */
export function ConfigDetailPage() {
  const { configId } = useParams({ strict: false }) as { configId: string };
  const config = useQuery({ queryKey: ['fleet-config', configId], queryFn: () => api.fleetConfig(configId), refetchInterval: 10000, refetchIntervalInBackground: false });
  const [version, setVersion] = useState<number>();
  const [drafting, setDrafting] = useState(false);
  const c = config.data;
  useEffect(() => {
    if (version === undefined && c?.latest_version) setVersion(c.latest_version);
  }, [c, version]);

  return (
    <>
      <FleetHead />
      <FleetGate configs>
        <div className="od-page" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          <Crumbs items={[{ label: 'Configs', to: '/fleet/configs' }, { label: c?.name ?? configId }]} />
          {config.isPending && <p role="status" className="od-muted">Loading config…</p>}
          {config.error && <Problem title="Could not load this config" error={config.error} retry={() => void config.refetch()} />}
          {c && (
            <>
              <ConfigSummary c={c} onNewVersion={() => setDrafting(true)} drafting={drafting} />
              {drafting && <VersionDraft c={c} base={version} onDone={(v) => { setDrafting(false); if (v) setVersion(v); }} />}
              {!c.latest_version && !drafting && (
                <div className="od-card"><div className="od-empty"><div className="od-empty__title">No versions yet</div><div className="od-empty__body">Write the first version of this layer. Assign it on Assignments to roll it out.</div></div></div>
              )}
              {c.latest_version > 0 && (
                <div style={{ display: 'flex', gap: 12, minHeight: 0, alignItems: 'flex-start' }}>
                  <Versions configId={c.id} selected={version} onSelect={setVersion} />
                  {version !== undefined && <VersionView configId={c.id} version={version} />}
                </div>
              )}
            </>
          )}
        </div>
      </FleetGate>
    </>
  );
}

function ConfigSummary({ c, onNewVersion, drafting }: { c: FleetConfig; onNewVersion: () => void; drafting: boolean }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(c.name);
  const [description, setDescription] = useState(c.description);
  const [confirming, setConfirming] = useState(false);
  const [fix, setFix] = useState('');
  const writeDenied = useDenial('fleet.config.write');
  const update = useMutation({
    mutationFn: () => api.updateFleetConfig(c.id, { revision: c.revision, name: name.trim() !== c.name ? name.trim() : undefined, description: description.trim() !== c.description ? description.trim() : undefined }),
    onSuccess: (next) => {
      qc.setQueryData(['fleet-config', c.id], next);
      void qc.invalidateQueries({ queryKey: ['fleet-configs'] });
      setEditing(false);
    },
  });
  const remove = useMutation({
    mutationFn: () => api.deleteFleetConfig(c.id, c.revision),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['fleet-configs'] });
      void navigate({ to: '/fleet/configs' });
    },
  });

  if (editing) {
    return (
      <form
        className="od-card"
        aria-label="Edit config"
        onSubmit={(e) => {
          e.preventDefault();
          if (!isFleetName(name.trim())) { setFix('Use lowercase letters, digits and dashes, starting with a letter or digit.'); return; }
          setFix('');
          update.mutate();
        }}
      >
        <fieldset disabled={update.isPending} className="od-card__body" style={{ border: 0, margin: 0, display: 'flex', gap: 12, flexWrap: 'wrap', alignItems: 'flex-end' }}>
          <div className="od-field" style={{ width: 240 }}>
            <label htmlFor="config-name">Name</label>
            <input id="config-name" className="od-input mono" autoFocus value={name} onChange={(e) => setName(e.target.value)} />
          </div>
          <div className="od-field" style={{ flex: 1, minWidth: 240 }}>
            <label htmlFor="config-description">Description</label>
            <input id="config-description" className="od-input" value={description} onChange={(e) => setDescription(e.target.value)} />
          </div>
          <button type="submit" className="od-btn od-btn--primary od-btn--sm">{update.isPending ? 'Saving…' : 'Save'}</button>
          <button type="button" className="od-btn od-btn--sm" onClick={() => { setEditing(false); setName(c.name); setDescription(c.description); }}>Cancel</button>
        </fieldset>
        {fix && <p role="alert" className="od-field__error" style={{ margin: '0 14px 12px' }}>{fix}</p>}
        {update.error && <div style={{ padding: '0 14px 12px' }}><Problem title="Could not save" error={update.error} /></div>}
      </form>
    );
  }
  return (
    <>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
        <h2 className="mono" style={{ margin: 0, fontSize: 18 }}>{c.name}</h2>
        <span className="od-subtle" style={{ fontSize: 12 }}>{c.description || 'No description'} · revision {c.revision} · created {relTime(c.created_at)} by {c.created_by}</span>
        <span style={{ flex: 1 }} />
        <button type="button" className="od-btn od-btn--sm" disabled={!!writeDenied} title={writeDenied} onClick={() => setEditing(true)}><Icon name="pencil" size={14} /><span>Rename</span></button>
        <button type="button" className="od-btn od-btn--sm od-btn--danger" disabled={confirming || !!writeDenied} title={writeDenied} onClick={() => setConfirming(true)}><Icon name="trash-2" size={14} /><span>Delete config</span></button>
        <Link to="/fleet/bisect" search={{ config: c.id }} className="od-btn od-btn--sm"><span>Bisect versions</span></Link>
        <button type="button" className="od-btn od-btn--sm od-btn--primary" disabled={drafting || !!writeDenied} title={writeDenied} onClick={onNewVersion}><Icon name="plus" size={14} /><span>New version</span></button>
      </div>
      {confirming && (
        <InlineConfirm danger confirmLabel="Delete config" busy={remove.isPending} onCancel={() => setConfirming(false)} onConfirm={() => remove.mutate()}>
          Delete <strong>{c.name}</strong> and all {c.latest_version} versions? Fleet refuses while an assignment uses it.
        </InlineConfirm>
      )}
      {remove.error && <Problem title="Could not delete the config" error={remove.error} />}
    </>
  );
}

function Versions({ configId, selected, onSelect }: { configId: string; selected?: number; onSelect: (v: number) => void }) {
  const pages = useCursors();
  const versions = useQuery({ queryKey: ['fleet-config-versions', configId, pages.cursor], queryFn: () => api.fleetConfigVersions(configId, pages.cursor), refetchInterval: 10000, refetchIntervalInBackground: false });
  return (
    <nav className="od-card" aria-label="Config versions" style={{ width: 280, flex: 'none', overflow: 'hidden' }}>
      <div className="od-card__head"><h3 className="od-card__title" style={{ margin: 0 }}>Versions</h3></div>
      {versions.isPending && <p role="status" className="od-muted" style={{ padding: '8px 14px' }}>Loading versions…</p>}
      {versions.error && <Problem bar title="Could not load versions" error={versions.error} retry={() => void versions.refetch()} />}
      <div style={{ display: 'flex', flexDirection: 'column' }}>
        {(versions.data?.items ?? []).map((v) => (
          <button
            key={v.version}
            type="button"
            aria-current={selected === v.version ? 'true' : undefined}
            onClick={() => onSelect(v.version)}
            style={{ textAlign: 'left', padding: '8px 14px', border: 0, borderTop: '1px solid var(--border)', font: 'inherit', color: 'inherit', cursor: 'pointer', background: selected === v.version ? 'var(--accent-soft, var(--surface))' : 'transparent' }}
          >
            <strong>v{v.version}</strong> <span className="od-subtle" style={{ fontSize: 12 }}>{relTime(v.created_at)}{v.created_by ? ` · ${v.created_by}` : ''}</span>
            {v.message && <div className="od-muted" style={{ fontSize: 12 }}>{v.message}</div>}
          </button>
        ))}
      </div>
      <Pager c={pages} next={versions.data?.next_cursor} noun="versions" />
    </nav>
  );
}

function VersionView({ configId, version }: { configId: string; version: number }) {
  const [tab, setTab] = useState<'content' | 'changes'>('content');
  const current = useQuery({ queryKey: ['fleet-config-version', configId, version], queryFn: () => api.fleetConfigVersion(configId, version), staleTime: Infinity });
  const previous = useQuery({ queryKey: ['fleet-config-version', configId, version - 1], queryFn: () => api.fleetConfigVersion(configId, version - 1), staleTime: Infinity, enabled: version > 1 && tab === 'changes' });
  const v = current.data;
  return (
    <section className="od-card" style={{ flex: 1, minWidth: 0, overflow: 'hidden' }} aria-label={`Version ${version}`}>
      <div className="od-card__head" style={{ gap: 8 }}>
        <h3 className="od-card__title" style={{ margin: 0 }}>v{version}</h3>
        {v && <span className="od-subtle mono" style={{ fontSize: 11.5 }} title={v.sha256}>{v.size} bytes · sha256 {v.sha256.slice(0, 12)}</span>}
        <span style={{ flex: 1 }} />
        <div className="od-seg" role="group" aria-label="Version view">
          <button type="button" aria-pressed={tab === 'content'} onClick={() => setTab('content')}>Content</button>
          <button type="button" aria-pressed={tab === 'changes'} disabled={version <= 1} onClick={() => setTab('changes')}>Changes from v{version - 1 || '—'}</button>
        </div>
        <Link to="/fleet/assignments" search={{ config: configId, version }} className="od-btn od-btn--sm"><Icon name="rocket" size={14} /><span>Assign this version</span></Link>
      </div>
      {current.isPending && <p role="status" className="od-muted" style={{ padding: '8px 14px' }}>Loading version…</p>}
      {current.error && <Problem bar title="Could not load this version" error={current.error} retry={() => void current.refetch()} />}
      {v && tab === 'content' && <div style={{ height: 420 }}><YamlEditor value={v.body ?? ''} onChange={() => undefined} path={`fleet-config/${configId}/v${version}.yaml`} readOnly /></div>}
      {v && tab === 'changes' && (
        <div style={{ padding: 12, overflow: 'auto', maxHeight: 520 }}>
          {previous.isPending && <p role="status" className="od-muted">Loading v{version - 1}…</p>}
          {previous.error && <Problem title={`Could not load v${version - 1}`} error={previous.error} />}
          {previous.data && <UnifiedDiff before={previous.data.body ?? ''} after={v.body ?? ''} />}
        </div>
      )}
    </section>
  );
}

/** VersionDraft writes a new version, starting from the version in view. */
function VersionDraft({ c, base, onDone }: { c: FleetConfig; base?: number; onDone: (version?: number) => void }) {
  const qc = useQueryClient();
  const seed = useQuery({ queryKey: ['fleet-config-version', c.id, base], queryFn: () => api.fleetConfigVersion(c.id, base ?? 0), staleTime: Infinity, enabled: base !== undefined && base > 0 });
  const [body, setBody] = useState<string>();
  const [message, setMessage] = useState('');
  const [discarding, setDiscarding] = useState(false);
  // Seed the draft once; a later change of the version in view never replaces it.
  useEffect(() => {
    if (body === undefined && (base === undefined || base === 0 || seed.data)) setBody(seed.data?.body ?? '');
  }, [body, base, seed.data]);
  const dirty = body !== undefined && body !== (seed.data?.body ?? '');
  const create = useMutation({
    mutationFn: () => api.createFleetConfigVersion(c.id, { body: body ?? '', message: message.trim() || undefined }),
    onSuccess: (v) => {
      void qc.invalidateQueries({ queryKey: ['fleet-config', c.id] });
      void qc.invalidateQueries({ queryKey: ['fleet-config-versions', c.id] });
      void qc.invalidateQueries({ queryKey: ['fleet-configs'] });
      onDone(v.version);
    },
  });
  return (
    <section className="od-card" aria-label="New version" style={{ overflow: 'hidden' }}>
      <div className="od-card__head" style={{ gap: 8 }}>
        <h3 className="od-card__title" style={{ margin: 0 }}>New version · v{c.latest_version + 1}</h3>
        {base ? <span className="od-subtle" style={{ fontSize: 12 }}>starts from v{base}</span> : null}
      </div>
      {seed.error && <Problem bar title={`Could not load v${base}`} error={seed.error} />}
      {body === undefined ? <p role="status" className="od-muted" style={{ padding: '8px 14px' }}>Loading v{base}…</p> : (
        <div style={{ height: 360 }}><YamlEditor value={body} onChange={setBody} path={`fleet-config/${c.id}/draft.yaml`} /></div>
      )}
      <form
        style={{ display: 'flex', gap: 8, alignItems: 'flex-end', padding: 12, flexWrap: 'wrap' }}
        onSubmit={(e) => { e.preventDefault(); if (body?.trim()) create.mutate(); }}
      >
        <div className="od-field" style={{ flex: 1, minWidth: 240 }}>
          <label htmlFor="version-message">What changed</label>
          <input id="version-message" className="od-input" value={message} onChange={(e) => setMessage(e.target.value)} />
        </div>
        <button type="submit" className="od-btn od-btn--primary od-btn--sm" disabled={create.isPending || !body?.trim()}>{create.isPending ? 'Saving…' : 'Save version'}</button>
        <button type="button" className="od-btn od-btn--sm" disabled={create.isPending} onClick={() => (dirty ? setDiscarding(true) : onDone())}>Cancel</button>
      </form>
      <p className="od-subtle" style={{ margin: '0 12px 12px', fontSize: 12 }}>Saving keeps the version; nothing changes on agents until it is assigned and rolled out.</p>
      {discarding && (
        <div style={{ padding: '0 12px 12px' }}>
          <InlineConfirm danger confirmLabel="Discard draft" cancelLabel="Keep editing" onCancel={() => setDiscarding(false)} onConfirm={() => onDone()}>
            Discard the unsaved version?
          </InlineConfirm>
        </div>
      )}
      {create.error && <div style={{ padding: '0 12px 12px' }}><Problem title="Could not save the version" error={create.error} /></div>}
    </section>
  );
}
