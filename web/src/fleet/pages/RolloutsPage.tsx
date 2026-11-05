import { useState } from 'react';
import { Link, useParams } from '@tanstack/react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../api/client';
import type { FleetRollout } from '../../api/types';
import { InlineConfirm } from '../../capture/pages/InlineConfirm';
import { Icon } from '../../ui/Icon';
import { Crumbs } from '../../ui/PageHead';
import { ACTION_LABEL, isActive, relTime, rolloutActions, rolloutChip, rolloutMove, rolloutTally, shortHex, wavesText, type RolloutAction } from '../fleetView';
import { ChipView, FleetGate, FleetHead, Pager, Problem, useCursors } from './FleetHead';
import { useDenial } from '../../ui/session';

/** RolloutsPage lists fleet's rollouts in fleet's order. Outcomes come only with a rollout's own page. */
export function RolloutsPage() {
  const pages = useCursors();
  const rollouts = useQuery({ queryKey: ['fleet-rollouts', pages.cursor], queryFn: () => api.fleetRollouts(pages.cursor), refetchInterval: 5000, refetchIntervalInBackground: false });
  const items = rollouts.data?.items ?? [];
  return (
    <>
      <FleetHead />
      <FleetGate configs>
        <div className="od-page" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          {rollouts.error && <Problem title="Could not load rollouts" error={rollouts.error} retry={() => { pages.reset(); void rollouts.refetch(); }} />}
          {rollouts.isPending && <p role="status" className="od-muted">Loading rollouts…</p>}
          {rollouts.data && (
            <div className="od-card" style={{ overflow: 'hidden' }}>
              <table className="od-table">
                <thead><tr><th>Rollout</th><th>State</th><th>Change</th><th>Agents</th><th className="num">Wave</th><th>Updated</th></tr></thead>
                <tbody>
                  {items.map((r) => {
                    const t = rolloutTally(r);
                    return (
                      <tr key={r.id}>
                        <td><Link to="/fleet/rollouts/$rolloutId" params={{ rolloutId: r.id }} className="mono">{shortHex(r.id)}</Link></td>
                        <td><ChipView chip={rolloutChip(r)} /></td>
                        <td className="mono">{rolloutMove(r)}</td>
                        <td style={{ minWidth: 160 }}>
                          {t.known && <Progress done={t.done} />}
                          <span className="od-subtle" style={{ fontSize: 12 }}>
                            {t.known ? `${t.succeeded}/${t.targets} done${t.failed ? ` · ${t.failed} failed` : ''}` : `${t.targets} agent${t.targets === 1 ? '' : 's'}`}
                          </span>
                        </td>
                        <td className="num">{Math.min(r.current_wave + 1, r.wave_count)} / {r.wave_count}</td>
                        <td className="od-subtle">{relTime(r.updated_at)}</td>
                      </tr>
                    );
                  })}
                  {!items.length && <tr><td colSpan={6} className="od-subtle" style={{ textAlign: 'center', padding: 24 }}>No rollouts yet. Creating an assignment starts one.</td></tr>}
                </tbody>
              </table>
              <Pager c={pages} next={rollouts.data.next_cursor} noun="rollouts" />
            </div>
          )}
        </div>
      </FleetGate>
    </>
  );
}

function Progress({ done }: { done: number }) {
  return (
    <div className="od-progress" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={done} aria-label="Agents done">
      <span style={{ width: `${done}%` }} />
    </div>
  );
}

/** Fleet's target outcomes, as serve validates them; '' is any. */
const OUTCOMES = ['', 'pending', 'released', 'baking', 'offline', 'succeeded', 'failed', 'unhealthy', 'timed_out', 'unmatched', 'incompatible'];

/** RolloutDetailPage is one rollout: its waves, its targets and the controls its state allows. */
export function RolloutDetailPage() {
  const { rolloutId } = useParams({ strict: false }) as { rolloutId: string };
  const rollout = useQuery({
    queryKey: ['fleet-rollout', rolloutId],
    queryFn: () => api.fleetRollout(rolloutId),
    refetchInterval: (q) => (q.state.data && !isActive(q.state.data.state) ? false : 3000),
    refetchIntervalInBackground: false,
  });
  const r = rollout.data;
  return (
    <>
      <FleetHead />
      <FleetGate configs>
        <div className="od-page" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          <Crumbs items={[{ label: 'Rollouts', to: '/fleet/rollouts' }, { label: shortHex(rolloutId) }]} />
          {rollout.isPending && <p role="status" className="od-muted">Loading rollout…</p>}
          {rollout.error && <Problem title="Could not load this rollout" error={rollout.error} retry={() => void rollout.refetch()} />}
          {r && <RolloutView r={r} />}
        </div>
      </FleetGate>
    </>
  );
}

function RolloutView({ r }: { r: FleetRollout }) {
  const qc = useQueryClient();
  const [asking, setAsking] = useState<RolloutAction>();
  const rolloutDenied = useDenial('fleet.rollout');
  const control = useMutation({
    mutationFn: (action: RolloutAction) => api.controlFleetRollout(r.id, action),
    onSuccess: (next) => {
      qc.setQueryData(['fleet-rollout', r.id], next);
      void qc.invalidateQueries({ queryKey: ['fleet-rollouts'] });
      void qc.invalidateQueries({ queryKey: ['fleet-rollout-targets', r.id] });
      setAsking(undefined);
    },
  });
  const t = rolloutTally(r);
  const p = r.policy;
  return (
    <>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
        <h2 className="mono" style={{ margin: 0, fontSize: 18 }}>{rolloutMove(r)}</h2>
        <ChipView chip={rolloutChip(r)} />
        <span className="od-subtle" style={{ fontSize: 12 }}>started {relTime(r.created_at)} by {r.created_by}</span>
        <span style={{ flex: 1 }} />
        {rolloutActions(r.state).map((a) => (
          <button
            key={a}
            type="button"
            className={`od-btn od-btn--sm${a === 'resume' ? ' od-btn--primary' : a === 'pause' ? '' : ' od-btn--danger'}`}
            disabled={control.isPending || !!asking || !!rolloutDenied}
            title={rolloutDenied}
            onClick={() => (a === 'pause' || a === 'resume' ? control.mutate(a) : setAsking(a))}
          >
            {ACTION_LABEL[a]}
          </button>
        ))}
      </div>
      {asking && (
        <InlineConfirm danger confirmLabel={ACTION_LABEL[asking]} busy={control.isPending} onCancel={() => setAsking(undefined)} onConfirm={() => control.mutate(asking)}>
          {asking === 'rollback'
            ? 'Roll back? Fleet returns every released agent to the version it had before.'
            : 'Cancel? Fleet ends the rollout now and returns every released agent to the version it had before, without waiting for them to report it.'}
        </InlineConfirm>
      )}
      {control.error && <Problem title="Fleet refused the change" error={control.error} />}

      <div style={{ display: 'grid', gridTemplateColumns: 'minmax(0, 2fr) minmax(0, 1fr)', gap: 12 }}>
        <section className="od-card">
          <div className="od-card__head"><h3 className="od-card__title" style={{ margin: 0 }}>Waves</h3><span className="od-subtle" style={{ fontSize: 12 }}>
            {/* Fleet stops recording outcomes when a rollout ends, so nothing of a finished one is pending. */}
            {t.succeeded}/{t.targets} done · {t.failed} failed · {isActive(r.state) ? `${t.pending} pending` : 'outcomes when it ended'}
          </span></div>
          <div className="od-card__body" style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
            <Progress done={t.done} />
            {(r.waves ?? []).map((w) => (
              <div key={w.wave} style={{ display: 'flex', gap: 8, alignItems: 'center', fontSize: 12.5 }}>
                <Icon name={w.released_at ? (w.wave < r.current_wave || !isActive(r.state) ? 'circle-check' : 'loader-circle') : 'circle-dashed'} size={14} />
                <strong>Wave {w.wave + 1}</strong>
                <span className="od-subtle">{w.targets} agent{w.targets === 1 ? '' : 's'}</span>
                <span style={{ flex: 1 }} />
                {Object.entries(w.outcomes ?? {}).filter(([, n]) => n > 0).map(([k, n]) => <span key={k} className="od-subtle">{k.replaceAll('_', ' ')} {n}</span>)}
                <span className="od-subtle">{w.released_at ? `released ${relTime(w.released_at)}` : 'not released'}</span>
              </div>
            ))}
            {!r.waves?.length && <p className="od-muted" style={{ margin: 0 }}>No wave released yet.</p>}
          </div>
        </section>
        <section className="od-card">
          <div className="od-card__head"><h3 className="od-card__title" style={{ margin: 0 }}>Policy</h3></div>
          <dl className="od-kv od-card__body">
            <dt>Waves</dt><dd className="mono">{wavesText(p)}</dd>
            <dt>Canary group</dt><dd className="mono">{p.canary_group_id ? shortHex(p.canary_group_id) : 'none'}</dd>
            <dt>Bake</dt><dd>{p.bake_seconds !== undefined ? `${p.bake_seconds} s` : 'default'}</dd>
            <dt>Failure budget</dt><dd>{p.max_failures !== undefined ? p.max_failures : p.max_failure_percent !== undefined ? `${p.max_failure_percent}%` : 'default'}</dd>
            <dt>On failure</dt><dd>{p.on_failure || 'default'}</dd>
            <dt>Assignment</dt><dd className="mono">{shortHex(r.assignment_id)}</dd>
            <dt>Config</dt><dd><Link to="/fleet/configs/$configId" params={{ configId: r.config_id }} className="mono">{shortHex(r.config_id)}</Link></dd>
          </dl>
        </section>
      </div>
      <Targets id={r.id} live={isActive(r.state)} />
    </>
  );
}

function Targets({ id, live }: { id: string; live: boolean }) {
  const pages = useCursors();
  const [outcome, setOutcome] = useState('');
  const targets = useQuery({
    queryKey: ['fleet-rollout-targets', id, outcome, pages.cursor],
    queryFn: () => api.fleetRolloutTargets(id, outcome, pages.cursor),
    refetchInterval: live ? 5000 : false,
    refetchIntervalInBackground: false,
  });
  const items = targets.data?.items ?? [];
  return (
    <section className="od-card" style={{ overflow: 'hidden' }}>
      <div className="od-card__head" style={{ gap: 8 }}>
        <h3 className="od-card__title" style={{ margin: 0 }}>Targets</h3>
        <span style={{ flex: 1 }} />
        <select className="od-select" aria-label="Outcome filter" value={outcome} onChange={(e) => { pages.reset(); setOutcome(e.target.value); }}>
          {OUTCOMES.map((o) => <option key={o} value={o}>{o ? `Outcome: ${o.replaceAll('_', ' ')}` : 'Outcome: any'}</option>)}
        </select>
      </div>
      {targets.error && <Problem bar title="Could not load targets" error={targets.error} retry={() => void targets.refetch()} />}
      {targets.isPending && <p role="status" className="od-muted" style={{ padding: '8px 14px' }}>Loading targets…</p>}
      {targets.data && (
        <table className="od-table">
          <thead><tr><th>Agent</th><th className="num">Wave</th><th>Outcome</th><th>Accepted</th><th>Released</th><th>Observed</th></tr></thead>
          <tbody>
            {items.map((t) => (
              <tr key={t.agent_id}>
                <td className="mono">{t.agent_id}</td>
                <td className="num">{t.wave + 1}</td>
                <td>{t.outcome.replaceAll('_', ' ')}</td>
                <td>{t.accepted ? 'Yes' : 'No'}</td>
                <td className="od-subtle">{relTime(t.released_at)}</td>
                <td className="od-subtle">{relTime(t.observed_at)}</td>
              </tr>
            ))}
            {!items.length && <tr><td colSpan={6} className="od-subtle" style={{ textAlign: 'center', padding: 24 }}>No target {outcome ? `with outcome ${outcome.replaceAll('_', ' ')}` : 'yet'}.</td></tr>}
          </tbody>
        </table>
      )}
      <Pager c={pages} next={targets.data?.next_cursor} noun="targets" />
    </section>
  );
}
