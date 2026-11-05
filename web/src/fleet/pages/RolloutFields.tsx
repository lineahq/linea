import { useQuery } from '@tanstack/react-query';
import { api } from '../../api/client';
import type { RolloutForm } from '../fleetView';

/** RolloutFields edits a rollout policy; empty fields keep fleet's defaults. */
export function RolloutFields({ form, onChange, idPrefix }: { form: RolloutForm; onChange: (f: RolloutForm) => void; idPrefix: string }) {
  const groups = useQuery({ queryKey: ['collector-groups', ''], queryFn: () => api.collectorGroups('') });
  const id = (s: string) => `${idPrefix}-${s}`;
  return (
    <fieldset style={{ border: 0, padding: 0, margin: 0, display: 'flex', gap: 12, flexWrap: 'wrap', alignItems: 'flex-start' }}>
      <legend className="od-section-label" style={{ marginBottom: 6 }}>Rollout</legend>
      <div className="od-field" style={{ width: 200 }}>
        <label htmlFor={id('waves')}>Waves</label>
        <input id={id('waves')} className="od-input mono" placeholder="1, 25%, 100%" value={form.waves} onChange={(e) => onChange({ ...form, waves: e.target.value })} />
        <span className="od-field__help">Agents or percent per wave.</span>
      </div>
      <div className="od-field" style={{ width: 200 }}>
        <label htmlFor={id('canary')}>Canary group</label>
        <select id={id('canary')} className="od-select" value={form.canary} onChange={(e) => onChange({ ...form, canary: e.target.value })}>
          <option value="">None</option>
          {(groups.data?.items ?? []).map((g) => <option key={g.id} value={g.id}>{g.name}</option>)}
        </select>
      </div>
      <div className="od-field" style={{ width: 120 }}>
        <label htmlFor={id('bake')}>Bake (s)</label>
        <input id={id('bake')} className="od-input" inputMode="numeric" placeholder="default" value={form.bake} onChange={(e) => onChange({ ...form, bake: e.target.value })} />
      </div>
      <div className="od-field" style={{ width: 140 }}>
        <label htmlFor={id('failures')}>Failure budget</label>
        <input id={id('failures')} className="od-input" placeholder="2 or 10%" value={form.maxFailures} onChange={(e) => onChange({ ...form, maxFailures: e.target.value })} />
      </div>
      <div className="od-field" style={{ width: 160 }}>
        <label htmlFor={id('on-failure')}>On failure</label>
        <select id={id('on-failure')} className="od-select" value={form.onFailure} onChange={(e) => onChange({ ...form, onFailure: e.target.value as RolloutForm['onFailure'] })}>
          <option value="">Fleet default</option>
          <option value="pause">Pause</option>
          <option value="rollback">Roll back</option>
        </select>
      </div>
    </fieldset>
  );
}
